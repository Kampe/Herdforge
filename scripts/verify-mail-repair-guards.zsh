#!/usr/bin/env zsh
# FAC-824: non-vacuity driver for the operator mailbox-repair guards.
#
# Runs the focused repair suite, then mutates the REAL production source one
# guard at a time. A mutant counts as killed only when BOTH hold, as separate
# evidence:
#
#   1. it COMPILES, proven by building the test binary, and
#   2. the NAMED killer test fails with the NAMED assertion text, read from
#      `go test -json` rather than scraped from console output.
#
# A compile error, a panic, a timeout, a skip, a no-match, or an unrelated
# assertion is not a kill. Counting any of them is how a vacuous control passes.
#
# A refusal test on its own proves nothing either: the suite must pass before
# any mutant is applied, and must pass again after the exact pristine source is
# restored, or the controls are measuring a broken tree rather than a guard.
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

source_rel=pkg/mail/repair.go
test_pkg=./pkg/mail/
test_run='TestRepair|TestScanJSONObject|TestDuplicateKeyRow|TestAppendLineWrite|TestOrdinarySendRefuses|TestNextSequenceValue|TestRowsWithReadableIdentity|TestWellFormedUnrelatedRows|TestBlankLines|TestUnrelatedDuplicateKey|TestTargetRefusals'

# Finite, explicit, and bounded at both ends before any arithmetic: an absurd
# or overflowing override must be rejected, not added to.
go_timeout=${VERIFY_MAIL_REPAIR_GO_TIMEOUT:-180}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 3 )) || (( go_timeout < 30 || go_timeout > 900 )); then
	print -u2 "warning: ignoring unusable VERIFY_MAIL_REPAIR_GO_TIMEOUT, using 180s"
	go_timeout=180
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
cleanup_timeout=60

# Reports are additive. This script creates ONE subdirectory it owns and never
# deletes a directory chosen by a caller: a path being well formed is not
# permission to destroy what is in it.
report_parent=${VERIFY_MAIL_REPAIR_REPORT_DIR:-$repo_root/.verify-mail-repair-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
summary=$run_dir/summary.txt
: >| "$summary"

work=""
work_owned=0
work_add_started=0
cleanup_failed=0

cleanup() {
	local code=$?
	# RESIDUAL, stated rather than papered over: ownership is only claimed AFTER
	# `git worktree add` returns success. If that command fails part way it may
	# already have created the directory, administrative metadata under
	# .git/worktrees, or both, and this script does not own or remove any of it.
	#
	# That is deliberate. A partial add is exactly the case where the script
	# cannot tell what it made from what was already there, and deleting under
	# that uncertainty is how a cleanup routine destroys someone else's
	# checkout. It reports the path and leaves it; an operator or `git worktree
	# prune` resolves it with more context than this script has.
	if (( work_add_started )) && ! (( work_owned )) && [[ -n "$work" ]]; then
		print -u2 "warning: 'git worktree add' did not complete; a PARTIAL checkout and/or administrative metadata may exist at $work and is deliberately left untouched"
		print -r -- 'cleanup SKIPPED: worktree creation did not complete; a partial checkout may remain (see stderr for its path). Nothing was deleted.' >> "$summary"
	fi
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
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/verify-mail-repair-XXXXXX")
# mktemp made the directory; `git worktree add` needs the path absent. rmdir
# refuses a non-empty directory, which is the guard we want.
rmdir -- "$work"
work_add_started=1
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

# events_valid proves the whole stream parses BEFORE any predicate reads it. A
# parse failure and a genuine no-match are otherwise the same status, which
# would let a truncated stream read as a clean result.
events_valid() {
	jq -e -s 'type == "array" and length > 0' -- "$1" >/dev/null 2>&1
}

# jq_predicate runs one complete jq test over the validated stream and maps its
# status onto 0 match / 1 no match / 2 evaluation failed.
#
# It is a whole predicate rather than `jq | grep -q` on purpose. Under
# `set -o pipefail` grep exits at its FIRST match, jq dies on SIGPIPE, and the
# pipeline reports 141 even though the match succeeded -- so a long panic
# stream, which is exactly the failure condition, would have been classified
# clean. A predicate consumes all of its input and cannot invert that way.
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

# stream_broken looks for build, panic, timeout and tool failures across the
# ENTIRE stream, not just the killer's own events: an expected assertion
# followed by a crash somewhere else is not a successful control. Oniguruma
# anchors ^ at line starts, so a marker inside a multi-line Output still hits.
stream_broken() {
	jq_predicate 'any(.[]; (.Action == "output")
		and (((.Output // "") | test("panic: |test timed out|\\[build failed\\]|^# |^signal: |fatal error: "))))' "$1"
}

# test_emitted reports whether the named test or one of its subtests produced
# an event of this action, optionally carrying text. Literal substring, so no
# assertion has to be regex-escaped.
test_emitted() {
	local events=$1 test_name=$2 action=$3 want=${4-}
	if [[ -z "$want" ]]; then
		jq_predicate 'any(.[]; (.Action == $a)
			and (((.Test // "") == $t) or (((.Test // "") | startswith($t + "/")))))' \
			"$events" --arg t "$test_name" --arg a "$action"
		return $?
	fi
	jq_predicate 'any(.[]; (.Action == $a)
		and (((.Test // "") == $t) or (((.Test // "") | startswith($t + "/"))))
		and (((.Output // "") | contains($w))))' \
		"$events" --arg t "$test_name" --arg a "$action" --arg w "$want"
}

# read_source returns the file's contents, or fails loudly. An unreadable
# source must never be indistinguishable from an empty one: `|| true` on a read
# turns "I could not look" into "I looked and saw nothing".
read_source() {
	local file=$1 content
	[[ -r "$file" ]] || { print -u2 "error: cannot read $file"; return 2; }
	content=$(<"$file") || { print -u2 "error: failed reading $file"; return 2; }
	[[ -n "$content" ]] || { print -u2 "error: $file is empty"; return 2; }
	print -r -- "$content"
}

# count_literal counts LITERAL OCCURRENCES, not matching lines.
#
# grep -F -c counts lines, so two copies of an anchor on one line report 1 —
# and ${content//anchor/replacement} would then replace BOTH while the guard
# said the anchor was unique. Occurrences and lines are different numbers and
# only one of them is the safety property.
count_literal() {
	local anchor=$1 content=$2 rest=$2 n=0
	[[ -n "$anchor" ]] || { print -u2 'error: empty anchor'; return 2; }
	while [[ "$rest" == *"$anchor"* ]]; do
		(( n += 1 ))
		rest=${rest#*"$anchor"}
	done
	print -r -- "$n"
}

patch_source() {
	local anchor=$1 replacement=$2 file=$work/$source_rel content found mutated
	content=$(read_source "$file") || return 2
	found=$(count_literal "$anchor" "$content") || return 2
	if [[ "$found" != 1 ]]; then
		print -u2 "error: anchor occurs $found time(s), want exactly 1 (source drifted): $anchor"
		return 1
	fi
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
# passed. Only an exact go test exit 1, over a stream with no tool failure
# anywhere in it, naming the killer and carrying the expected text, is a kill.
classify_run() {
	local run_exit=$1 events=$2 killer=$3 want=$4 probe
	if ! events_valid "$events"; then print -r -- 'INVALID-EVENTS'; return; fi
	if (( run_exit == 0 )); then print -r -- 'SURVIVED'; return; fi
	if (( run_exit != 1 )); then print -r -- "TOOLFAIL(exit $run_exit)"; return; fi

	# probe is reset before every predicate: a leftover status from the previous
	# question would answer the next one.
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

	probe=0; test_emitted "$events" "$killer" output "$want" || probe=$?
	case $probe in
		1) print -r -- 'WRONG-ASSERTION'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac

	print -r -- 'KILLED'
}

# id | anchor | replacement | killer test | required assertion text
#
# One mutant per guard the card names, and no more: stale/conflicting identity,
# malformed unrelated rows, privileged records, concurrent mailbox change, and
# the durable prepare / recovery / completion-error boundaries.
sep=$'\x1f'
mutations=(
"fingerprint-cas-removed${sep}		if fp := strings.TrimSpace(req.Fingerprint); fp != \"\" && !strings.EqualFold(fp, originalHash) {${sep}		if fp := strings.TrimSpace(req.Fingerprint); false && fp != \"\" { // MUTANT: compare-and-swap removed${sep}TestRepairRefusesStaleFingerprint${sep}stale fingerprint must refuse"
"quarantine-identity-removed${sep}		if err := m.checkQuarantineIdentity(req.ID, originalHash); err != nil {${sep}		if err := error(nil); err != nil { // MUTANT: concurrent-change check removed${sep}TestRepairRefusesConflictingQuarantinedOriginals${sep}conflicting quarantined originals must refuse"
"unrelated-corruption-unrecorded${sep}			otherBad = append(otherBad, fmt.Sprintf(\"line %d: %s\", i+1, why))${sep}			_ = why // MUTANT: unrelated corruption never recorded${sep}TestRepairRefusesWhenAnotherRowIsMalformed${sep}an unrelated malformed row did not refuse the repair"
"duplicate-identity-ignored${sep}				if containsString(scan.IDs, req.ID) {${sep}				if false && containsString(scan.IDs, req.ID) { // MUTANT: conflicting identity ignored${sep}TestRepairRefusesDuplicateKeyRowEvenWhenARepairableRowExists${sep}a duplicate-key row carrying the target id did not block the repair"
"privileged-refusal-removed${sep}			return nil, \"\", time.Time{}, fmt.Errorf(\"%w: row carries %q\", ErrRepairPrivileged, key)${sep}			_ = key // MUTANT: privileged refusal removed${sep}TestRepairRefusesPrivilegedSignedControlMessage${sep}a signed control message must never be silently rewritten"
"prepare-phase-mislabelled${sep}		plan.Phase = RepairPhasePrepare${sep}		plan.Phase = RepairPhaseResult // MUTANT: pre-mutation record not labelled prepare${sep}TestRepairAuditRecordsPrepareBeforeResult${sep}first record is not a prepare record"
"readback-verification-removed${sep}		if err := m.verifyRepairedMailbox(expected, repaired); err != nil {${sep}		if err := error(nil); err != nil { // MUTANT: durable readback removed${sep}TestRepairFailsClosedWhenReadbackMismatches${sep}a durable row that differs from the repaired row must fail closed"
"completion-error-swallowed${sep}			return fmt.Errorf(\"%w: %v\", ErrRepairCompletionUnrecorded, err)${sep}			return nil // MUTANT: completion error swallowed, repair reports success${sep}TestRepairFailsWhenCompletionRecordCannotBeWritten${sep}an unrecorded completion must fail clearly"
)

note "pin $pin"
note "source $source_rel ($pristine)"
# The artifact records WHICH invocation, never where on the host it lives.
if [[ "$run_dir" == "$repo_root"/* ]]; then
	note "report ${run_dir#$repo_root/}"
else
	note "report ${run_dir:t} (under VERIFY_MAIL_REPAIR_REPORT_DIR, outside the repository)"
fi

# required_tests is every identity a run must PROVE it executed: each mutant's
# killer, plus the positive controls that show a repair still works.
#
# A count is not an identity. Eight unrelated repair tests passing says nothing
# about whether THIS killer ran, and a required test that was skipped or
# renamed away would leave the count intact while the control it anchors
# silently stopped existing. The selector already knows these names, so the
# baseline checks for them by name.
required_tests=(
	# killers, one per mutant below
	TestRepairRefusesStaleFingerprint
	TestRepairRefusesConflictingQuarantinedOriginals
	TestRepairRefusesWhenAnotherRowIsMalformed
	TestRepairRefusesDuplicateKeyRowEvenWhenARepairableRowExists
	TestRepairRefusesPrivilegedSignedControlMessage
	TestRepairAuditRecordsPrepareBeforeResult
	TestRepairFailsClosedWhenReadbackMismatches
	TestRepairFailsWhenCompletionRecordCannotBeWritten
	# positive controls: a suite of refusals alone cannot show a repair works
	TestRepairNormalizesLegacyOffsetAndAssignsSequence
	TestRepairedRowIsVisibleToDedupeScan
	TestRepairAppliesWhenTheWriterIsFaithful
	TestWellFormedUnrelatedRowsAndBodyMentionsStillRepair
	TestAppendLineWriteSurvivesAFailedSync
)

# assert_required_identities proves, by name, that every required test reported
# a top-level PASS and that none of them (or their subtests) was skipped.
assert_required_identities() {
	local events=$1 label=$2 name missing=0 probe
	if ! events_valid "$events"; then
		note "$label: event stream did not parse - no identity can be read from it"
		return 1
	fi
	for name in "${required_tests[@]}"; do
		probe=0; test_emitted "$events" "$name" skip || probe=$?
		case $probe in
			0) note "$label: required test $name was SKIPPED - a skipped control is not a control"; (( missing += 1 )); continue ;;
			2) note "$label: could not read events while checking $name"; return 1 ;;
		esac
		# Exact top-level pass, not a subtest and not a prefix match.
		probe=0
		jq_predicate 'any(.[]; .Action == "pass" and ((.Test // "") == $t))' \
			"$events" --arg t "$name" || probe=$?
		case $probe in
			1) note "$label: required test $name did not report a top-level PASS"; (( missing += 1 )) ;;
			2) note "$label: could not read events while checking $name"; return 1 ;;
		esac
	done
	if (( missing > 0 )); then
		note "$label: $missing of ${#required_tests[@]} required identities were absent or skipped"
		return 1
	fi
	note "$label: all ${#required_tests[@]} required identities passed"
	return 0
}

baseline_exit=$(run_focused "$test_run" "$run_dir/baseline.json" "$run_dir/baseline.err")
if (( baseline_exit != 0 )); then
	note "baseline FAILED (exit $baseline_exit) - the suite must pass before any mutant means anything"
	exit 1
fi
assert_required_identities "$run_dir/baseline.json" 'baseline' || exit 1
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
# The restored run must prove the SAME identities, not merely the same count:
# it is the evidence that the mutants were undone and the guards are back.
assert_required_identities "$run_dir/restored.json" 'restored baseline' || exit 1
note 'restored baseline PASS'

if (( failures > 0 )); then
	note "$failures of ${#mutations[@]} controls did not kill their mutant"
	exit 1
fi
note "all ${#mutations[@]} controls compiled and died on their named assertion"
