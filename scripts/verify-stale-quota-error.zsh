#!/usr/bin/env zsh
# FAC-818: run on hosted CI. Remove each stale-snapshot error guard in an owned
# checkout, require compilation, then require its named assertion and restore.
set -euo pipefail

repo_root=$(git -C "${0:A:h}/.." rev-parse --show-toplevel)
for tool in git go timeout jq mktemp rmdir rm; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done
pin=$(git -C "$repo_root" rev-parse HEAD)
src=pkg/usage/cache.go
oracle=TestStaleBackoffLimitsRetainsProviderError
sep=$'\x1f'
controls=(
"propagation${sep}"$'\t\tsnap.Errors = map[string]string{name: record.Error}'"${sep}// MUTANT: error propagation removed${sep}stale limits lost exact-provider telemetry error"
"empty-error${sep}"$'\tif record.Error != "" {\n\t\tsnap.Errors'"${sep}"$'\tif true {\n\t\tsnap.Errors'"${sep}errorless cache record fabricated an empty telemetry error"
)
pristine=$(git -C "$repo_root" rev-parse "$pin:$src")
content=$(git -C "$repo_root" show "$pin:$src")
for control in "${controls[@]}"; do
	fields=("${(@ps:$sep:)control}")
	(( ${#fields} == 4 )) || { print -u2 'error: malformed mutation control'; exit 1; }
	anchor=$fields[2]
	parts=("${(@ps:$anchor:)content}")
	(( ${#parts} == 2 )) || { print -u2 'error: mutation anchor must occur exactly once'; exit 1; }
done

report_parent=${VERIFY_STALE_QUOTA_REPORT_DIR:-$repo_root/.verify-stale-quota-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
work_parent=$(mktemp -d)
work_parent=$(cd "$work_parent" && pwd -P)
work=$work_parent/checkout
owned=0
cleanup() {
	if (( owned )); then
		local listing row registered=0
		[[ ! -L "$work_parent" && "$work" == "$work_parent/checkout" ]] || return 1
		listing=$(timeout -k 10s 10s git -C "$repo_root" worktree list --porcelain -z) || return 1
		for row in "${(@0)listing}"; do
			[[ "$row" != "worktree $work" ]] || registered=1
		done
		if (( registered )); then
			timeout -k 10s 60s git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1 || return 1
		else
			# Git may fail before registration, leaving partial checkout files.
			# Only this invocation's reserved child may be removed directly.
			timeout -k 10s 10s rm -rf -- "$work" || return 1
		fi
		owned=0
	fi
	if [[ -d "$work_parent" ]]; then
		rmdir -- "$work_parent" || return 1
	fi
}
on_exit() {
	local rc=$?
	trap - EXIT
	cleanup || { print -u2 "error: owned checkout cleanup failed"; exit 1; }
	exit "$rc"
}
trap on_exit EXIT
trap 'exit 143' INT TERM HUP
# Arm cleanup before Git can create or register the disposable checkout.
owned=1
timeout -k 10s 120s git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null 2>&1
# Worktree creation is complete; verification starts below.
[[ "$(git -C "$work" hash-object -- "$src")" == "$pristine" ]] || exit 1
print -r -- "pin: $pin" > "$run_dir/summary.txt"
note() { print -r -- "$@" | tee -a "$run_dir/summary.txt"; }
timeout -k 10s 120s zsh "$repo_root/scripts/verify-stale-quota-cleanup.zsh" "$run_dir/cleanup"

run_oracle() {
	local rc=0
	( cd "$work" && timeout -k 10s 180s go test -json -count=1 -p 1 -parallel 1 -timeout 60s -run "^${oracle}\$" ./pkg/usage/ ) > "$1" 2> "$2" || rc=$?
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

for control in "${controls[@]}"; do
	fields=("${(@ps:$sep:)control}")
	name=$fields[1]
	anchor=$fields[2]
	replacement=$fields[3]
	assertion=$fields[4]
	control_dir=$run_dir/$name
	mkdir -p -- "$control_dir"
	print -r -- "${content//"$anchor"/"$replacement"}" > "$work/$src"
	[[ "$(git -C "$work" hash-object -- "$src")" != "$pristine" ]] || exit 1
	git -C "$work" diff -- "$src" > "$control_dir/mutant.diff"
	( cd "$work" && timeout -k 10s 120s go test -p 1 -c -o /dev/null ./pkg/usage/ ) > "$control_dir/mutant.compile" 2>&1 || {
		note "compile: FAILED $name"
		print -u2 'BROKEN-MUTANT: compile failed; this is not assertion evidence'
		exit 1
	}
	note "compile: PASS $name"
	events=$control_dir/mutant.json
	rc=$(run_oracle "$events" "$control_dir/mutant.console")
	git -C "$work" checkout -- "$src"
	[[ "$(git -C "$work" hash-object -- "$src")" == "$pristine" ]] || exit 1
	if (( rc != 1 )) || ! events_valid "$events" || crashed "$events" || ! jq -e -s --arg oracle "$oracle" --arg assertion "$assertion" '
		any(.[]; .Action == "fail" and .Test == $oracle) and
		any(.[]; .Action == "output" and .Test == $oracle and (.Output | contains($assertion)))
	' "$events" >/dev/null; then
		note "kill: FAILED $name (exit $rc)"
		print -u2 "BROKEN-RUN: mutant exit $rc did not prove the named assertion"
		exit 1
	fi
	note "kill: PASS $name: $oracle: $assertion"
	restored=$control_dir/restored.json
	rc=$(run_oracle "$restored" "$control_dir/restored.console")
	if (( rc != 0 )) || ! oracle_passed "$restored"; then
		note "restored: FAILED $name (exit $rc)"
		print -u2 'error: named oracle did not pass after restoring production source'
		exit 1
	fi
	note "restored: PASS $name: $oracle"
done
cleanup || { print -u2 'error: owned checkout cleanup failed'; exit 1; }
note "KILLED: ${#controls} controls: $oracle"
