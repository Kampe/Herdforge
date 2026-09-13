#!/usr/bin/env zsh
# FAC-825: non-vacuity driver for the registered census's bounded rotating
# window.
#
# The window exists because one expensive lane could consume the whole
# registered phase before any lane qualified (the measured 0/571 sweep). Its
# tests pass, which says nothing about whether they would notice the window
# being removed. This driver removes each guard from the REAL production source
# one at a time and requires the named oracle to die on its named assertion.
#
# A mutant counts as KILLED only when ALL of these hold, as separate evidence:
#
#   1. it COMPILES, proven by building the test binary in its own phase,
#   2. the run exits NON-ZERO,
#   3. the NAMED killer test emits a `fail` action, and
#   4. that test's own output carries the NAMED assertion,
#
# all read from `go test -json`. A compile error, a panic, a Go-internal test
# timeout, an external timeout, a skip, or an unrelated assertion is NOT a kill
# and is reported under its own verdict, so a vacuous control can never be
# counted as proof.
#
# The controls are grouped by the claim each defends:
#
#   the window is applied BEFORE the expensive work, not after -- the process
#   population batch, the per-lane examination order (status, ownership,
#   landing, lease evidence) and the governor's allocation walk each get their
#   own control, because "bounded" is a different claim at each of those sites;
#   unselected lanes are preserved FAIL-CLOSED rather than silently dropped;
#   the window ROTATES, so the ring is fair across sweeps;
#   the cursor accounts for EXAMINATION, not for success, and a cancelled sweep
#   advances only by what it examined;
#   the deferral is COUNTED and a broken cursor stays VISIBLE.
#
# Mutation happens in one ephemeral detached worktree this invocation creates
# and owns, placed outside the evidence directory so a failed cleanup can never
# be uploaded as an artifact. The invoking checkout is never written to, and
# this script deletes nothing it did not create.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"

for tool in git go timeout jq mktemp rmdir; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

census_src=pkg/resources/git_census.go
governor_src=pkg/resources/governor.go
resources_pkg=./pkg/resources/

# The oracles. The baseline runs the whole set; a mutant runs only its own
# anchored killer, so an unrelated failure can never be reported as this
# control's kill.
census_run='TestRegisteredCensusWindow|TestGovernorCensus'
census_expect='TestRegisteredCensusWindowBoundsExpensiveOpsAndQualifiesSelected TestRegisteredCensusWindowRotatesToNextSlice TestRegisteredCensusWindowSelectedEvidenceFailureStaysUnknown TestRegisteredCensusWindowCancellationAdvancesByAccountedLanes TestRegisteredCensusWindowDefaultBoundsWorkPerSweep TestRegisteredCensusWindowUnwiredEnumeratesAllLanes TestRegisteredCensusWindowAdvancesWhenEverySelectedLaneIsUnproven TestRegisteredCensusWindowAdvancesByExaminedNotByProvenLanes TestRegisteredCensusWindowCancellationLeavesUnexaminedLaneForNextSweep TestRegisteredCensusWindowWrappingCancellationAdvancesInRingOrder TestRegisteredCensusWindowNegativeStartStartsAtRingHead TestRegisteredCensusWindowOversizedStartWrapsIntoRange TestGovernorCensusStageCountsDeferredAndUnknownLanes TestGovernorCensusCountsEnumeratorUnknownsWithoutDoubleCounting TestGovernorCensusCursorWriteFailureIsVisibleAndChangesNoLane'

# Finite and bounded at both ends BEFORE any arithmetic: an absurd or
# overflowing override must be rejected, never added to.
go_timeout=${VERIFY_CENSUS_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 4 )) || (( go_timeout < 60 || go_timeout > 1800 )); then
	print -u2 "warning: ignoring unusable VERIFY_CENSUS_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
worktree_timeout=120
cleanup_timeout=60

report_parent=${VERIFY_CENSUS_REPORT_DIR:-$repo_root/.verify-census-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
summary=$run_dir/summary.txt
: >| "$summary"

work_parent=$(mktemp -d)
work=""
work_owned=0
work_attempted=0
cleanup_done=0
cleanup_rc=0

note() { print -r -- "$@" | tee -a "$summary"; }

# cleanup_worktree is idempotent and returns non-zero when it could not finish.
# It is called EXPLICITLY before the verdict, so a leaked checkout fails the run
# instead of being discovered after the exit status was already decided.
cleanup_worktree() {
	if (( cleanup_done )); then
		return $cleanup_rc
	fi
	cleanup_done=1
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
	if (( cleanup_rc == 0 )) && [[ -n "$work_parent" && -d "$work_parent" ]]; then
		rmdir -- "$work_parent" 2>/dev/null || true
	fi
	return $cleanup_rc
}

on_exit() { cleanup_worktree || true; }
on_signal() {
	trap - EXIT INT TERM HUP
	cleanup_worktree || true
	print -u2 "verify-census-window: terminated by signal"
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

# run_crashed reports an ending no assertion can speak for: a panic, a
# Go-internal test timeout, or a build failure. Any of those can print the
# intended assertion on the way down, so they are excluded BEFORE the kill
# predicate is consulted.
run_crashed() {
	local events=$1
	jq_predicate "$events" \
		'any(.[]; .Action == "output" and (.Output | test("^panic: |^fatal error: |test timed out after |\\[build failed\\]|^# ")))'
}

test_passed() {
	local events=$1
	local name=$2
	jq_predicate "$events" "any(.[]; .Action == \"pass\" and .Test == \"$name\")"
}

baseline_ok() {
	local events=$1
	local name
	events_valid "$events" || return 1
	run_crashed "$events" && return 1
	for name in ${=census_expect}; do
		test_passed "$events" "$name" || return 1
	done
	return 0
}

killed_by() {
	local events=$1
	local name=$2
	local assertion=$3
	local exit_code=$4
	(( exit_code != 0 )) || return 1
	events_valid "$events" || return 1
	run_crashed "$events" && return 1
	jq_predicate "$events" "any(.[]; .Action == \"fail\" and .Test == \"$name\")" || return 1
	jq_predicate "$events" \
		"any(.[]; .Action == \"output\" and (.Test // \"\") == \"$name\" and (.Output | contains(\"$assertion\")))"
}

# Serial by construction: one package, one test binary, one mutant at a time.
run_focused() {
	local selector=$1
	local events=$2
	local console=$3
	local rc=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$resources_pkg" ) >"$events" 2>"$console" || rc=$?
	print -r -- "$rc"
}

# compile_check is a SEPARATE phase from the assertion evidence: "the build
# broke" and "the test saw the guard" are different claims and must not share
# one verdict.
compile_check() {
	local log=$1
	local rc=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -p 1 -c -o /dev/null "$resources_pkg" ) >"$log" 2>&1 || rc=$?
	print -r -- "$rc"
}

typeset -A pristine
typeset -a mutated_sources

# count_occurrences counts the LITERAL, not the lines that contain it: two hits
# on one line would read as one and a uniqueness check would pass wrongly.
count_occurrences() {
	local haystack=$1
	local needle=$2
	local -a parts
	parts=("${(@ps:$needle:)haystack}")
	print -r -- $(( ${#parts} - 1 ))
}

# NOTE: `path` is a zsh special tied to PATH. A local named `path` here would
# replace command lookup with a filename and break every external call that
# follows, so source paths are always `source_path`.
patch_source() {
	local src=$1
	local from=$2
	local to=$3
	local source_path=$work/$src
	local content hits after
	if [[ -z "${pristine[$src]:-}" ]]; then
		print -u2 "harness error: no pristine hash for $src; refusing to patch an unknown baseline"
		exit 1
	fi
	content=$(<"$source_path")
	hits=$(count_occurrences "$content" "$from")
	if [[ "$hits" != "1" ]]; then
		print -u2 "harness error: anchor appears $hits time(s) in $src; a control must patch exactly one site"
		exit 1
	fi
	print -r -- "${content//"$from"/"$to"}" >| "$source_path"
	after=$(git -C "$work" hash-object -- "$src")
	if [[ "$after" == "${pristine[$src]}" ]]; then
		print -u2 "harness error: the mutation did not change $src"
		exit 1
	fi
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

# Row: id | source | anchor | replacement | killer test | required assertion.
#
# Every replacement keeps the original expression syntactically USED -- `false &&`
# rather than a bare `false`, or an explicit `_ =` -- because a mutant that
# leaves an identifier unused does not compile, and a control that cannot build
# proves nothing.
sep=$'\x1f'
mutations=(
"population-probes-whole-ring${sep}${census_src}${sep}			if windowActive {${sep}			if false && windowActive { // MUTANT: the population batch pays for every lane${sep}TestRegisteredCensusWindowBoundsExpensiveOpsAndQualifiesSelected${sep}process probe reached unselected lane"
"unselected-lanes-not-deferred${sep}${census_src}${sep}				lanes[i].State, lanes[i].PreserveReason = LaneUnknown, \"census_window_deferred\"${sep}				_ = i // MUTANT: unselected lanes are not preserved fail-closed${sep}TestRegisteredCensusWindowBoundsExpensiveOpsAndQualifiesSelected${sep}must be preserved as census_window_deferred unknown"
"examination-covers-whole-ring${sep}${census_src}${sep}		for offset := 0; offset < windowLimit; offset++ {${sep}		for offset := 0; offset < len(lanes); offset++ { // MUTANT: every lane pays for evidence${sep}TestRegisteredCensusWindowBoundsExpensiveOpsAndQualifiesSelected${sep}lease evidence must be read for exactly the selected lanes in order"
"window-never-rotates${sep}${census_src}${sep}		next := (windowStart + accounted) % len(lanes)${sep}		next := windowStart % len(lanes) // MUTANT: the cursor never leaves this window${sep}TestRegisteredCensusWindowRotatesToNextSlice${sep}first sweep must advance the cursor to 2"
"cursor-never-accounts-examination${sep}${census_src}${sep}		accounted++${sep}		_ = pos // MUTANT: examination is never accounted${sep}TestRegisteredCensusWindowAdvancesWhenEverySelectedLaneIsUnproven${sep}a fully unproven window must still advance by its width"
"cancelled-sweep-is-not-truncated${sep}${census_src}${sep}		if windowActive && ctx.Err() != nil {${sep}		if false && windowActive && ctx.Err() != nil { // MUTANT: cancellation no longer truncates the window${sep}TestRegisteredCensusWindowCancellationLeavesUnexaminedLaneForNextSweep${sep}cursor must advance by the one examined lane only"
"deferred-lane-pays-allocation-walk${sep}${governor_src}${sep}		if lanes[i].PreserveReason == \"census_window_deferred\" {${sep}		if false && lanes[i].PreserveReason == \"census_window_deferred\" { // MUTANT: deferred lanes walk the allocation tree${sep}TestGovernorCensusStageCountsDeferredAndUnknownLanes${sep}paid for the allocation walk"
"deferral-is-not-counted${sep}${governor_src}${sep}		if lanes[i].State == LaneUnknown {${sep}		if false && lanes[i].State == LaneUnknown { // MUTANT: unknown lanes are not counted as deferred${sep}TestGovernorCensusStageCountsDeferredAndUnknownLanes${sep}lanes left unknown"
"broken-cursor-is-invisible${sep}${governor_src}${sep}	listStage.CursorError = g.registeredCursorPersistErr${sep}	_ = g.registeredCursorPersistErr // MUTANT: a failed cursor advance is hidden${sep}TestGovernorCensusCursorWriteFailureIsVisibleAndChangesNoLane${sep}a failed cursor advance must surface as a partial diagnostic on the stage"
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
	note "report ${run_dir:t} (under VERIFY_CENSUS_REPORT_DIR, outside the repository)"
fi
# The source SHAs are retained with the evidence: a log that does not say what
# it mutated cannot be checked against the candidate later.
for src in "${mutated_sources[@]}"; do
	note "source $src (${pristine[$src]})"
done

failures=0

baseline_exit=$(run_focused "$census_run" "$run_dir/baseline.json" "$run_dir/baseline.err")
if (( baseline_exit != 0 )) || ! baseline_ok "$run_dir/baseline.json"; then
	note "baseline FAILED (exit $baseline_exit) - the suite must pass before any mutant means anything"
	exit 1
fi
note "baseline PASS (every expected test passed at top level)"

index=0
for record in "${mutations[@]}"; do
	fields=("${(@ps:$sep:)record}")
	id=$fields[1]
	src=$fields[2]
	from=$fields[3]
	to=$fields[4]
	killer=$fields[5]
	assertion=$fields[6]
	(( index += 1 ))
	stem=$run_dir/$(printf 'm%02d-%s' "$index" "$id")

	patch_source "$src" "$from" "$to"
	compile_exit=$(compile_check "$stem.compile.log")
	if (( compile_exit != 0 )); then
		restore_source "$src"
		note "mutant $id: COMPILE-FAIL (exit $compile_exit) - a mutant that does not build proves nothing"
		(( failures += 1 ))
		continue
	fi

	mutant_exit=$(run_focused "^${killer}\$" "$stem.json" "$stem.err")
	verdict="NOT-KILLED"
	if (( mutant_exit == 124 || mutant_exit == 137 )); then
		verdict="EXTERNAL-TIMEOUT"
	elif ! events_valid "$stem.json"; then
		verdict="EVENTS-UNREADABLE"
	elif run_crashed "$stem.json"; then
		verdict="CRASHED-OR-TIMED-OUT"
	elif killed_by "$stem.json" "$killer" "$assertion" "$mutant_exit"; then
		verdict="KILLED"
	elif (( mutant_exit == 0 )); then
		verdict="MUTANT-SURVIVED"
	elif jq_predicate "$stem.json" "any(.[]; .Action == \"skip\" and .Test == \"$killer\")"; then
		verdict="SKIPPED"
	else
		verdict="WRONG-TEST-OR-ASSERTION"
	fi

	note "mutant $id: compile PASS, run $verdict (killer $killer, exit $mutant_exit)"
	[[ "$verdict" == "KILLED" ]] || (( failures += 1 ))
	restore_source "$src"
done

for src in "${mutated_sources[@]}"; do
	restore_source "$src"
done

restored_exit=$(run_focused "$census_run" "$run_dir/restored.json" "$run_dir/restored.err")
if (( restored_exit != 0 )) || ! baseline_ok "$run_dir/restored.json"; then
	note "restored baseline FAILED (exit $restored_exit) - the source did not come back clean"
	(( failures += 1 ))
else
	note "restored baseline PASS"
fi

if ! cleanup_worktree; then
	note "cleanup FAILED - the ephemeral worktree was left behind"
	(( failures += 1 ))
fi

if (( failures != 0 )); then
	note "RESULT: $failures control(s) or checks did not pass"
	exit 1
fi
note "RESULT: every control killed its guard, with passing baselines either side"
exit 0
