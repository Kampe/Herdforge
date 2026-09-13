#!/usr/bin/env zsh
# FAC-613: proves the lifecycle refusal diagnostics are not vacuous.
#
# The boundary attaches the census stage summary to a refusal. Each guard below
# is removed from the REAL source, one at a time, and the named oracle must die
# on its named assertion. A mutant counts as KILLED only when it COMPILES, the
# run exits non-zero, the NAMED test emits a fail action, and that test's own
# output carries the NAMED assertion — all read from `go test -json`, so a
# build failure, panic or unrelated failure is never counted as proof.
#
# Deliberately short: this is one narrow diagnostic change, not a framework.
set -euo pipefail

repo_root=$(git -C "${0:A:h}/.." rev-parse --show-toplevel)
cd "$repo_root"
for tool in git go timeout jq; do
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

pristine=$(git hash-object -- "$src")
[[ -n "$pristine" ]] || { print -u2 "error: cannot hash $src"; exit 1; }
restore() {
	git checkout -- "$src"
	local now=$(git hash-object -- "$src")
	[[ "$now" == "$pristine" ]] || { print -u2 "harness error: $src did not restore"; exit 1; }
}
trap 'restore' EXIT INT TERM HUP

# The registry: name, anchor, replacement, killer, first assertion. Every
# anchor must occur EXACTLY ONCE in the live source, checked before the
# baseline is spent so a drifted row fails fast instead of reading as a
# survivor.
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
)

occurrences() {
	local haystack=$1 needle=$2
	local -a parts
	parts=("${(@ps:$needle:)haystack}")
	print -r -- $(( ${#parts} - 1 ))
}

live=$(<"$src")
for row in "${controls[@]}"; do
	typeset -a f; f=("${(@ps:$sep:)row}")
	(( ${#f} == 5 )) || { print -u2 "harness error: control ${f[1]:-?} has ${#f} fields, want 5"; exit 1; }
	[[ -n "$f[1]" && -n "$f[2]" && -n "$f[3]" && -n "$f[4]" && -n "$f[5]" ]] || { print -u2 "harness error: empty field in $f[1]"; exit 1; }
	[[ "$f[2]" != "$f[3]" ]] || { print -u2 "harness error: $f[1] replacement equals its anchor"; exit 1; }
	[[ " ${oracles[*]} " == *" $f[4] "* ]] || { print -u2 "harness error: $f[1] names a killer outside the baseline"; exit 1; }
	hits=$(occurrences "$live" "$f[2]")
	[[ "$hits" == 1 ]] || { print -u2 "harness error: $f[1] anchor occurs $hits time(s) in $src"; exit 1; }
done
note "registry: ${#controls} control(s) validated against the live source"

events_ok() { jq -e -s 'length > 0' "$1" >/dev/null 2>&1; }
crashed() { jq -e -s 'any(.[]; .Action=="output" and (.Output | test("^panic: |^fatal error: |test timed out after |\\[build failed\\]|^# ")))' "$1" >/dev/null 2>&1; }

run_tests() {
	local sel=$1 events=$2 console=$3 rc=0
	timeout -k 10s 600s go test -json -count=1 -p 1 -parallel 1 -timeout 300s -run "$sel" "$pkg" >"$events" 2>"$console" || rc=$?
	print -r -- "$rc"
}

baseline_events=$run_dir/baseline.json
rc=$(run_tests "$selector" "$baseline_events" "$run_dir/baseline.console")
if (( rc != 0 )) || ! events_ok "$baseline_events" || crashed "$baseline_events"; then
	note "BASELINE FAILED (exit $rc) — see $baseline_events"
	exit 1
fi
for name in "${oracles[@]}"; do
	jq -e -s "any(.[]; .Action==\"pass\" and .Test==\"$name\")" "$baseline_events" >/dev/null 2>&1 \
		|| { note "BASELINE did not pass $name"; exit 1; }
done
note "baseline: ${#oracles} oracle(s) pass on unmutated source"

failures=0
for row in "${controls[@]}"; do
	typeset -a f; f=("${(@ps:$sep:)row}")
	name=$f[1]; killer=$f[4]; want=$f[5]
	content=$(<"$src")
	print -r -- "${content//"$f[2]"/"$f[3]"}" >| "$src"
	[[ "$(git hash-object -- "$src")" != "$pristine" ]] || { note "harness error: $name did not change $src"; exit 1; }
	git diff -- "$src" >| "$run_dir/$name.diff" 2>/dev/null || true

	compile_rc=0
	timeout -k 10s 300s go test -p 1 -c -o /dev/null "$pkg" >"$run_dir/$name.compile" 2>&1 || compile_rc=$?
	if (( compile_rc != 0 )); then
		note "BROKEN-MUTANT $name: does not compile (see $run_dir/$name.compile)"
		restore; failures=$(( failures + 1 )); continue
	fi

	events=$run_dir/$name.json
	rc=$(run_tests "^${killer}\$" "$events" "$run_dir/$name.console")
	restore

	if (( rc == 0 )); then
		note "SURVIVED  $name: $killer passed with the guard removed"
		failures=$(( failures + 1 ))
	elif crashed "$events"; then
		note "BROKEN-RUN $name: the run crashed rather than asserting"
		failures=$(( failures + 1 ))
	elif ! jq -e -s "any(.[]; .Action==\"fail\" and .Test==\"$killer\")" "$events" >/dev/null 2>&1; then
		note "WRONG-TEST $name: something other than $killer failed"
		failures=$(( failures + 1 ))
	elif ! jq -e -s "any(.[]; .Action==\"output\" and (.Test // \"\")==\"$killer\" and (.Output | contains(\"$want\")))" "$events" >/dev/null 2>&1; then
		note "WRONG-ASSERTION $name: $killer failed, but not on \"$want\""
		failures=$(( failures + 1 ))
	else
		note "KILLED    $name -> $killer: \"$want\""
	fi
done

if (( failures )); then
	note "verify-lifecycle-diagnostics: $failures control(s) did not prove their guard"
	exit 1
fi
note "verify-lifecycle-diagnostics: every control killed its named oracle"
