#!/usr/bin/env zsh
# FAC-839: hosted-only causal controls for explicit task-source enrollment.
set -euo pipefail

repo_root=$(git -C "${0:A:h}/.." rev-parse --show-toplevel)
for tool in git go timeout jq mktemp rmdir rm; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done
pin=$(git -C "$repo_root" rev-parse HEAD)
report_parent=${VERIFY_TASK_SOURCE_REPORT_DIR:-$repo_root/.verify-task-source-logs}
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
			timeout -k 10s 10s rm -rf -- "$work" || return 1
		fi
		owned=0
	fi
	[[ ! -d "$work_parent" ]] || rmdir -- "$work_parent"
}
on_exit() {
	local rc=$?
	trap - EXIT
	cleanup || { print -u2 'error: owned checkout cleanup failed'; exit 1; }
	exit "$rc"
}
trap on_exit EXIT
trap 'exit 143' INT TERM HUP
owned=1
timeout -k 10s 120s git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null 2>&1
print -r -- "pin: $pin" > "$run_dir/summary.txt"
note() { print -r -- "$@" | tee -a "$run_dir/summary.txt"; }
run_suite() {
	local pattern=$1 stem=$2 rc=0
	( cd "$work" && timeout -k 10s 420s go test -json -count=1 -p 1 -parallel 1 -timeout 300s -run "$pattern" ./cmd/herd/ ) > "$stem.json" 2> "$stem.console" || rc=$?
	print -r -- "$rc"
}
healthy_events() {
	jq -e -s 'length > 0 and all(.[]; type == "object") and
		(all(.[]; .Action != "skip")) and
		(all(.[]; .Action != "output" or (.Output | test("^panic: |^fatal error: |test timed out after |\\[build failed\\]|^# ") | not)))' "$1" >/dev/null
}
positive() {
	local phase=$1 rc
	rc=$(run_suite '^TestTaskSource' "$run_dir/$phase")
	(( rc == 0 )) && healthy_events "$run_dir/$phase.json" || return 1
	jq -e -s '
		[.[] | select(.Action == "pass") | .Test] as $passed |
		["TestTaskSourceEnrollmentRetiresNamedTask", "TestTaskSourceEnrollmentProtectsLiveHome",
		 "TestTaskSourceEnrollmentRefusesUnprovenAuthority", "TestTaskSourceActRefusesDrift", "TestTaskSourceHardHomes"] |
		all(.[]; . as $name | $passed | index($name) != null)
	' "$run_dir/$phase.json" >/dev/null
	note "$phase: PASS all five task-source suites"
}

# Exact source + oracle + assertion. Each mutant must compile and fail for
# its intended behavioral reason, then all suites must pass after restoration.
sep=$'\x1f'
controls=(
"resident-name-only${sep}cmd/herd/worktreereap.go${sep}case isResidentHome(e.Branch, e.Path) && r.taskSource == nil:${sep}case isResidentHome(e.Branch, e.Path):${sep}TestTaskSourceEnrollmentRetiresNamedTask${sep}enrolled named task was kept"
"live-home-bypass${sep}cmd/herd/worktree_task_source.go${sep}if home == path || strings.HasPrefix(home, path+string(filepath.Separator)) {${sep}if false && (home == path || strings.HasPrefix(home, path+string(filepath.Separator))) {${sep}TestTaskSourceEnrollmentProtectsLiveHome${sep}live resident home was not refused by independent home guard"
)
for control in "${controls[@]}"; do
	fields=("${(@ps:$sep:)control}")
	(( ${#fields} == 6 )) || exit 1
	content=$(git -C "$repo_root" show "$pin:$fields[2]")
	anchor=$fields[3]
	parts=("${(@ps:$anchor:)content}")
	(( ${#parts} == 2 )) || { print -u2 'error: mutation anchor must occur exactly once'; exit 1; }
done
positive baseline
for control in "${controls[@]}"; do
	fields=("${(@ps:$sep:)control}")
	name=$fields[1] src=$fields[2] anchor=$fields[3] replacement=$fields[4] oracle=$fields[5] assertion=$fields[6]
	control_dir=$run_dir/$name
	mkdir -p -- "$control_dir"
	pristine=$(git -C "$repo_root" rev-parse "$pin:$src")
	content=$(git -C "$repo_root" show "$pin:$src")
	print -r -- "${content//"$anchor"/"$replacement"}" > "$work/$src"
	[[ "$(git -C "$work" hash-object -- "$src")" != "$pristine" ]] || exit 1
	git -C "$work" diff -- "$src" > "$control_dir/mutant.diff"
	( cd "$work" && timeout -k 10s 420s go test -p 1 -c -o /dev/null ./cmd/herd/ ) > "$control_dir/mutant.compile" 2>&1 || {
		note "compile: FAILED $name"; exit 1
	}
	note "compile: PASS $name"
	rc=$(run_suite "^${oracle}\$" "$control_dir/mutant")
	git -C "$work" checkout -- "$src"
	[[ "$(git -C "$work" hash-object -- "$src")" == "$pristine" ]] || exit 1
	if (( rc != 1 )) || ! healthy_events "$control_dir/mutant.json" || ! jq -e -s --arg oracle "$oracle" --arg assertion "$assertion" '
		any(.[]; .Action == "fail" and .Test == $oracle) and
		any(.[]; .Action == "output" and .Test == $oracle and (.Output | contains($assertion)))
	' "$control_dir/mutant.json" >/dev/null; then
		note "kill: FAILED $name (exit $rc)"; exit 1
	fi
	note "kill: PASS $name: $oracle: $assertion"
	positive "restored-$name"
done
cleanup
note 'KILLED: 2 task-source controls; all pristine suites restored'
