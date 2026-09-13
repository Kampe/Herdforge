#!/usr/bin/env zsh
# FAC-829: non-vacuity driver for the resource observer guards.
#
# Runs the observer suites as baselines, then mutates the REAL production source
# one guard at a time. A mutant counts as KILLED only when ALL of these hold, as
# separate evidence:
#
#   1. it COMPILES, proven by building the test binary,
#   2. the run exits NON-ZERO,
#   3. the NAMED killer test (or subtest) emits a `fail` action, and
#   4. its output carries the NAMED assertion text,
#
# all read from `go test -json` rather than scraped from console output. A
# compile error, a panic, a Go-internal test timeout, an external timeout, a
# skip, or an unrelated assertion is NOT a kill, and each is reported under its
# own verdict so a vacuous control cannot be counted as evidence.
#
# Every control lives in pkg/resources and runs on a fake clock with injected
# probes, so no verdict depends on the runner's own load. Nothing here starts an
# observer against the host.
#
# Mutation happens in one ephemeral detached worktree this invocation creates
# and owns, placed OUTSIDE the evidence directory so a failed cleanup can never
# be uploaded as an artifact. The invoking checkout is never written to, and
# this script deletes nothing it did not create.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"

# Every external command this driver actually runs. python3 does the literal
# counting and patching, so its absence must fail here rather than midway
# through a mutation.
for tool in git go timeout jq mktemp python3 rmdir; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

observer_src=pkg/resources/observer.go
cli_src=cmd/herd/main.go
run_src=pkg/resources/observer_run.go
paths_src=pkg/resources/observer_paths.go
resources_pkg=./pkg/resources/
herd_pkg=./cmd/herd/

# The baseline runs the WHOLE focus set; a mutant runs only its anchored killer,
# so an unrelated failure elsewhere can never be reported as this control's kill.
observer_run='TestObserverDropsOverrunTicksWithoutCatchUp|TestObserverRefusesPublishBeforeObservation|TestObserverStampsPublishTimeAtWriteTime|TestObserverHistoryStaysBounded|TestObserverHistoryNeverRestampsEarlierSamples|TestObserverConfigRefusesBusyLoopBounds|TestObserverConfigAcceptsDefaults|TestObserverUsableFailsClosed|TestObserverUsableAcceptsBothHealthyPlatformShapes|TestObserverUsableRefusesContradictoryReports|TestObserverUsableRefusesBrokenChronology|TestObserverRecordsSampleFailureWithoutAdmitting|TestObserverStopsAtLifetime|TestObserverStopsOnCancellationAndPublishesTermination|TestObserverLockScopeIsReportedHonestly|TestObserverLockIsDistinctFromCapacityAndReaperLocks|TestObserverPathsHaveNoCallerOverride|TestObserverStatusPathCannotBeTheGuardReport|TestReadObserverStatusRefusesAnOversizedFile|TestReadObserverStatusRefusesAnUnknownSchema|TestObserverStatusRoundTripsAtomically|TestRunObserverRefusesWhenTheLockIsHeld|TestRunObserverRefusalDoesNotTouchTheStatusFile|TestRunObserverPreservesPublishFailureThroughCancellation|TestRunObserverCleanCancellationStaysSuccessful|TestNormalizeObserverExitKeepsOtherCauses'

# Exit 0 alone is not a baseline: a selector that matched nothing also exits 0.
# Every one of these must be seen PASSING at top level.
# The CLI contract lives in cmd/herd, so it gets its own baseline: a mutant
# that removes mode validation must be measured against a suite that was
# passing beforehand in THAT package too.
herd_run='TestRunResourcesRejectsFlagsTheModeCannotHonour|TestRunResourcesAcceptsTheStatusModeAndItsJSON|TestValidateResourcesModeAcceptsEverySupportedCombination|TestObserverStatusCommandFailsClosed|TestObserverWatchRefusesBadBoundsBeforeSampling'
herd_expect='TestRunResourcesRejectsFlagsTheModeCannotHonour TestRunResourcesAcceptsTheStatusModeAndItsJSON TestValidateResourcesModeAcceptsEverySupportedCombination TestObserverStatusCommandFailsClosed TestObserverWatchRefusesBadBoundsBeforeSampling'

observer_expect='TestObserverDropsOverrunTicksWithoutCatchUp TestObserverRefusesPublishBeforeObservation TestObserverStampsPublishTimeAtWriteTime TestObserverHistoryStaysBounded TestObserverConfigRefusesBusyLoopBounds TestObserverUsableAcceptsBothHealthyPlatformShapes TestObserverUsableRefusesContradictoryReports TestObserverUsableRefusesBrokenChronology TestObserverRecordsSampleFailureWithoutAdmitting TestObserverLockScopeIsReportedHonestly TestReadObserverStatusRefusesAnOversizedFile TestRunObserverPreservesPublishFailureThroughCancellation TestRunObserverCleanCancellationStaysSuccessful TestNormalizeObserverExitKeepsOtherCauses'

# Finite and bounded at both ends BEFORE any arithmetic: an absurd or
# overflowing override must be rejected, never added to.
go_timeout=${VERIFY_OBSERVER_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 4 )) || (( go_timeout < 60 || go_timeout > 1800 )); then
	print -u2 "warning: ignoring unusable VERIFY_OBSERVER_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
worktree_timeout=120
cleanup_timeout=60

# Evidence is additive. This script creates ONE subdirectory it owns and never
# deletes a directory chosen by a caller: a path being well formed is not
# permission to destroy what is in it.
report_parent=${VERIFY_OBSERVER_REPORT_DIR:-$repo_root/.verify-observer-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
summary=$run_dir/summary.txt
: >| "$summary"

# The ephemeral worktree lives OUTSIDE the evidence directory. A checkout under
# the uploaded path would mean a failed cleanup ships an entire source tree as a
# CI artifact.
work_parent=$(mktemp -d)
work=""
work_owned=0
work_attempted=0
cleanup_done=0
cleanup_rc=0

note() {
	print -r -- "$@" | tee -a "$summary"
}

# cleanup_worktree is idempotent and returns non-zero when it could not finish.
# It is called explicitly before the verdict, so a leaked worktree cannot be
# discovered after the exit status has already been decided.
cleanup_worktree() {
	if (( cleanup_done )); then
		return $cleanup_rc
	fi
	cleanup_done=1
	# Only the checkout THIS invocation created, and only once creation
	# succeeded. No blind fallback deletion, and no global worktree prune:
	# other worktrees and their metadata are not ours to touch.
	if (( work_owned )) && [[ -n "$work" ]]; then
		if timeout -k 10s "${cleanup_timeout}s" \
			git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1; then
			work_owned=0
		else
			print -u2 "warning: could not remove the ephemeral worktree; it is left in place deliberately"
			cleanup_rc=1
		fi
	elif (( work_attempted )); then
		print -u2 "warning: a worktree path was reserved but never created; nothing was removed"
	fi
	# Remove only the empty parent this run made. rmdir refuses a non-empty
	# directory, which is exactly the safety wanted here.
	if (( cleanup_rc == 0 )) && [[ -n "$work_parent" && -d "$work_parent" ]]; then
		rmdir -- "$work_parent" 2>/dev/null || true
	fi
	return $cleanup_rc
}

on_exit() {
	cleanup_worktree || true
}

# A signal terminates the driver with a failure status. It does not fall through
# into the rest of the run, and the EXIT trap is cleared first so cleanup cannot
# be attempted twice.
on_signal() {
	trap - EXIT INT TERM HUP
	cleanup_worktree || true
	print -u2 "verify-resource-observer: terminated by signal"
	exit 143
}

trap on_exit EXIT
trap on_signal INT TERM HUP

pin=$(git -C "$repo_root" rev-parse HEAD)
work=$work_parent/checkout
work_attempted=1
if ! timeout -k 10s "${worktree_timeout}s" \
	git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null 2>&1; then
	print -u2 "harness error: could not create the ephemeral worktree within ${worktree_timeout}s"
	exit 1
fi
work_owned=1

# events_valid proves the WHOLE stream parses before any predicate reads it. A
# truncated or interleaved stream must not be interrogated as if it were data.
events_valid() {
	local events=$1
	[[ -s "$events" ]] || return 1
	jq -e -s 'length > 0' "$events" >/dev/null 2>&1
}

# jq_predicate runs a COMPLETE jq expression. It is never piped into grep:
# under `set -o pipefail` a short-circuiting reader can SIGPIPE the writer and
# invert the result exactly when the predicate matched.
jq_predicate() {
	local events=$1
	local filter=$2
	jq -e -s "$filter" "$events" >/dev/null 2>&1
}

# test_emitted_exact binds to the test ITSELF, never to a subtest: a baseline
# that passed only a subtest is not a passing test.
test_emitted_exact() {
	local events=$1
	local name=$2
	local action=$3
	jq_predicate "$events" \
		"any(.[]; .Action == \"$action\" and .Test == \"$name\")"
}

baseline_ok() {
	local events=$1
	local expected=$2
	local name
	events_valid "$events" || return 1
	for name in ${=expected}; do
		test_emitted_exact "$events" "$name" "pass" || return 1
	done
	return 0
}

# run_crashed reports a run that ended in a way no assertion can speak for: a
# panic, a Go-internal test timeout, or a build failure. Any of these can print
# the intended assertion text on the way down, so they are excluded BEFORE the
# kill predicate is consulted.
run_crashed() {
	local events=$1
	jq_predicate "$events" \
		'any(.[]; .Action == "output" and (.Output | test("^panic: |^fatal error: |test timed out after |\\[build failed\\]|^# ")))'
}

# killed_by requires a genuine, attributable failure: a non-zero exit, a `fail`
# action on the NAMED test, and the NAMED assertion in that test's own output.
# Any one of the three alone is not a kill -- a test can fail for an unrelated
# reason, and the text can appear in the log of a run that passed.
killed_by() {
	local events=$1
	local name=$2
	local assertion=$3
	local exit_code=$4
	(( exit_code != 0 )) || return 1
	events_valid "$events" || return 1
	run_crashed "$events" && return 1
	jq_predicate "$events" \
		"any(.[]; .Action == \"fail\" and .Test == \"$name\")" || return 1
	jq_predicate "$events" \
		"any(.[]; .Action == \"output\" and (.Test // \"\") == \"$name\" and (.Output | contains(\"$assertion\")))"
}

run_focused() {
	local pkg=$1
	local selector=$2
	local events=$3
	local console=$4
	local rc=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$pkg" ) >"$events" 2>"$console" || rc=$?
	print -r -- "$rc"
}

# compile_check proves the mutant BUILDS, separately from whether it fails a
# test. A mutant that does not compile is a broken control, never a kill.
compile_check() {
	local pkg=$1
	local console=$2
	local rc=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -c -o /dev/null -p 1 "$pkg" ) >"$console" 2>&1 || rc=$?
	print -r -- "$rc"
}

# count_occurrences walks the literal. `grep -c` counts LINES, so two hits on
# one line would read as one and a uniqueness check would pass wrongly.
count_occurrences() {
	local file=$1
	local needle=$2
	python3 - "$file" "$needle" <<'PYEOF'
import sys
with open(sys.argv[1], 'r', encoding='utf-8') as handle:
    body = handle.read()
print(body.count(sys.argv[2]))
PYEOF
}

typeset -A pristine
typeset -a mutated_sources

# NOTE: `path` is a zsh special tied to PATH. A local named `path` here would
# replace command lookup with a filename and break every external call that
# follows, so source paths are always `source_path`.
patch_source() {
	local src=$1
	local from=$2
	local to=$3
	local source_path="$work/$src"
	local hits
	if [[ -z "${pristine[$src]:-}" ]]; then
		print -u2 "harness error: no pristine hash for $src; refusing to patch an unknown baseline"
		exit 1
	fi
	hits=$(count_occurrences "$source_path" "$from")
	if [[ "$hits" != "1" ]]; then
		print -u2 "harness error: anchor appears $hits time(s) in $src; a control must patch exactly one site"
		exit 1
	fi
	python3 - "$source_path" "$from" "$to" <<'PYEOF'
import sys
target, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
with open(target, 'r', encoding='utf-8') as handle:
    body = handle.read()
with open(target, 'w', encoding='utf-8') as handle:
    handle.write(body.replace(old, new, 1))
PYEOF
}

restore_source() {
	local src=$1
	local now
	if [[ -z "${pristine[$src]:-}" ]]; then
		print -u2 "harness error: no pristine hash for $src; refusing to restore against an unknown baseline"
		exit 1
	fi
	git -C "$work" checkout -- "$src"
	now=$(git -C "$work" hash-object -- "$src")
	if [[ "$now" != "${pristine[$src]}" ]]; then
		print -u2 "harness error: $src did not restore to its pristine content"
		exit 1
	fi
}

restore_all() {
	local src
	for src in "${mutated_sources[@]}"; do
		restore_source "$src"
	done
}

# Each record: name | source | from | to | package | killer test | assertion text.
#
# Every replacement keeps the original expression syntactically USED -- `false &&`
# rather than a bare `false` -- because a mutant that leaves a variable unused
# does not compile, and a control that cannot build proves nothing.
#
# Two rows carry a correction worth recording, because both were wrong in ways
# that looked right:
#
#   pressure-enum-inconsistency-ignored SURVIVED against the subtest that sets
#   PressureKnown=false on a Darwin-shaped report. That report gates on nothing,
#   so the later neither-signal guard refuses it whether or not the enum/boolean
#   check exists -- the control was measuring a redundant path. The row now
#   targets the OTHER direction: a report claiming pressure IS known while
#   naming an unknown level, gating on healthy headroom, which the neither-signal
#   guard does not reach and the numbers otherwise admit. That is the case only
#   this check catches, and its healthy counterpart is the linux shape in
#   TestObserverUsableAcceptsBothHealthyPlatformShapes, identical except that it
#   honestly reports pressure as not known.
#
#   mode-validation-discarded defends the CLI contract itself. Every observer
#   bound was parsed and then dropped outside --watch, and --gate and --selftest
#   under --watch were skipped by an early return, so a resource guard the
#   operator asked for silently did not run and the command still exited 0. The
#   mutant feeds the validator an empty flag set, which compiles and restores
#   exactly that behaviour, and the named CLI oracle catches it.
#
#   catch-up-burst is anchored to the assertion its oracle ACTUALLY emits.
#   Replaying every missed tick makes each gap exactly one, so the test's
#   zero-skipped-ticks assertion fires before its inter-sample spacing check.
#   Both claims belong to the same contract and both are violated; the control
#   names the one the oracle reaches first rather than accepting any failure.
#
# One guard is deliberately NOT mutated here. reportSelfConsistent refuses a
# missing decided_at/rendered_at with a named message, but an absent stamp also
# fails the RFC3339 parse immediately after, so removing the explicit check
# still refuses and no compiling mutation can isolate it. The requirement is
# implemented and covered by the decided_at_missing and rendered_at_missing
# subtests; it is recorded here as unmutatable rather than given a control that
# would pass for the wrong reason.
sep=$'\x1f'
mutations=(
"publishes-before-observation${sep}${observer_src}${sep}		if at.Before(completed) {${sep}		if false && at.Before(completed) { // MUTANT: a publish stamp may predate its observation${sep}${resources_pkg}${sep}TestObserverRefusesPublishBeforeObservation${sep}the copied-stale-field defect is not guarded"
"expiry-ignores-metric-window${sep}${observer_src}${sep}	if window.Before(cadence) {${sep}	if false && window.Before(cadence) { // MUTANT: expiry no longer bounded by the metric window${sep}${resources_pkg}${sep}TestObserverStampsPublishTimeAtWriteTime${sep}outlives the metric window"
"history-unbounded${sep}${observer_src}${sep}	if len(history) < ObserverHistoryMax {${sep}	if true || len(history) < ObserverHistoryMax { // MUTANT: the ring stops being a ring${sep}${resources_pkg}${sep}TestObserverHistoryStaysBounded${sep}history grew to"
"usable-trusts-published-booleans${sep}${observer_src}${sep}	if !again.Admits() {${sep}	if false && !again.Admits() { // MUTANT: the re-decision no longer overrules the booleans${sep}${resources_pkg}${sep}TestObserverUsableRefusesContradictoryReports/saturated_cpu_with_admit_booleans${sep}published booleans must not outrank published numbers"
"pressure-enum-inconsistency-ignored${sep}${observer_src}${sep}	if level.Known() != r.PressureKnown {${sep}	if false && level.Known() != r.PressureKnown { // MUTANT: the enum and its boolean may disagree${sep}${resources_pkg}${sep}TestObserverUsableRefusesContradictoryReports/pressure_known_contradicts_an_unknown_level${sep}published booleans must not outrank published numbers"
"config-accepts-busy-loop${sep}${observer_src}${sep}	if c.Interval < MinObserverInterval || c.Interval > MaxObserverInterval {${sep}	if false && (c.Interval < MinObserverInterval || c.Interval > MaxObserverInterval) { // MUTANT: any interval accepted${sep}${resources_pkg}${sep}TestObserverConfigRefusesBusyLoopBounds/interval_above_ceiling_only${sep}must be refused, not repaired"
"read-bound-removed${sep}${observer_src}${sep}		return status, fmt.Errorf(\"observer: status exceeds the %d-byte bound; refusing to parse it\",${sep}		_ = fmt.Sprintf(\"%d\", // MUTANT: an oversized status is parsed anyway${sep}${resources_pkg}${sep}TestReadObserverStatusRefusesAnOversizedFile${sep}the size bound is not enforced"
"catch-up-burst${sep}${observer_src}${sep}		next := int64(elapsed/cfg.Interval) + 1${sep}		next := tickIndex + 1 // MUTANT: replay every missed tick${sep}${resources_pkg}${sep}TestObserverDropsOverrunTicksWithoutCatchUp${sep}an overrunning sample produced zero skipped ticks; drops are being hidden"
"failed-sample-keeps-admit${sep}${observer_src}${sep}			sample.Report.Admits = false${sep}			_ = sampleErr // MUTANT: a failed sample keeps its admit${sep}${resources_pkg}${sep}TestObserverRecordsSampleFailureWithoutAdmitting${sep}a failure must not keep its admit"
"broken-chronology-ignored${sep}${observer_src}${sep}	if why := sampleChronologyProblem(status, at); why != \"\" {${sep}	if why := \"\"; why != \"\" { // MUTANT: stamps no longer have to be credible${sep}${resources_pkg}${sep}TestObserverUsableRefusesBrokenChronology/missing_published_at${sep}was reported usable"
"publish-failure-swallowed-by-cancellation${sep}${run_src}${sep}	if errors.Is(err, ErrObserverPublishFailed) {${sep}	if false && errors.Is(err, ErrObserverPublishFailed) { // MUTANT: a cancellation hides a failed final write${sep}${resources_pkg}${sep}TestRunObserverPreservesPublishFailureThroughCancellation${sep}a failed terminal publication was reported as a clean shutdown"
"mode-validation-discarded${sep}${cli_src}${sep}	mode, err := validateResourcesMode(provided)${sep}	mode, err := validateResourcesMode(map[string]bool{}) // MUTANT: the mode validator never sees the operator's flags${sep}${herd_pkg}${sep}TestRunResourcesRejectsFlagsTheModeCannotHonour${sep}an unsupported mix must be refused, never ignored"
"unknown-scope-sounds-safe${sep}${paths_src}${sep}	return \"unknown scope: treat exclusivity as unproven\"${sep}	return \"exclusive\" // MUTANT: an unrecognised scope claims exclusivity${sep}${resources_pkg}${sep}TestObserverLockScopeIsReportedHonestly/an_unrecognised_scope_refuses_to_sound_safe${sep}it must state that exclusivity is unproven"
)

# Snapshot EVERY source a control declares, exactly once, before anything is
# mutated. Deriving the inventory from the table is the point: a control can no
# longer name a file the snapshot forgot. A declared source that is missing or
# unhashable is a HARNESS error and stops the run; it is not a failed control
# and must not be silently skipped.
for record in "${mutations[@]}"; do
	fields=("${(@ps:$sep:)record}")
	src=$fields[2]
	if [[ -n "${pristine[$src]:-}" ]]; then
		continue
	fi
	if [[ ! -f "$work/$src" ]]; then
		print -u2 "harness error: a control declares $src, which is not a file in the checkout at $pin"
		exit 1
	fi
	pristine[$src]=$(git -C "$work" hash-object -- "$src")
	if [[ -z "${pristine[$src]}" ]]; then
		print -u2 "harness error: cannot hash $src"
		exit 1
	fi
	mutated_sources+=("$src")
done

note "pin $pin"
if [[ "$run_dir" == "$repo_root"/* ]]; then
	note "report ${run_dir#$repo_root/}"
else
	note "report ${run_dir:t} (under VERIFY_OBSERVER_REPORT_DIR, outside the repository)"
fi
for src in "${mutated_sources[@]}"; do
	note "source $src (${pristine[$src]})"
done

baseline_failed=0
for spec in "${resources_pkg}${sep}${observer_run}${sep}resources${sep}${observer_expect}" "${herd_pkg}${sep}${herd_run}${sep}herd${sep}${herd_expect}"; do
	fields=("${(@ps:$sep:)spec}")
	baseline_exit=$(run_focused "$fields[1]" "$fields[2]" "$run_dir/baseline-$fields[3].json" "$run_dir/baseline-$fields[3].err")
	if (( baseline_exit != 0 )); then
		note "baseline $fields[3] FAILED (exit $baseline_exit) - the suite must pass before any mutant means anything"
		baseline_failed=1
	elif ! baseline_ok "$run_dir/baseline-$fields[3].json" "$fields[4]"; then
		note "baseline $fields[3] FAILED - exit 0 but the required tests did not all pass at top level"
		baseline_failed=1
	else
		note "baseline $fields[3] PASS (every expected test passed at top level)"
	fi
done
(( baseline_failed == 0 )) || exit 1

failures=0
index=0
for record in "${mutations[@]}"; do
	fields=("${(@ps:$sep:)record}")
	name=$fields[1]
	src=$fields[2]
	from=$fields[3]
	to=$fields[4]
	pkg=$fields[5]
	killer=$fields[6]
	assertion=$fields[7]
	index=$(( index + 1 ))

	patch_source "$src" "$from" "$to"

	compile_exit=$(compile_check "$pkg" "$run_dir/compile-$index-$name.log")
	if (( compile_exit != 0 )); then
		note "$name COMPILE-FAIL (exit $compile_exit) - a control that does not build proves nothing"
		failures=$(( failures + 1 ))
		restore_source "$src"
		continue
	fi

	events=$run_dir/mutant-$index-$name.json
	mutant_exit=$(run_focused "$pkg" "^${killer%%/*}\$" "$events" "$run_dir/mutant-$index-$name.err")
	verdict="NOT-KILLED"
	if (( mutant_exit == 124 || mutant_exit == 137 )); then
		verdict="EXTERNAL-TIMEOUT"
	elif ! events_valid "$events"; then
		verdict="EVENTS-UNREADABLE"
	elif run_crashed "$events"; then
		verdict="CRASHED-OR-TIMED-OUT"
	elif killed_by "$events" "$killer" "$assertion" "$mutant_exit"; then
		verdict="KILLED"
	elif (( mutant_exit == 0 )); then
		verdict="MUTANT-SURVIVED"
	elif jq_predicate "$events" "any(.[]; .Action == \"skip\" and .Test == \"$killer\")"; then
		verdict="SKIPPED"
	else
		verdict="WRONG-TEST-OR-ASSERTION"
	fi

	note "$name compile PASS, run $verdict (killer $killer, exit $mutant_exit)"
	[[ "$verdict" == "KILLED" ]] || failures=$(( failures + 1 ))

	restore_source "$src"
done

restore_all

for spec in "${resources_pkg}${sep}${observer_run}${sep}resources${sep}${observer_expect}" "${herd_pkg}${sep}${herd_run}${sep}herd${sep}${herd_expect}"; do
	fields=("${(@ps:$sep:)spec}")
	restored_exit=$(run_focused "$fields[1]" "$fields[2]" "$run_dir/restored-$fields[3].json" "$run_dir/restored-$fields[3].err")
	if (( restored_exit != 0 )); then
		note "restored baseline $fields[3] FAILED (exit $restored_exit) - the source did not come back clean"
		failures=$(( failures + 1 ))
	elif ! baseline_ok "$run_dir/restored-$fields[3].json" "$fields[4]"; then
		note "restored baseline $fields[3] FAILED - exit 0 but the required tests did not all pass"
		failures=$(( failures + 1 ))
	else
		note "restored baseline $fields[3] PASS"
	fi
done

# Cleanup runs BEFORE the verdict, and a leak is a failure of this run rather
# than a warning nobody sees.
if ! cleanup_worktree; then
	note "cleanup FAILED - the ephemeral worktree was left behind"
	failures=$(( failures + 1 ))
fi

if (( failures != 0 )); then
	note "RESULT: $failures control(s) or checks did not pass"
	exit 1
fi
note "RESULT: every control killed its guard, with passing baselines either side"
exit 0
