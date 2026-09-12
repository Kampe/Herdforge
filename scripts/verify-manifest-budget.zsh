#!/usr/bin/env zsh
# FAC-827: non-vacuity driver for the source manifest budget guards.
#
# Runs the focused budget suite, then mutates the REAL production source one
# guard at a time and requires each mutant to die on a NAMED assertion.
#
# A compile error, a timeout or a tool failure is NOT a kill. Those prove the
# build broke, not that a test can see the guard, and counting them is how a
# vacuous control passes.
#
# Every mutation happens inside an ephemeral detached worktree this script
# creates and removes. The invoking checkout is never written to.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"

for tool in git go timeout; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

source_rel=pkg/verifier/hermetic_docker_runner.go
test_pkg=./pkg/verifier/
test_run='TestManifestBudget|TestDefaultManifestBudget'

# Finite and explicit. The outer wall clock is the backstop for a go test that
# never reaches its own timeout.
go_timeout=${VERIFY_BUDGET_GO_TIMEOUT:-180}
[[ "$go_timeout" =~ '^[1-9][0-9]*$' ]] || go_timeout=180
wall_timeout=$(( go_timeout + 120 ))

log_dir=${VERIFY_BUDGET_LOG_DIR:-$repo_root/.verify-budget-logs}
# The log directory is wiped at start, so refuse a value that would take
# anything else with it: it must be an absolute path with a real leaf.
if [[ "$log_dir" != /*/?* || "$log_dir" == */.. || "$log_dir" == */../* ]]; then
	print -u2 "error: refusing an unsafe log directory"
	exit 1
fi
rm -r -f -- "$log_dir"
mkdir -p -- "$log_dir"
summary=$log_dir/summary.txt
: >| "$summary"

# Trusted runner temp first, so CI never writes the mutation checkout outside
# the space the runner owns and cleans.
temp_base=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
work=""

cleanup() {
	local code=$?
	if [[ -n "$work" ]]; then
		git -C "$repo_root" worktree remove --force -- "$work" >/dev/null 2>&1 || rm -r -f -- "$work"
	fi
	git -C "$repo_root" worktree prune >/dev/null 2>&1 || true
	return $code
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

pin=$(git -C "$repo_root" rev-parse HEAD)
work=$(mktemp -d "$temp_base/verify-budget-XXXXXX")
# mktemp made it; git worktree add needs the path absent. rmdir refuses a
# non-empty directory, which is the guard we want here.
rmdir -- "$work"
git -C "$repo_root" worktree add --detach --quiet -- "$work" "$pin"

work_pin=$(git -C "$work" rev-parse HEAD)
[[ "$work_pin" == "$pin" ]] || { print -u2 "error: mutation checkout is at $work_pin, not $pin"; exit 1; }
pristine=$(git -C "$work" hash-object -- "$source_rel")
[[ -n "$pristine" ]] || { print -u2 "error: cannot hash $source_rel"; exit 1; }

note() { print -r -- "$1" >> "$summary"; print -r -- "$1"; }

# focused runs the suite in the mutation checkout and echoes its exit status.
focused() {
	local log=$1 status=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$test_run" "$test_pkg" ) >"$log" 2>&1 || status=$?
	print -r -- "$status"
}

# patch_source replaces exactly one occurrence of anchor. Any other count is a
# hard failure, so source drift cannot become a silent no-op mutant.
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

# classify maps one mutant run onto the contract. Only a named assertion
# failure in the named test counts as a kill.
classify() {
	local status=$1 log=$2 killer=$3 want=$4
	if (( status == 0 )); then print -r -- 'SURVIVED'; return; fi
	if (( status != 1 )); then print -r -- "TOOLFAIL(exit $status)"; return; fi
	if grep -qF -- '[build failed]' "$log" || grep -qE '^# |panic: test timed out|^signal: ' "$log"; then
		print -r -- 'BUILDFAIL'; return
	fi
	grep -qF -- "--- FAIL: $killer" "$log" || { print -r -- 'WRONG-TEST'; return; }
	grep -qF -- "$want" "$log" || { print -r -- 'WRONG-ASSERTION'; return; }
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

status=$(focused "$log_dir/baseline.log")
if (( status != 0 )); then
	note "baseline FAILED (exit $status) - the suite must pass before any mutant means anything"
	tail -n 40 -- "$log_dir/baseline.log" >&2
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
	log=$log_dir/$(printf 'm%02d-%s.log' "$index" "$id")

	patch_source "$anchor" "$replacement"
	status=$(focused "$log")
	verdict=$(classify "$status" "$log" "$killer" "$want")
	restore_source

	note "mutant $id: $verdict (killer $killer)"
	if [[ "$verdict" != KILLED ]]; then
		(( failures += 1 ))
		tail -n 30 -- "$log" >&2
	fi
done

restore_source
status=$(focused "$log_dir/restored.log")
if (( status != 0 )); then
	note "restored baseline FAILED (exit $status) - the source did not come back clean"
	tail -n 40 -- "$log_dir/restored.log" >&2
	exit 1
fi
note 'restored baseline PASS'

if (( failures > 0 )); then
	note "$failures of ${#mutations[@]} controls did not kill their mutant"
	exit 1
fi
note "all ${#mutations[@]} controls killed their mutant on a named assertion"
