#!/usr/bin/env zsh
# FAC-829: non-vacuity driver for the resource observer guards.
#
# Runs the observer suites as baselines, then mutates the REAL production source
# one guard at a time. A mutant counts as KILLED only when BOTH hold, as
# separate evidence:
#
#   1. it COMPILES, proven by building the test binary, and
#   2. the NAMED killer test fails with the NAMED assertion text, read from
#      `go test -json` rather than scraped from console output.
#
# A compile error, a timeout, a skip, or an unrelated assertion is not a kill.
# Counting any of them is how a vacuous control passes.
#
# Every control lives in pkg/resources and runs on a fake clock with injected
# probes, so no mutant's verdict depends on the runner's own load. Nothing here
# starts an observer against the host.
#
# All mutation happens in one ephemeral detached worktree this invocation
# creates and owns. The invoking checkout is never written to, and this script
# deletes nothing it did not create.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"

for tool in git go timeout jq mktemp; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

observer_src=pkg/resources/observer.go
paths_src=pkg/resources/observer_paths.go
resources_pkg=./pkg/resources/

# The baseline runs the WHOLE focus set; a mutant runs only its anchored killer,
# so an unrelated failure elsewhere can never be reported as this control's kill.
observer_run='TestObserverDropsOverrunTicksWithoutCatchUp|TestObserverRefusesPublishBeforeObservation|TestObserverStampsPublishTimeAtWriteTime|TestObserverHistoryStaysBounded|TestObserverHistoryNeverRestampsEarlierSamples|TestObserverConfigRefusesBusyLoopBounds|TestObserverConfigAcceptsDefaults|TestObserverUsableFailsClosed|TestObserverUsableAcceptsBothHealthyPlatformShapes|TestObserverUsableRefusesContradictoryReports|TestObserverUsableRefusesBrokenChronology|TestObserverRecordsSampleFailureWithoutAdmitting|TestObserverStopsAtLifetime|TestObserverStopsOnCancellationAndPublishesTermination|TestObserverLockScopeIsReportedHonestly|TestObserverLockIsDistinctFromCapacityAndReaperLocks|TestObserverPathsHaveNoCallerOverride|TestObserverStatusPathCannotBeTheGuardReport|TestReadObserverStatusRefusesAnOversizedFile|TestReadObserverStatusRefusesAnUnknownSchema|TestObserverStatusRoundTripsAtomically|TestRunObserverRefusesWhenTheLockIsHeld|TestRunObserverRefusalDoesNotTouchTheStatusFile'

# Exit 0 alone is not a baseline: a selector that matched nothing also exits 0.
# Every one of these must be seen PASSING at top level.
observer_expect='TestObserverDropsOverrunTicksWithoutCatchUp TestObserverRefusesPublishBeforeObservation TestObserverStampsPublishTimeAtWriteTime TestObserverHistoryStaysBounded TestObserverConfigRefusesBusyLoopBounds TestObserverUsableAcceptsBothHealthyPlatformShapes TestObserverUsableRefusesContradictoryReports TestObserverUsableRefusesBrokenChronology TestObserverRecordsSampleFailureWithoutAdmitting TestObserverLockScopeIsReportedHonestly TestReadObserverStatusRefusesAnOversizedFile'

# Finite and bounded at both ends BEFORE any arithmetic: an absurd or
# overflowing override must be rejected, never added to.
go_timeout=${VERIFY_OBSERVER_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 4 )) || (( go_timeout < 60 || go_timeout > 1800 )); then
	print -u2 "warning: ignoring unusable VERIFY_OBSERVER_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
cleanup_timeout=60

# Reports are additive. This script creates ONE subdirectory it owns and never
# deletes a directory chosen by a caller: a path being well formed is not
# permission to destroy what is in it.
report_parent=${VERIFY_OBSERVER_REPORT_DIR:-$repo_root/.verify-observer-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
summary=$run_dir/summary.txt
: >| "$summary"

work=""
work_owned=0
work_attempted=0
cleanup_failed=0

note() {
	print -r -- "$@" | tee -a "$summary"
}

cleanup() {
	local code=$?
	# Only the checkout THIS invocation created, and only once its creation
	# succeeded. No blind fallback deletion, and no global worktree prune:
	# other worktrees and their metadata are not ours to touch.
	if (( work_owned )) && [[ -n "$work" ]]; then
		if ! timeout -k 10s "${cleanup_timeout}s" \
			git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1; then
			print -u2 "warning: could not remove the ephemeral worktree at $work; it is left in place deliberately"
			cleanup_failed=1
		fi
	elif (( work_attempted )); then
		print -u2 "warning: a worktree path was reserved but never created; nothing was removed"
	fi
	return $code
}
trap cleanup EXIT INT TERM HUP

pin=$(git -C "$repo_root" rev-parse HEAD)
work=$(mktemp -d "$report_parent/work-XXXXXX")
rmdir -- "$work"
work_attempted=1
git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null
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
	local events=$1 filter=$2
	jq -e -s "$filter" "$events" >/dev/null 2>&1
}

# test_emitted_exact binds to the test ITSELF, never to a subtest: a baseline
# that passed only a subtest is not a passing test.
test_emitted_exact() {
	local events=$1 name=$2 action=$3
	jq_predicate "$events" \
		"any(.[]; .Action == \"$action\" and .Test == \"$name\")"
}

baseline_ok() {
	local events=$1 expected=$2 name
	events_valid "$events" || return 1
	for name in ${=expected}; do
		test_emitted_exact "$events" "$name" "pass" || return 1
	done
	return 0
}

# killed_by proves the NAMED test (or subtest) failed AND that its output
# carried the NAMED assertion text. Either alone is not a kill: a test can fail
# for an unrelated reason, and the text can appear in a passing run's log.
killed_by() {
	local events=$1 name=$2 assertion=$3
	events_valid "$events" || return 1
	jq_predicate "$events" \
		"any(.[]; .Action == \"fail\" and .Test == \"$name\")" || return 1
	jq_predicate "$events" \
		"any(.[]; .Action == \"output\" and (.Test // \"\") == \"$name\" and (.Output | contains(\"$assertion\")))"
}

# skipped_or_missing catches the two silent non-kills that look like passes.
run_focused() {
	local pkg=$1 selector=$2 events=$3 console=$4 rc=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$pkg" ) >"$events" 2>"$console" || rc=$?
	print -r -- "$rc"
}

# compile_check proves the mutant BUILDS, separately from whether it fails a
# test. A mutant that does not compile is a broken control, never a kill.
compile_check() {
	local pkg=$1 console=$2 rc=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -c -o /dev/null -p 1 "$pkg" ) >"$console" 2>&1 || rc=$?
	print -r -- "$rc"
}

# count_occurrences walks the literal. `grep -c` counts LINES, so two hits on
# one line would read as one and a uniqueness check would pass wrongly.
count_occurrences() {
	local file=$1 needle=$2
	python3 - "$file" "$needle" <<'PYEOF'
import sys
with open(sys.argv[1], 'r', encoding='utf-8') as handle:
    body = handle.read()
print(body.count(sys.argv[2]))
PYEOF
}

typeset -A pristine
typeset -a mutated_sources

patch_source() {
	local src=$1 from=$2 to=$3 path="$work/$src"
	if [[ -z "${pristine[$src]:-}" ]]; then
		print -u2 "harness error: no pristine hash for $src; refusing to patch an unknown baseline"
		exit 1
	fi
	local hits
	hits=$(count_occurrences "$path" "$from")
	if [[ "$hits" != "1" ]]; then
		print -u2 "harness error: anchor appears $hits time(s) in $src; a control must patch exactly one site"
		exit 1
	fi
	python3 - "$path" "$from" "$to" <<'PYEOF'
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, 'r', encoding='utf-8') as handle:
    body = handle.read()
with open(path, 'w', encoding='utf-8') as handle:
    handle.write(body.replace(old, new, 1))
PYEOF
}

restore_source() {
	local src=$1
	if [[ -z "${pristine[$src]:-}" ]]; then
		print -u2 "harness error: no pristine hash for $src; refusing to restore against an unknown baseline"
		exit 1
	fi
	git -C "$work" checkout -- "$src"
	local now
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
sep=$'\x1f'
mutations=(
"publishes-before-observation${sep}${observer_src}${sep}		if at.Before(completed) {${sep}		if false { // MUTANT: a publish stamp may predate its observation${sep}${resources_pkg}${sep}TestObserverRefusesPublishBeforeObservation${sep}the copied-stale-field defect is not guarded"
"expiry-ignores-metric-window${sep}${observer_src}${sep}	if window.Before(cadence) {${sep}	if false { // MUTANT: expiry no longer bounded by the metric window${sep}${resources_pkg}${sep}TestObserverStampsPublishTimeAtWriteTime${sep}outlives the metric window"
"history-unbounded${sep}${observer_src}${sep}	if len(history) < ObserverHistoryMax {${sep}	if true { // MUTANT: the ring stops being a ring${sep}${resources_pkg}${sep}TestObserverHistoryStaysBounded${sep}history grew to"
"usable-trusts-published-booleans${sep}${observer_src}${sep}	if !again.Admits() {${sep}	if false { // MUTANT: the re-decision no longer overrules the booleans${sep}${resources_pkg}${sep}TestObserverUsableRefusesContradictoryReports/saturated_cpu_with_admit_booleans${sep}published booleans must not outrank published numbers"
"pressure-enum-inconsistency-ignored${sep}${observer_src}${sep}	if level.Known() != r.PressureKnown {${sep}	if false { // MUTANT: the enum and its boolean may disagree${sep}${resources_pkg}${sep}TestObserverUsableRefusesContradictoryReports/pressure_enum_contradicts_pressure_known${sep}published booleans must not outrank published numbers"
"config-accepts-busy-loop${sep}${observer_src}${sep}	if c.Interval < MinObserverInterval || c.Interval > MaxObserverInterval {${sep}	if false { // MUTANT: any interval accepted, including zero${sep}${resources_pkg}${sep}TestObserverConfigRefusesBusyLoopBounds/zero_interval${sep}must be refused, not repaired"
"read-bound-removed${sep}${observer_src}${sep}		return status, fmt.Errorf(\"observer: status exceeds the %d-byte bound; refusing to parse it\",${sep}		_ = fmt.Sprintf(\"%d\", // MUTANT: an oversized status is parsed anyway${sep}${resources_pkg}${sep}TestReadObserverStatusRefusesAnOversizedFile${sep}the bound is not enforced"
"catch-up-burst${sep}${observer_src}${sep}		next := int64(elapsed/cfg.Interval) + 1${sep}		next := tickIndex + 1 // MUTANT: replay every missed tick${sep}${resources_pkg}${sep}TestObserverDropsOverrunTicksWithoutCatchUp${sep}that is a catch-up burst"
"failed-sample-keeps-admit${sep}${observer_src}${sep}			sample.Report.Admits = false${sep}			_ = false // MUTANT: a failed sample keeps its admit${sep}${resources_pkg}${sep}TestObserverRecordsSampleFailureWithoutAdmitting${sep}was reported usable"
"broken-chronology-ignored${sep}${observer_src}${sep}	if why := sampleChronologyProblem(status, at); why != \"\" {${sep}	if why := \"\"; why != \"\" { // MUTANT: stamps no longer have to be credible${sep}${resources_pkg}${sep}TestObserverUsableRefusesBrokenChronology/missing_published_at${sep}was reported usable"
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

baseline_exit=$(run_focused "$resources_pkg" "$observer_run" "$run_dir/baseline.json" "$run_dir/baseline.err")
if (( baseline_exit != 0 )); then
	note "baseline FAILED (exit $baseline_exit) - the suite must pass before any mutant means anything"
	exit 1
fi
if ! baseline_ok "$run_dir/baseline.json" "$observer_expect"; then
	note "baseline FAILED - exit 0 but the required tests did not all pass at top level"
	exit 1
fi
note "baseline PASS (every expected test passed at top level)"

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

	mutant_exit=$(run_focused "$pkg" "^${killer%%/*}\$" "$run_dir/mutant-$index-$name.json" "$run_dir/mutant-$index-$name.err")
	verdict="NOT-KILLED"
	if (( mutant_exit == 124 || mutant_exit == 137 )); then
		verdict="TIMEOUT"
	elif ! events_valid "$run_dir/mutant-$index-$name.json"; then
		verdict="EVENTS-UNREADABLE"
	elif killed_by "$run_dir/mutant-$index-$name.json" "$killer" "$assertion"; then
		verdict="KILLED"
	elif jq_predicate "$run_dir/mutant-$index-$name.json" "any(.[]; .Action == \"skip\" and .Test == \"$killer\")"; then
		verdict="SKIPPED"
	elif jq_predicate "$run_dir/mutant-$index-$name.json" "any(.[]; .Action == \"fail\")"; then
		verdict="WRONG-TEST-OR-ASSERTION"
	fi

	note "$name compile PASS, run $verdict (killer $killer)"
	[[ "$verdict" == "KILLED" ]] || failures=$(( failures + 1 ))

	restore_source "$src"
done

restore_all

restored_exit=$(run_focused "$resources_pkg" "$observer_run" "$run_dir/restored.json" "$run_dir/restored.err")
if (( restored_exit != 0 )); then
	note "restored baseline FAILED (exit $restored_exit) - the source did not come back clean"
	failures=$(( failures + 1 ))
elif ! baseline_ok "$run_dir/restored.json" "$observer_expect"; then
	note "restored baseline FAILED - exit 0 but the required tests did not all pass"
	failures=$(( failures + 1 ))
else
	note "restored baseline PASS"
fi

if (( cleanup_failed )); then
	note "NOTE: the ephemeral worktree could not be removed; see the warning above"
fi

if (( failures != 0 )); then
	note "RESULT: $failures control(s) did not kill their guard"
	exit 1
fi
note "RESULT: every control killed its guard, with passing baselines either side"
exit 0
