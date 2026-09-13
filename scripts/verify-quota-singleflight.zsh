#!/usr/bin/env zsh
# FAC-830: non-vacuity driver for the quota cache single-flight guards.
#
# Two things are proven here, and they are different claims:
#
#   1. REPRODUCTION. The suite is replayed under the exact shuffle seed that
#      failed on main (1789267423461819441), and then repeated a bounded number
#      of times. A test that only passes when it is scheduled first is not
#      deterministic, and this is what would catch that.
#   2. NON-VACUITY. Each guard is removed from the REAL production source one at
#      a time. A mutant counts as KILLED only when it COMPILES, the run exits
#      non-zero, the NAMED killer test emits a failure, and that test's own
#      output carries the NAMED assertion -- all read from `go test -json`. A
#      compile error, a panic, a Go-internal timeout, an external timeout, a
#      skip or an unrelated assertion is NOT a kill and gets its own verdict.
#
# Mutation happens in one ephemeral detached worktree this invocation creates
# and owns, placed outside the evidence directory so a failed cleanup cannot be
# uploaded as an artifact. The invoking checkout is never written to, and this
# script deletes nothing it did not create.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"

for tool in git go timeout jq mktemp rmdir; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

usage_pkg=./pkg/usage/
lock_src=pkg/usage/cache.go
lock_unix_src=pkg/usage/cache_lock_unix.go

# The seed that failed on main. Replaying it is the reproduction; the repeats
# are what turn a one-off pass into evidence of determinism.
shuffle_seed=${VERIFY_SINGLEFLIGHT_SEED:-1789267423461819441}
repeat=${VERIFY_SINGLEFLIGHT_REPEAT:-3}
if [[ "$repeat" != <-> ]] || (( repeat < 1 || repeat > 20 )); then
	print -u2 "warning: ignoring unusable VERIFY_SINGLEFLIGHT_REPEAT, using 3"
	repeat=3
fi

singleflight_run='TestProviderCacheAcrossProcessesSingleFlight|TestProviderCacheContendedLockIsBusyNotAPoll|TestProviderCacheSubprocessHelper'
singleflight_expect='TestProviderCacheAcrossProcessesSingleFlight TestProviderCacheContendedLockIsBusyNotAPoll'

go_timeout=${VERIFY_SINGLEFLIGHT_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 4 )) || (( go_timeout < 60 || go_timeout > 1800 )); then
	print -u2 "warning: ignoring unusable VERIFY_SINGLEFLIGHT_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
worktree_timeout=120
cleanup_timeout=60

report_parent=${VERIFY_SINGLEFLIGHT_REPORT_DIR:-$repo_root/.verify-singleflight-logs}
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
	print -u2 "verify-quota-singleflight: terminated by signal"
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

jq_predicate() {
	local events=$1
	local filter=$2
	jq -e -s "$filter" "$events" >/dev/null 2>&1
}

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
	for name in ${=singleflight_expect}; do
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

# run_seeded replays the suite under the exact failing shuffle seed. The helper
# is a subprocess of the test binary, so the package runs serially (-p 1) and
# the seed governs order within it.
run_seeded() {
	local events=$1
	local console=$2
	local rc=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-shuffle="$shuffle_seed" -run "$singleflight_run" "$usage_pkg" ) >"$events" 2>"$console" || rc=$?
	print -r -- "$rc"
}

run_focused() {
	local selector=$1
	local events=$2
	local console=$3
	local rc=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$usage_pkg" ) >"$events" 2>"$console" || rc=$?
	print -r -- "$rc"
}

compile_check() {
	local log=$1
	local rc=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -p 1 -c -o /dev/null "$usage_pkg" ) >"$log" 2>&1 || rc=$?
	print -r -- "$rc"
}

typeset -A pristine
typeset -a mutated_sources

# count_occurrences counts the LITERAL, not the lines that contain it: two hits
# on one line would read as one and a uniqueness check would pass wrongly.
# Splitting on the needle yields one more field than there are occurrences.
count_occurrences() {
	local haystack=$1
	local needle=$2
	local -a parts
	parts=("${(@ps:$needle:)haystack}")
	print -r -- $(( ${#parts} - 1 ))
}

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
# Both replacements keep every identifier syntactically used, because a mutant
# that does not compile proves nothing.
sep=$'\x1f'
mutations=(
"provider-lock-removed${sep}${lock_src}${sep}	return withCacheFileLock(lockPath, 300*time.Millisecond, fn)${sep}	_ = lockPath; return fn() // MUTANT: cross-process single-flight guard removed${sep}TestProviderCacheAcrossProcessesSingleFlight${sep}want exactly 1"
"busy-lock-loses-its-sentinel${sep}${lock_unix_src}${sep}			return fmt.Errorf(\"%w after %s: %w\", ErrCacheLockBusy, wait, err)${sep}			return fmt.Errorf(\"%w after %s\", err, wait) // MUTANT: a busy lock is indistinguishable from a broken one${sep}TestProviderCacheContendedLockIsBusyNotAPoll${sep}contender failed instead of reporting a busy lock"
)

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
	note "report ${run_dir:t} (under VERIFY_SINGLEFLIGHT_REPORT_DIR, outside the repository)"
fi
for src in "${mutated_sources[@]}"; do
	note "source $src (${pristine[$src]})"
done

failures=0

# Reproduction: the exact failing seed, then bounded repeats. A test that is
# order- or contention-sensitive fails here rather than on someone else's PR.
attempt=0
while (( attempt < repeat )); do
	(( attempt += 1 ))
	seeded_exit=$(run_seeded "$run_dir/seeded-$attempt.json" "$run_dir/seeded-$attempt.err")
	if (( seeded_exit != 0 )) || ! baseline_ok "$run_dir/seeded-$attempt.json"; then
		note "seeded replay $attempt/$repeat FAILED (exit $seeded_exit, seed $shuffle_seed)"
		(( failures += 1 ))
		break
	fi
	note "seeded replay $attempt/$repeat PASS (seed $shuffle_seed)"
done
if (( failures != 0 )); then
	note "RESULT: the suite is not deterministic under the recorded seed"
	exit 1
fi

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

	mutant_exit=$(run_focused "^${killer}\$|^TestProviderCacheSubprocessHelper\$" "$stem.json" "$stem.err")
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

restored_exit=$(run_focused "$singleflight_run" "$run_dir/restored.json" "$run_dir/restored.err")
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
note "RESULT: seed replayed clean $repeat time(s), every control killed its guard, restored baseline passes"
exit 0
