#!/usr/bin/env zsh
# FAC-613: proves the lifecycle refusal diagnostics are not vacuous.
#
# Each guard is removed from the REAL source, one at a time, and the named
# oracle must die on its named assertion. A mutant counts as KILLED only when
# it COMPILES, the run ends as a NORMAL Go test failure, the NAMED test emits a
# fail action, and that test's own output carries the NAMED assertion. An
# external timeout, a signal, a panic, a build failure or an unrelated failure
# is reported under its own verdict and never counted as proof.
#
# Mutation happens ONLY inside one detached worktree this script creates and
# owns. The invoking checkout is never written to: its source, index and
# untracked files are left exactly as they were, so a dirty working tree or
# another agent sharing the checkout cannot be damaged or exposed to mutant
# source. Evidence is written outside that worktree so a failed cleanup cannot
# take it with it.
set -euo pipefail

repo_root=$(git -C "${0:A:h}/.." rev-parse --show-toplevel)
cd "$repo_root"
for tool in git go timeout jq mktemp rmdir; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

src=cmd/herd/resource_governor.go
pkg=./cmd/herd/
oracles=(
	TestLifecycleBoundaryRefusalCarriesCensusStages
	TestLifecycleBoundaryRefusalPreservesErrorIdentity
	TestLifecycleBoundaryCancellationKeepsItsIdentity
	TestLifecycleSuccessIsNeverWrapped
	TestSummaryKeepsTheFailingPhaseAndItsCounts
	TestSummaryDropsEarliestStagesAndSaysSo
	TestSummaryCarriesNoPathOrCause
	TestRefusalWithoutStagesIsReturnedUnchanged
)
selector="^($(print -r -- ${(j:|:)oracles}))\$"

log_dir=${VERIFY_DIAG_REPORT_DIR:-$repo_root/.verify-lifecycle-diagnostics-logs}
mkdir -p -- "$log_dir"
run_dir=$(mktemp -d "$log_dir/run-XXXXXX")
note() { print -r -- "$@" | tee -a "$run_dir/summary.txt"; }

# ---------------------------------------------------------------------------
# The registry. name | anchor | replacement | killer | first assertion.
# ---------------------------------------------------------------------------
sep=$'\x1f'
controls=(
"boundary-keeps-the-summary${sep}	return report, lifecycleSweepFailure(report, err)${sep}	return report, err // MUTANT: the boundary discards the report again${sep}TestLifecycleBoundaryRefusalCarriesCensusStages${sep}returned a refusal with no stage summary"
"refusal-keeps-its-identity${sep}func (e *lifecycleSweepError) Unwrap() error { return e.err }${sep}func (e *lifecycleSweepError) Unwrap() error { return nil } // MUTANT: identity dropped${sep}TestLifecycleBoundaryRefusalPreservesErrorIdentity${sep}errors.Is no longer answers for the cause"
"success-stays-success${sep}	if err == nil {
		return nil
	}
	summary := censusStageSummary(report.Stages)${sep}	if false && err == nil { // MUTANT: a successful sweep is wrapped
		return nil
	}
	summary := censusStageSummary(report.Stages)${sep}TestLifecycleSuccessIsNeverWrapped${sep}a successful sweep was turned into a refusal"
"failing-phase-survives-truncation${sep}		records = records[1:]${sep}		records = records[:len(records)-1] // MUTANT: the LAST stage is dropped${sep}TestSummaryDropsEarliestStagesAndSaysSo${sep}truncation discarded the failing phase"
"summary-carries-no-raw-text${sep}		b.WriteString(\" cursor_error\")${sep}		b.WriteString(\" \" + stage.CursorError) // MUTANT: the raw cursor text reaches the refusal${sep}TestSummaryCarriesNoPathOrCause${sep}summary leaked a path"
)


# The pin is captured first: the registry is validated against the content the
# ephemeral worktree will actually carry, not against the invoking checkout's
# working file, which may be dirty and is never mutated.
pin=$(git rev-parse HEAD)
pristine=$(git rev-parse "$pin:$src")
occurrences() {
	local -a parts
	parts=("${(@ps:$2:)1}")
	print -r -- $(( ${#parts} - 1 ))
}

# Registry validation BEFORE any baseline is spent: field count, empty fields,
# duplicate names, a killer the baseline actually runs, and an anchor that
# occurs exactly once in the live source.
typeset -A seen
live=$(git show "$pin:$src")
for row in "${controls[@]}"; do
	typeset -a f; f=("${(@ps:$sep:)row}")
	(( ${#f} == 5 )) || { print -u2 "harness error: control ${f[1]:-?} has ${#f} field(s), want 5"; exit 1; }
	for field in "$f[1]" "$f[2]" "$f[3]" "$f[4]" "$f[5]"; do
		[[ -n "$field" ]] || { print -u2 "harness error: empty field in control $f[1]"; exit 1; }
	done
	[[ -z "${seen[$f[1]]:-}" ]] || { print -u2 "harness error: duplicate control name $f[1]"; exit 1; }
	seen[$f[1]]=1
	[[ "$f[2]" != "$f[3]" ]] || { print -u2 "harness error: $f[1] replacement equals its anchor"; exit 1; }
	[[ " ${oracles[*]} " == *" $f[4] "* ]] || { print -u2 "harness error: $f[1] names a killer outside the baseline"; exit 1; }
	hits=$(occurrences "$live" "$f[2]")
	[[ "$hits" == 1 ]] || { print -u2 "harness error: $f[1] anchor occurs $hits time(s) in $src"; exit 1; }
done
note "registry: ${#controls} control(s) validated against the pinned source"

# ---------------------------------------------------------------------------
# One owned, detached worktree. The invoking checkout is never mutated.
# ---------------------------------------------------------------------------
work_parent=$(mktemp -d)
work=""
owned=0
cleaned=0
cleanup_rc=0
cleanup() {
	(( cleaned )) && return $cleanup_rc
	cleaned=1
	if (( owned )) && [[ -n "$work" ]]; then
		if timeout -k 10s 60s git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1; then
			owned=0
		else
			print -u2 "warning: the ephemeral worktree could not be removed; it is left in place deliberately"
			cleanup_rc=1
		fi
	fi
	(( cleanup_rc == 0 )) && [[ -d "$work_parent" ]] && rmdir -- "$work_parent" 2>/dev/null
	return $cleanup_rc
}
# A signal TERMINATES after cleanup. It must never fall back into the loop.
trap 'cleanup || true' EXIT
trap 'trap - EXIT INT TERM HUP; cleanup || true; print -u2 "verify-lifecycle-diagnostics: terminated by signal"; exit 143' INT TERM HUP

work=$work_parent/checkout
timeout -k 10s 120s git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null 2>&1 \
	|| { print -u2 "harness error: could not create the ephemeral worktree"; exit 1; }
owned=1
[[ "$(git -C "$work" hash-object -- "$src")" == "$pristine" ]] \
	|| { print -u2 "harness error: the ephemeral worktree does not carry the pinned source"; exit 1; }
note "pin: $pin"
note "pristine $src: $pristine"
note "evidence: $run_dir"

restore() {
	git -C "$work" checkout -- "$src"
	local now=$(git -C "$work" hash-object -- "$src")
	[[ "$now" == "$pristine" ]] || { print -u2 "harness error: $src did not restore in the ephemeral worktree"; exit 1; }
}

events_ok() { jq -e -s 'length > 0' "$1" >/dev/null 2>&1; }
crashed() { jq -e -s 'any(.[]; .Action=="output" and (.Output | test("^panic: |^fatal error: |test timed out after |\\[build failed\\]|^# ")))' "$1" >/dev/null 2>&1; }
fired()   { jq -e -s "any(.[]; .Action==\"fail\" and .Test==\"$2\")" "$1" >/dev/null 2>&1; }
asserted(){ jq -e -s "any(.[]; .Action==\"output\" and (.Test // \"\")==\"$2\" and (.Output | contains(\"$3\")))" "$1" >/dev/null 2>&1; }

# go test reports an ordinary failure as exit 1. timeout reports 124, and a
# signalled child reports 128+N. Those are not assertion evidence.
run_tests() {
	local rc=0
	( cd "$work" && timeout -k 10s 600s go test -json -count=1 -p 1 -parallel 1 -timeout 300s -run "$1" "$pkg" ) >"$2" 2>"$3" || rc=$?
	print -r -- "$rc"
}

baseline=$run_dir/baseline.json
rc=$(run_tests "$selector" "$baseline" "$run_dir/baseline.console")
if (( rc != 0 )) || ! events_ok "$baseline" || crashed "$baseline"; then
	note "BASELINE FAILED (exit $rc) — see $baseline"
	exit 1
fi
for name in "${oracles[@]}"; do
	jq -e -s "any(.[]; .Action==\"pass\" and .Test==\"$name\")" "$baseline" >/dev/null 2>&1 \
		|| { note "BASELINE did not pass $name"; exit 1; }
done
note "baseline: ${#oracles} oracle(s) pass on the pinned source"

failures=0
for row in "${controls[@]}"; do
	typeset -a f; f=("${(@ps:$sep:)row}")
	name=$f[1]; killer=$f[4]; want=$f[5]
	content=$(<"$work/$src")
	print -r -- "${content//"$f[2]"/"$f[3]"}" >| "$work/$src"
	[[ "$(git -C "$work" hash-object -- "$src")" != "$pristine" ]] \
		|| { note "harness error: $name did not change $src"; exit 1; }
	git -C "$work" diff -- "$src" >| "$run_dir/$name.diff" 2>/dev/null || true

	compile_rc=0
	( cd "$work" && timeout -k 10s 300s go test -p 1 -c -o /dev/null "$pkg" ) >"$run_dir/$name.compile" 2>&1 || compile_rc=$?
	if (( compile_rc != 0 )); then
		note "BROKEN-MUTANT $name: does not compile"
		restore; failures=$(( failures + 1 )); continue
	fi

	events=$run_dir/$name.json
	rc=$(run_tests "^${killer}\$" "$events" "$run_dir/$name.console")
	restore

	if (( rc == 0 )); then
		note "SURVIVED  $name: $killer passed with the guard removed"
		failures=$(( failures + 1 ))
	elif (( rc == 124 || rc >= 128 )); then
		note "BROKEN-RUN $name: ended by external timeout or signal (exit $rc), not by an assertion"
		failures=$(( failures + 1 ))
	elif (( rc != 1 )) || ! events_ok "$events" || crashed "$events"; then
		note "BROKEN-RUN $name: not an ordinary test failure (exit $rc)"
		failures=$(( failures + 1 ))
	elif ! fired "$events" "$killer"; then
		note "WRONG-TEST $name: something other than $killer failed"
		failures=$(( failures + 1 ))
	elif ! asserted "$events" "$killer" "$want"; then
		note "WRONG-ASSERTION $name: $killer failed, but not on \"$want\""
		failures=$(( failures + 1 ))
	else
		note "KILLED    $name -> $killer: \"$want\""
	fi
done

final=$(git -C "$work" hash-object -- "$src")
note "restored $src: $final"
[[ "$final" == "$pristine" ]] || { note "harness error: $src did not end at its pinned content"; failures=$(( failures + 1 )); }

cleanup || { note "harness error: the ephemeral worktree could not be removed"; failures=$(( failures + 1 )); }

if (( failures )); then
	note "verify-lifecycle-diagnostics: $failures control(s) did not prove their guard"
	exit 1
fi
note "verify-lifecycle-diagnostics: every control killed its named oracle"
