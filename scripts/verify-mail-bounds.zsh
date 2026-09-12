#!/usr/bin/env zsh
# FAC-828: non-vacuity driver for the bounded inbox guards.
#
# Mutates the REAL production source one guard at a time. A mutant counts as
# killed only when BOTH hold, as separate evidence:
#
#   1. it COMPILES, proven by building the test binary, and
#   2. the NAMED killer test fails, read from `go test -json` rather than
#      scraped from console output.
#
# A compile error, a build failure inside the stream, a panic, a timeout, a
# skip or an unrelated test's failure is NOT a kill. Counting any of them is
# how a vacuous control passes.
#
# All mutation happens in one ephemeral detached worktree this invocation
# creates and owns. The invoking checkout is never written to, and this script
# deletes nothing it did not create. It is a REMOTE CI driver: do not run it
# on a workstation under a local-execution hold.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"

for tool in git go timeout jq mktemp; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

source_rel=pkg/mail/bounded.go
cli_rel=cmd/herd/mail_bounds.go
test_pkg=./cmd/herd/
test_run='TestBoundedInbox|TestBoundedRequest'

go_timeout=${VERIFY_BOUNDS_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 4 )) || (( go_timeout < 60 || go_timeout > 1800 )); then
	print -u2 "warning: ignoring unusable VERIFY_BOUNDS_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 180 ))
compile_timeout=$go_timeout
cleanup_timeout=60

# A dirty invoking checkout cannot be pinned: the worktree would be created
# from HEAD while the sources under review live only in the working tree, so
# every result would describe code that is not what is being verified.
if [[ -n "$(git -C "$repo_root" status --porcelain --untracked-files=no)" ]]; then
	print -u2 'error: the invoking checkout has uncommitted tracked changes; commit them so the verified pin is exact'
	exit 1
fi

report_parent=${VERIFY_BOUNDS_REPORT_DIR:-$repo_root/.verify-bounds-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
summary=$run_dir/summary.txt
: >| "$summary"

work=""
work_owned=0
cleanup_failed=0

cleanup() {
	local code=$?
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
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/verify-bounds-XXXXXX")
rmdir -- "$work"
git -C "$repo_root" worktree add --detach --quiet -- "$work" "$pin"
work_owned=1

work_pin=$(git -C "$work" rev-parse HEAD)
[[ "$work_pin" == "$pin" ]] || { print -u2 "error: mutation checkout is at $work_pin, not $pin"; exit 1; }

# Source inventory: every file this driver mutates, hashed before anything
# touches it, so a restore is proven rather than assumed.
typeset -A pristine
for rel in "$source_rel" "$cli_rel"; do
	pristine[$rel]=$(git -C "$work" hash-object -- "$rel")
	[[ -n "${pristine[$rel]}" ]] || { print -u2 "error: cannot hash $rel"; exit 1; }
	note "source $rel (${pristine[$rel]})"
done
note "pin $pin"
if [[ "$run_dir" == "$repo_root"/* ]]; then
	note "report ${run_dir#$repo_root/}"
else
	note "report ${run_dir:t} (under VERIFY_BOUNDS_REPORT_DIR, outside the repository)"
fi

compile_check() {
	local log=$1 exit_code=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -p 1 -c -o /dev/null "$test_pkg" ) >"$log" 2>&1 || exit_code=$?
	print -r -- "$exit_code"
}

run_focused() {
	local selector=$1 events=$2 console=$3 exit_code=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$test_pkg" ) >"$events" 2>"$console" || exit_code=$?
	print -r -- "$exit_code"
}

events_valid() {
	jq -e -s 'type == "array" and length > 0' -- "$1" >/dev/null 2>&1
}

# A whole predicate, never `jq | grep -q`: under pipefail grep exits at its
# first match, jq dies on SIGPIPE, and the pipeline reports 141 even though
# the match succeeded -- so a long panic stream, exactly the failure case,
# would classify clean.
jq_predicate() {
	local filter=$1 events=$2
	shift 2
	local predicate_exit=0
	jq -s -e "$filter" "$@" -- "$events" >/dev/null 2>&1 || predicate_exit=$?
	if (( predicate_exit == 0 )); then return 0; fi
	if (( predicate_exit == 1 )); then return 1; fi
	print -u2 "error: jq could not evaluate the event stream (exit $predicate_exit)"
	return 2
}

stream_broken() {
	jq_predicate 'any(.[]; ((.Output // "") | test("build failed|cannot find package|panic: |test timed out|signal: killed")))' "$1"
}

test_emitted() {
	local events=$1 test_name=$2 action=$3
	jq_predicate 'any(.[]; (.Action == $a)
		and (((.Test // "") == $t) or (((.Test // "") | startswith($t + "/")))))' \
		"$events" --arg t "$test_name" --arg a "$action"
}

# classify maps one mutant run onto the contract. Skips, timeouts, build
# failures and wrong-test failures are each their own verdict.
classify() {
	local run_exit=$1 events=$2 killer=$3 want=$4 probe
	if ! events_valid "$events"; then print -r -- 'INVALID-EVENTS'; return; fi
	if (( run_exit == 0 )); then print -r -- 'SURVIVED'; return; fi
	if (( run_exit != 1 )); then print -r -- "TOOLFAIL(exit $run_exit)"; return; fi

	probe=0; stream_broken "$events" || probe=$?
	case $probe in
		0) print -r -- 'BROKEN-RUN'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac
	probe=0; test_emitted "$events" "$killer" skip || probe=$?
	case $probe in
		0) print -r -- 'SKIPPED'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac
	probe=0; test_emitted "$events" "$killer" fail || probe=$?
	case $probe in
		1) print -r -- 'WRONG-TEST'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac
	# The named test failing is not enough: it must fail on the assertion this
	# mutant was built to break. Without this a test that failed for an
	# unrelated reason would be counted as a kill.
	probe=0
	jq_predicate 'any(.[]; (.Action == "output")
		and (((.Test // "") == $t) or (((.Test // "") | startswith($t + "/"))))
		and (((.Output // "") | contains($w))))' "$events" --arg t "$killer" --arg w "$want" || probe=$?
	case $probe in
		1) print -r -- 'WRONG-ASSERTION'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac
	print -r -- 'KILLED'
}

patch_source() {
	local rel=$1 anchor=$2 replacement=$3 file=$work/$1 found content mutated
	# OCCURRENCES, not matching lines: grep -c reports 1 for a line holding the
	# anchor twice, and the global substitution below would mutate both.
	found=$(grep -F -o -- "$anchor" "$file" | wc -l | tr -d ' ')
	if [[ "$found" != 1 ]]; then
		print -u2 "error: anchor matched ${found:-0} occurrences, want exactly 1 (source drifted): $anchor"
		return 1
	fi
	content=$(<"$file")
	print -r -- "${content//"$anchor"/"$replacement"}" >| "$file"
	mutated=$(git -C "$work" hash-object -- "$rel")
	[[ "$mutated" != "${pristine[$rel]}" ]] || { print -u2 'error: mutation did not change the source'; return 1; }
}

restore_sources() {
	local rel now
	git -C "$work" checkout --quiet -- "$source_rel" "$cli_rel"
	for rel in "$source_rel" "$cli_rel"; do
		now=$(git -C "$work" hash-object -- "$rel")
		[[ "$now" == "${pristine[$rel]}" ]] || { print -u2 "error: restore left $rel at $now, want ${pristine[$rel]}"; return 1; }
	done
}

# Every killer named below, so the baseline can prove each one exists and
# passes before any mutant is allowed to claim it failed.
expected_passes=(
	TestBoundedInboxByteBudgetBindsOnSerializedSize
	TestBoundedInboxRejectsUnusableCursors
	TestBoundedInboxCursorIsBoundToStorage
	TestBoundedInboxRejectsRewoundStorage
	TestBoundedInboxRejectsEmptiedStores
	TestBoundedInboxRejectsUnorderedStorage
	TestBoundedInboxRefusesDuplicateIdentities
	TestBoundedInboxRefusesNonPositiveIdentities
	TestBoundedInboxDetectsReplacementButToleratesAck
	TestBoundedInboxQuarantinesMalformedRowPastAFullPage
	TestBoundedInboxOversizedRecordFailsInsteadOfStalling
	TestBoundedInboxReportsFeedbackErrorBehindAFullControlPage
	TestBoundedInboxBytesAccountForBothSources
)

baseline_exit=$(run_focused "$test_run" "$run_dir/baseline.json" "$run_dir/baseline.err")
if (( baseline_exit != 0 )); then
	note "baseline FAILED (exit $baseline_exit) - the suite must pass before any mutant means anything"
	exit 1
fi
# Exit 0 alone can be vacuous: a selector that matched nothing also exits 0.
if ! events_valid "$run_dir/baseline.json"; then
	note 'baseline produced an unreadable event stream'
	exit 1
fi
# Every killer this driver relies on must be PRESENT and PASSING in the
# baseline. "some test passed" would let a silently excluded subset -- a
# renamed test, a build-tagged-out file -- read as a healthy baseline while
# the mutant it anchors could never have run.
baseline_missing=0
for record in "${expected_passes[@]}"; do
	probe=0; test_emitted "$run_dir/baseline.json" "$record" pass || probe=$?
	if (( probe != 0 )); then
		note "baseline is missing a passing $record (probe $probe)"
		baseline_missing=1
	fi
done
(( baseline_missing == 0 )) || exit 1
note 'baseline PASS (every anchored killer present and passing)'

sep=$'\x1f'
# id | file | anchor | replacement | killer test | required assertion text
mutations=(
"limit-not-enforced${sep}${source_rel}${sep}		if len(page.Envelopes) >= opts.Limit || page.Bytes+size > opts.MaxBytes {${sep}		if false { // MUTANT: limit and byte budget ignored${sep}TestBoundedInboxByteBudgetBindsOnSerializedSize${sep}near-boundary page"
"cursor-recipient-unbound${sep}${source_rel}${sep}	if string(decoded) != recipient {${sep}	if false { // MUTANT: cursor recipient binding dropped${sep}TestBoundedInboxRejectsUnusableCursors${sep}was accepted"
"cursor-storage-unbound${sep}${source_rel}${sep}	if parts[2] != source {${sep}	if false { // MUTANT: cursor storage binding dropped${sep}TestBoundedInboxCursorIsBoundToStorage${sep}different mailbox/feedback root was accepted"
"rewound-storage-accepted${sep}${source_rel}${sep}	if sawAny && cur.Control > maxSeen {${sep}	if false { // MUTANT: cursor ahead of the store accepted${sep}TestBoundedInboxRejectsRewoundStorage${sep}cursor ahead of the store was accepted"
"unordered-storage-accepted${sep}${source_rel}${sep}		if sawAny && env.Sequence < maxSeen {${sep}		if false { // MUTANT: unordered store paged anyway${sep}TestBoundedInboxRejectsUnorderedStorage${sep}unordered store was paged anyway"
"late-scan-abandoned${sep}${source_rel}${sep}		if page.Truncated {
			// Page is already full. Keep validating, retain nothing, and do
			// NOT advance the cursor over this record.
			continue
		}${sep}		if page.Truncated {
			break // MUTANT: stop scanning once the page is full
		}${sep}TestBoundedInboxQuarantinesMalformedRowPastAFullPage${sep}past the page boundary was never quarantined"
"oversized-record-skipped${sep}${source_rel}${sep}		if size > opts.MaxBytes {${sep}		if false { // MUTANT: oversized record silently skipped${sep}TestBoundedInboxOversizedRecordFailsInsteadOfStalling${sep}oversized record produced a page instead of an error"
"feedback-error-swallowed${sep}${cli_rel}${sep}			return nil, highest, false, fmt.Errorf(\"mail: unparseable feedback record: %w\", err)${sep}			continue // MUTANT: unreadable feedback silently dropped${sep}TestBoundedInboxReportsFeedbackErrorBehindAFullControlPage${sep}full control page hid an unreadable feedback store"
"feedback-truncation-before-existence${sep}${cli_rel}${sep}	if page.Truncated || controlErr != nil {
		remainingLimit, remainingBytes = 0, 0
	}${sep}	if page.Truncated || controlErr != nil {
		return out, controlErr // MUTANT: return before validating feedback
	}${sep}TestBoundedInboxReportsFeedbackErrorBehindAFullControlPage${sep}full control page hid an unreadable feedback store"
"feedback-bytes-uncounted${sep}${cli_rel}${sep}		out.RetainedBytes += size${sep}		_ = size // MUTANT: feedback bytes not counted${sep}TestBoundedInboxBytesAccountForBothSources${sep}feedback bytes were not counted"
)

failures=0
index=0
for record in "${mutations[@]}"; do
	(( index += 1 ))
	fields=("${(@ps:$sep:)record}")
	id=$fields[1]; rel=$fields[2]; anchor=$fields[3]; replacement=$fields[4]; killer=$fields[5]; want=$fields[6]
	stem=$run_dir/$(printf 'm%02d-%s' "$index" "$id")

	patch_source "$rel" "$anchor" "$replacement"

	compile_exit=$(compile_check "$stem.compile.log")
	if (( compile_exit != 0 )); then
		note "$id: DID-NOT-COMPILE (exit $compile_exit) - refused as a kill"
		(( failures += 1 ))
		restore_sources
		continue
	fi

	run_exit=$(run_focused "$killer" "$stem.json" "$stem.err")
	verdict=$(classify "$run_exit" "$stem.json" "$killer" "$want")
	note "$id: $verdict (killer $killer, exit $run_exit)"
	[[ "$verdict" == KILLED ]] || (( failures += 1 ))

	restore_sources
	restored_exit=$(run_focused "$killer" "$stem.restored.json" "$stem.restored.err")
	# Exit 0 alone is not a restored baseline: a selector that matched nothing
	# also exits 0. The killer must be present and PASSING again.
	restored_ok=0
	if (( restored_exit == 0 )) && events_valid "$stem.restored.json"; then
		probe=0; test_emitted "$stem.restored.json" "$killer" pass || probe=$?
		(( probe == 0 )) && restored_ok=1
	fi
	if (( ! restored_ok )); then
		note "$id: restore did not return $killer to PASS (exit $restored_exit)"
		(( failures += 1 ))
	fi
done

note "mutants=$index failures=$failures"
(( failures == 0 )) || exit 1
note 'all bounded-inbox controls killed their named assertions'
