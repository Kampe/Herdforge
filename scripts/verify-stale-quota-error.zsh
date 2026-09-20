#!/usr/bin/env zsh
# FAC-818: run on hosted CI. Remove only stale-snapshot error propagation in
# an owned checkout, require compilation, then require the named assertion.
set -euo pipefail

repo_root=$(git -C "${0:A:h}/.." rev-parse --show-toplevel)
for tool in git go timeout jq mktemp rmdir; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done
pin=$(git -C "$repo_root" rev-parse HEAD)
src=pkg/usage/cache.go
oracle=TestStaleBackoffLimitsRetainsProviderError
assertion='stale limits lost exact-provider telemetry error'
anchor=$'\t\tErrors:      map[string]string{name: record.Error},'
pristine=$(git -C "$repo_root" rev-parse "$pin:$src")
content=$(git -C "$repo_root" show "$pin:$src")
parts=("${(@ps:$anchor:)content}")
(( ${#parts} == 2 )) || { print -u2 'error: mutation anchor must occur exactly once'; exit 1; }

report_parent=${VERIFY_STALE_QUOTA_REPORT_DIR:-$repo_root/.verify-stale-quota-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
work_parent=$(mktemp -d)
work=$work_parent/checkout
owned=0
cleanup() {
	if (( owned )); then
		timeout -k 10s 60s git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1 || return 1
		owned=0
	fi
	if [[ -d "$work_parent" ]]; then
		rmdir -- "$work_parent" || return 1
	fi
}
trap 'cleanup || print -u2 "error: owned checkout cleanup failed"' EXIT
trap 'trap - EXIT INT TERM HUP; cleanup || true; exit 143' INT TERM HUP
timeout -k 10s 120s git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null 2>&1
owned=1
[[ "$(git -C "$work" hash-object -- "$src")" == "$pristine" ]] || exit 1
print -r -- "pin: $pin" > "$run_dir/summary.txt"
note() { print -r -- "$@" | tee -a "$run_dir/summary.txt"; }

run_oracle() {
	local rc=0
	( cd "$work" && timeout -k 10s 600s go test -json -count=1 -p 1 -parallel 1 -timeout 300s -run "^${oracle}\$" ./pkg/usage/ ) > "$1" 2> "$2" || rc=$?
	print -r -- "$rc"
}
crashed() {
	jq -e -s 'any(.[]; .Action == "output" and (.Output | test("^panic: |^fatal error: |test timed out after |\\[build failed\\]|^# ")))' "$1" >/dev/null 2>&1
}
events_valid() {
	jq -e -s --arg oracle "$oracle" '
		length > 0 and all(.[]; type == "object") and
		(all(.[]; .Action != "skip" or .Test != $oracle))
	' "$1" >/dev/null 2>&1
}
oracle_passed() {
	events_valid "$1" && ! crashed "$1" &&
		jq -e -s --arg oracle "$oracle" 'any(.[]; .Action == "pass" and .Test == $oracle)' "$1" >/dev/null
}
baseline=$run_dir/baseline.json
rc=$(run_oracle "$baseline" "$run_dir/baseline.console")
if (( rc != 0 )) || ! oracle_passed "$baseline"; then
	note "baseline: FAILED (exit $rc)"
	print -u2 'error: named baseline did not pass'
	exit 1
fi
note "baseline: PASS $oracle"

print -r -- "${content//"$anchor"/}" > "$work/$src"
[[ "$(git -C "$work" hash-object -- "$src")" != "$pristine" ]] || exit 1
git -C "$work" diff -- "$src" > "$run_dir/mutant.diff"
( cd "$work" && timeout -k 10s 300s go test -p 1 -c -o /dev/null ./pkg/usage/ ) > "$run_dir/mutant.compile" 2>&1 || {
	note 'compile: FAILED'
	print -u2 'BROKEN-MUTANT: compile failed; this is not assertion evidence'
	exit 1
}
note 'compile: PASS mutant'
events=$run_dir/mutant.json
rc=$(run_oracle "$events" "$run_dir/mutant.console")
git -C "$work" checkout -- "$src"
[[ "$(git -C "$work" hash-object -- "$src")" == "$pristine" ]] || exit 1
if (( rc != 1 )) || ! events_valid "$events" || crashed "$events" || ! jq -e -s --arg oracle "$oracle" --arg assertion "$assertion" '
	any(.[]; .Action == "fail" and .Test == $oracle) and
	any(.[]; .Action == "output" and .Test == $oracle and (.Output | contains($assertion)))
' "$events" >/dev/null; then
	note "kill: FAILED (exit $rc)"
	print -u2 "BROKEN-RUN: mutant exit $rc did not prove the named assertion"
	exit 1
fi
note "kill: PASS $oracle: $assertion"
restored=$run_dir/restored.json
rc=$(run_oracle "$restored" "$run_dir/restored.console")
if (( rc != 0 )) || ! oracle_passed "$restored"; then
	note "restored: FAILED (exit $rc)"
	print -u2 'error: named oracle did not pass after restoring production source'
	exit 1
fi
note "restored: PASS $oracle"
cleanup || { print -u2 'error: owned checkout cleanup failed'; exit 1; }
note "KILLED: $oracle: $assertion"
