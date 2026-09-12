#!/usr/bin/env zsh
# FAC-827: non-vacuity driver for the source manifest budget guards.
#
# Runs the focused budget suite, then mutates the REAL production source one
# guard at a time. A mutant counts as killed only when BOTH hold, as separate
# evidence:
#
#   1. it COMPILES, proven by building the test binary, and
#   2. the NAMED killer test fails with the NAMED assertion text, read from
#      `go test -json` rather than scraped from console output.
#
# A compile error, a timeout, a skip, or an unrelated assertion is not a kill.
# Counting any of them is how a vacuous control passes.
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

source_rel=pkg/verifier/hermetic_docker_runner.go
test_pkg=./pkg/verifier/
test_run='TestManifestBudget|TestDefaultManifestBudget'

# Finite, explicit, and bounded at both ends before any arithmetic: an absurd
# or overflowing override must be rejected, not added to.
go_timeout=${VERIFY_BUDGET_GO_TIMEOUT:-180}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 3 )) || (( go_timeout < 30 || go_timeout > 900 )); then
	print -u2 "warning: ignoring unusable VERIFY_BUDGET_GO_TIMEOUT, using 180s"
	go_timeout=180
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
cleanup_timeout=60

# Reports are additive. This script creates ONE subdirectory it owns and never
# deletes a directory chosen by a caller: a path being well formed is not
# permission to destroy what is in it.
report_parent=${VERIFY_BUDGET_REPORT_DIR:-$repo_root/.verify-budget-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
summary=$run_dir/summary.txt
: >| "$summary"

work=""
work_owned=0
cleanup_failed=0

cleanup() {
	local code=$?
	# Only the checkout THIS invocation created, and only once its creation
	# succeeded. No blind fallback deletion, and no global prune: other
	# worktrees and their metadata are not ours to touch.
	if (( work_owned )) && [[ -n "$work" ]]; then
		if ! timeout -k 10s "${cleanup_timeout}s" \
			git -C "$repo_root" worktree remove --force -- "$work" >&2; then
			# The absolute path belongs in the operator's stderr, never in the
			# artifact: the summary is published and this repository forbids
			# absolute paths in it.
			print -u2 "error: could not remove the mutation checkout at $work; it is left in place deliberately"
			print -r -- 'cleanup FAILED: the mutation checkout was left in place (see stderr for its path)' >> "$summary"
			cleanup_failed=1
		fi
	fi
	if (( cleanup_failed )) && (( code == 0 )); then
		code=1
	fi
	return $code
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

note() { print -r -- "$1" >> "$summary"; print -r -- "$1"; }

pin=$(git -C "$repo_root" rev-parse HEAD)
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/verify-budget-XXXXXX")
# mktemp made the directory; `git worktree add` needs the path absent. rmdir
# refuses a non-empty directory, which is the guard we want.
rmdir -- "$work"
git -C "$repo_root" worktree add --detach --quiet -- "$work" "$pin"
work_owned=1

work_pin=$(git -C "$work" rev-parse HEAD)
[[ "$work_pin" == "$pin" ]] || { print -u2 "error: mutation checkout is at $work_pin, not $pin"; exit 1; }
pristine=$(git -C "$work" hash-object -- "$source_rel")
[[ -n "$pristine" ]] || { print -u2 "error: cannot hash $source_rel"; exit 1; }

# compile_check proves the mutant builds. Its result is kept separately from
# the assertion evidence, because "the build broke" and "the test saw the
# guard" are different claims.
compile_check() {
	local log=$1 exit_code=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -p 1 -c -o /dev/null "$test_pkg" ) >"$log" 2>&1 || exit_code=$?
	print -r -- "$exit_code"
}

# run_focused executes one -run selection serially and records machine-readable
# events. Mutants run ONLY their anchored killer and its subtests, so an
# unrelated test's failure or timeout cannot contaminate the verdict; the
# baseline runs the whole focus set.
run_focused() {
	local selector=$1 events=$2 console=$3 exit_code=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$test_pkg" ) >"$events" 2>"$console" || exit_code=$?
	print -r -- "$exit_code"
}

# events_valid proves the whole stream parses BEFORE anything reads it. A parse
# error inside a `jq | grep` conditional is indistinguishable from "no match",
# which would let a truncated stream read as a clean result.
events_valid() {
	jq -e -s 'type == "array" and length > 0' -- "$1" >/dev/null 2>&1
}

# stream_broken looks for build, panic, timeout and tool failures across the
# ENTIRE stream, not just the killer's own events: an expected assertion
# followed by a crash somewhere else is not a successful control.
stream_broken() {
	jq -r 'select(.Action == "output") | .Output // ""' -- "$1" \
		| grep -qE 'panic: |test timed out|\[build failed\]|^# |^signal: |fatal error: '
}

# events_for emits one field of every event bound to the named test or one of
# its subtests, so an assertion can never be credited to a different test.
events_for() {
	local events=$1 test_name=$2 action=$3 field=$4
	jq -r --arg t "$test_name" --arg a "$action" --arg f "$field" \
		'select(.Action == $a and ((.Test // "") == $t or ((.Test // "") | startswith($t + "/")))) | .[$f] // ""' \
		-- "$events"
}

patch_source() {
	local anchor=$1 replacement=$2 file=$work/$source_rel found content mutated
	found=$(grep -F -c -- "$anchor" "$file" || true)
	if [[ "$found" != 1 ]]; then
		print -u2 "error: anchor matched ${found:-0} times, want exactly 1 (source drifted): $anchor"
		return 1
	fi
	content=$(<"$file")
	print -r -- "${content//"$anchor"/"$replacement"}" >| "$file"
	mutated=$(git -C "$work" hash-object -- "$source_rel")
	[[ "$mutated" != "$pristine" ]] || { print -u2 'error: mutation did not change the source'; return 1; }
}

restore_source() {
	local now
	git -C "$work" checkout --quiet -- "$source_rel"
	now=$(git -C "$work" hash-object -- "$source_rel")
	[[ "$now" == "$pristine" ]] || { print -u2 "error: restore left $now, want $pristine"; return 1; }
}

# classify_run maps one mutant onto the contract, given a compile that already
# passed. Only a named assertion failure in the named test counts.
# classify_run maps one mutant onto the contract, given a compile that already
# passed. Only an exact go test exit 1, over a stream with no tool failure
# anywhere in it, naming the killer and carrying the expected text, is a kill.
classify_run() {
	local run_exit=$1 events=$2 killer=$3 want=$4
	if ! events_valid "$events"; then print -r -- 'INVALID-EVENTS'; return; fi
	if (( run_exit == 0 )); then print -r -- 'SURVIVED'; return; fi
	if (( run_exit != 1 )); then print -r -- "TOOLFAIL(exit $run_exit)"; return; fi
	if stream_broken "$events"; then print -r -- 'BROKEN-RUN'; return; fi
	if [[ -n "$(events_for "$events" "$killer" skip Test)" ]]; then print -r -- 'SKIPPED'; return; fi
	if [[ -z "$(events_for "$events" "$killer" fail Test)" ]]; then print -r -- 'WRONG-TEST'; return; fi
	if ! events_for "$events" "$killer" output Output | grep -qF -- "$want"; then
		print -r -- 'WRONG-ASSERTION'; return
	fi
	print -r -- 'KILLED'
}

# id | anchor | replacement | killer test | required assertion text
sep=$'\x1f'
mutations=(
"per-file-refusal-removed${sep}	if size > b.fileBytes {${sep}	if false { // MUTANT: per-file refusal removed${sep}TestManifestBudgetRefusesSingleFileOverThePerFileLimit${sep}expected a refusal"
"aggregate-refusal-removed${sep}	if total > b.totalBytes-size {${sep}	if false { // MUTANT: aggregate refusal removed${sep}TestManifestBudgetRefusesAggregateOfManySmallFiles${sep}expected a refusal"
"aggregate-uses-overflowing-sum${sep}	if total > b.totalBytes-size {${sep}	if total+size > b.totalBytes { // MUTANT: overflowing comparison${sep}TestManifestBudgetAdmitRefusesGenuineInt64Overflow${sep}expected a refusal"
"archive-file-site-bypassed${sep}			if err := budget.admit(\"candidate archive\", \"file\", name, header.Size, total); err != nil {${sep}			if err := error(nil); err != nil { // MUTANT: archive file site bypassed${sep}TestManifestBudgetFileParityAcrossProducerAndReadback${sep}producer and readback disagreed"
"archive-symlink-site-bypassed${sep}			if err := budget.admit(\"candidate archive\", \"symlink target\", name, int64(len(target)), total); err != nil {${sep}			if err := error(nil); err != nil { // MUTANT: archive symlink site bypassed${sep}TestManifestBudgetSymlinkParityAcrossProducerAndReadback${sep}producer and readback disagreed"
"readback-file-site-bypassed${sep}			if err := budget.admit(\"copied source\", \"file\", clean, info.Size(), total); err != nil {${sep}			if err := error(nil); err != nil { // MUTANT: readback file site bypassed${sep}TestManifestBudgetFileParityAcrossProducerAndReadback${sep}producer and readback disagreed"
"readback-symlink-site-bypassed${sep}			if err := budget.admit(\"copied source\", \"symlink target\", clean, int64(len(member.target)), total); err != nil {${sep}			if err := error(nil); err != nil { // MUTANT: readback symlink site bypassed${sep}TestManifestBudgetSymlinkParityAcrossProducerAndReadback${sep}producer and readback disagreed"
)

note "pin $pin"
note "source $source_rel ($pristine)"
# The artifact records WHICH invocation, never where on the host it lives.
if [[ "$run_dir" == "$repo_root"/* ]]; then
	note "report ${run_dir#$repo_root/}"
else
	note "report ${run_dir:t} (under VERIFY_BUDGET_REPORT_DIR, outside the repository)"
fi

baseline_exit=$(run_focused "$test_run" "$run_dir/baseline.json" "$run_dir/baseline.err")
if (( baseline_exit != 0 )); then
	note "baseline FAILED (exit $baseline_exit) - the suite must pass before any mutant means anything"
	exit 1
fi
note 'baseline PASS'

failures=0
index=0
for record in "${mutations[@]}"; do
	(( index += 1 ))
	fields=("${(@ps:$sep:)record}")
	id=$fields[1]; anchor=$fields[2]; replacement=$fields[3]
	killer=$fields[4]; want=$fields[5]
	stem=$run_dir/$(printf 'm%02d-%s' "$index" "$id")

	patch_source "$anchor" "$replacement"
	compile_exit=$(compile_check "$stem.compile.log")
	if (( compile_exit != 0 )); then
		restore_source
		note "mutant $id: COMPILE-FAIL (exit $compile_exit) - a mutant that does not build proves nothing"
		(( failures += 1 ))
		continue
	fi
	# Anchored to this mutant's one killer, subtests included.
	run_exit=$(run_focused "^${killer}$" "$stem.json" "$stem.err")
	verdict=$(classify_run "$run_exit" "$stem.json" "$killer" "$want")
	restore_source

	note "mutant $id: compile PASS, run $verdict (killer $killer, exit $run_exit)"
	if [[ "$verdict" != KILLED ]]; then
		(( failures += 1 ))
		[[ -s "$stem.err" ]] && tail -n 20 -- "$stem.err" >&2
	fi
done

restore_source
restored_exit=$(run_focused "$test_run" "$run_dir/restored.json" "$run_dir/restored.err")
if (( restored_exit != 0 )); then
	note "restored baseline FAILED (exit $restored_exit) - the source did not come back clean"
	exit 1
fi
note 'restored baseline PASS'

if (( failures > 0 )); then
	note "$failures of ${#mutations[@]} controls did not kill their mutant"
	exit 1
fi
note "all ${#mutations[@]} controls compiled and died on their named assertion"
