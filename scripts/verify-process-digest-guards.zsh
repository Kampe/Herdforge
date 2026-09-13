#!/usr/bin/env zsh
# FAC-36: non-vacuity driver for the `herd process` read-path guards.
#
# A negative assertion proves nothing until something has been shown to break
# it. This runs the focused suites, then mutates the REAL production source one
# guard at a time. A mutant counts as killed only when BOTH hold, as separate
# evidence:
#
#   1. it COMPILES, proven by building the test binary, and
#   2. the NAMED killer test fails with the NAMED assertion text, read from
#      `go test -json` rather than scraped from console output.
#
# A compile error is BROKEN-RUN, not a kill: a mutant that does not build never
# reached the assertion, so it says nothing about the guard. A panic, a
# timeout, a skip, a no-match, or an unrelated assertion is not a kill either.
# Counting any of them is how a vacuous control passes.
#
# The suites must pass before any mutant is applied, and must pass again after
# the exact pristine source is restored, or the controls are measuring a broken
# tree rather than a guard.
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

# Finite, explicit, and bounded at both ends before any arithmetic: an absurd
# or overflowing override must be rejected, not added to.
go_timeout=${VERIFY_PROCESS_DIGEST_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 3 )) || (( go_timeout < 60 || go_timeout > 900 )); then
	print -u2 "warning: ignoring unusable VERIFY_PROCESS_DIGEST_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
cleanup_timeout=60

# Reports are additive. This script creates ONE subdirectory it owns and never
# deletes a directory chosen by a caller: a path being well formed is not
# permission to destroy what is in it.
report_parent=${VERIFY_PROCESS_DIGEST_REPORT_DIR:-$repo_root/.verify-process-digest-logs}
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
	# A partial add is exactly the case where the script cannot tell what it
	# made from what was already there, and deleting under that uncertainty is
	# how a cleanup routine destroys someone else's checkout.
	if (( work_add_started )) && ! (( work_owned )) && [[ -n "$work" ]]; then
		print -u2 "warning: 'git worktree add' did not complete; a PARTIAL checkout and/or administrative metadata may exist at $work and is deliberately left untouched"
		print -r -- 'cleanup SKIPPED: worktree creation did not complete; a partial checkout may remain (see stderr for its path). Nothing was deleted.' >> "$summary"
	fi
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
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/verify-process-digest-XXXXXX")
# mktemp made the directory; `git worktree add` needs the path absent. rmdir
# refuses a non-empty directory, which is the guard we want.
rmdir -- "$work"
work_add_started=1
git -C "$repo_root" worktree add --detach --quiet -- "$work" "$pin"
work_owned=1

work_pin=$(git -C "$work" rev-parse HEAD)
[[ "$work_pin" == "$pin" ]] || { print -u2 "error: mutation checkout is at $work_pin, not $pin"; exit 1; }

sep=$'\x1f'

# ---------------------------------------------------------------------------
# Sources under mutation. Each is hashed pristine up front so a restore can be
# proven byte-for-byte rather than assumed.
# ---------------------------------------------------------------------------
sources=(
	cmd/herd/process_digest.go
	pkg/herdr/read_bounded.go
)
typeset -A pristine
for rel in "${sources[@]}"; do
	h=$(git -C "$work" hash-object -- "$rel") || { print -u2 "error: cannot hash $rel"; exit 1; }
	[[ -n "$h" ]] || { print -u2 "error: empty hash for $rel"; exit 1; }
	pristine[$rel]=$h
done

# ---------------------------------------------------------------------------
# Focused suites. These deliberately EXCLUDE the subprocess CLI fixtures: they
# build and run the whole binary, which this driver would then pay for on every
# mutant. Their coverage belongs to the ordinary test job; this job exists to
# prove the assertions below are not vacuous.
# ---------------------------------------------------------------------------
suites=(
"./cmd/herd/${sep}^(TestProcessDigestReportsEveryRealPaneIdentity|TestProcessDigestCanceledSweepStopsAndReportsPartial|TestProcessDigestCapsPaneTextBeforeClassification|TestProcessDigestVerifiedReadStaysComplete|TestProcessDigestEmptyFleetSucceedsWithZeroItems|TestProcessDigestLiteralErrorJSONFromAPaneIsAgentContent|TestProcessDigestUnverifiedRosterIsNeverACleanFleet|TestProcessDigestVerifiedEmptyRosterStaysClean)\$${sep}TestProcessDigestReportsEveryRealPaneIdentity TestProcessDigestCanceledSweepStopsAndReportsPartial TestProcessDigestCapsPaneTextBeforeClassification TestProcessDigestVerifiedReadStaysComplete TestProcessDigestEmptyFleetSucceedsWithZeroItems TestProcessDigestLiteralErrorJSONFromAPaneIsAgentContent TestProcessDigestUnverifiedRosterIsNeverACleanFleet TestProcessDigestVerifiedEmptyRosterStaysClean"
"./pkg/herdr/${sep}^(TestCappedWriterBoundsAndStopsOnce|TestDecodeHerdrTransportFailsClosedOnStructuredReplies|TestPaneReadContextAcceptsKnownResultBodies|TestPaneReadContextPreservesLiteralErrorJSONInsideAResult|TestRunHerdrReadContextReadsAHealthyChild|TestPaneReadContextMarksUnidentifiedSuccessUnverified|TestPaneReadContextVerifiesIdentifiedSuccess|TestAgentListVerifiedContextRefusesToVerifyUnidentifiedRosters)\$${sep}TestCappedWriterBoundsAndStopsOnce TestDecodeHerdrTransportFailsClosedOnStructuredReplies TestPaneReadContextAcceptsKnownResultBodies TestPaneReadContextPreservesLiteralErrorJSONInsideAResult TestRunHerdrReadContextReadsAHealthyChild TestPaneReadContextMarksUnidentifiedSuccessUnverified TestPaneReadContextVerifiesIdentifiedSuccess TestAgentListVerifiedContextRefusesToVerifyUnidentifiedRosters"
)

# ---------------------------------------------------------------------------
# Controls. Six independent guards, each the subject of a review finding.
#
# What each one ACTUALLY proves, stated at its real strength:
#
#   real-roster-wiring     the digest reads the LIVE roster, not a demo record
#                          or an empty result that still exits 0
#   sweep-context-checked-between-panes
#                          the sweep consults its context BETWEEN panes. That
#                          is the whole claim. It does NOT prove the same
#                          context reaches each pane read, because the killer
#                          is an in-memory helper test with the transport
#                          stubbed out. The end-to-end claim -- one shared
#                          deadline bounding the roster read and every pane
#                          read of a real child -- is carried by
#                          TestProcessCLIHungPanesStopAtTheSweepDeadline, a
#                          subprocess fixture that this driver deliberately
#                          excludes and that the ordinary CI test job runs.
#                          That test remains required; this control is not a
#                          substitute for it.
#   finite-transport-bytes reads are capped at the process boundary
#   error-envelope-closed  a transport error envelope fails closed
#   native-identity-required
#                          a success envelope WITHOUT the native response
#                          identity the installed 0.9.0 contract requires is
#                          never reported as a VERIFIED read. This is the PR839
#                          defect: any non-null result was accepted outright,
#                          so an id-less {"result":{"agents":[]}} produced a
#                          clean, non-partial, zero-agent digest that exited 0.
#                          Scope, stated honestly: the control proves the
#                          identity gate is load bearing. It does NOT pin the
#                          concrete id type, because this repository holds no
#                          recorded native herdr response to read one from.
#   native-identity-required
#                          a success envelope WITHOUT the native identity the
#                          installed 0.9.0 contract requires is never reported
#                          as a verified read. This is the PR839 defect: an
#                          id-less {"result":{"agents":[]}} became a clean,
#                          non-partial, zero-agent digest that exited 0.
#   partial-truncation     a tail this sweep cut makes the digest incomplete
#
# id | source | test package | anchor | replacement | killer | required assertion
# ---------------------------------------------------------------------------
mutations=(
"real-roster-wiring${sep}cmd/herd/process_digest.go${sep}./cmd/herd/${sep}	agents, rosterVerified, err := processAgentList(ctx)${sep}	agents, rosterVerified, err := []herdr.AgentEntry(nil), true, error(nil) // MUTANT: empty result instead of the live roster${sep}TestProcessDigestReportsEveryRealPaneIdentity${sep}want 3 items, got 0"
"sweep-context-checked-between-panes${sep}cmd/herd/process_digest.go${sep}./cmd/herd/${sep}		if ctxErr := ctx.Err(); ctxErr != nil {${sep}		if ctxErr := error(nil); ctxErr != nil { // MUTANT: sweep context ignored between panes${sep}TestProcessDigestCanceledSweepStopsAndReportsPartial${sep}a canceled sweep still issued"
"partial-truncation${sep}cmd/herd/process_digest.go${sep}./cmd/herd/${sep}				result.Partial = true
				note := fmt.Sprintf(\"%s: pane tail truncated to %d bytes by this sweep\", agent.Name, limits.MaxTailBytes)${sep}				note := fmt.Sprintf(\"%s: pane tail truncated to %d bytes by this sweep\", agent.Name, limits.MaxTailBytes) // MUTANT: a cut tail no longer makes the sweep partial${sep}TestProcessDigestCapsPaneTextBeforeClassification${sep}a sweep-truncated tail left the digest claiming to be complete"
"finite-transport-bytes${sep}pkg/herdr/read_bounded.go${sep}./pkg/herdr/${sep}	remaining := w.limit - w.buf.Len()${sep}	remaining := len(p) // MUTANT: byte bound removed, every write is accepted${sep}TestCappedWriterBoundsAndStopsOnce${sep}bytes past a limit of 8"
"error-envelope-closed${sep}pkg/herdr/read_bounded.go${sep}./pkg/herdr/${sep}	case hasError:
		return transportReply{Envelope: true, Identified: identified},
			fmt.Errorf(\"%w: %s\", ErrReadTransportEnvelope, strings.TrimSpace(string(envelope.Error)))${sep}	case hasError:
		return transportReply{Result: envelope.Result, Envelope: true, Identified: identified}, nil // MUTANT: error envelope accepted as a result${sep}TestDecodeHerdrTransportFailsClosedOnStructuredReplies${sep}want herdr read: transport reported an error"
"native-identity-required${sep}pkg/herdr/read_bounded.go${sep}./pkg/herdr/${sep}	identified := validIdentity(envelope.ID)${sep}	identified := true // MUTANT: native response identity no longer required for a verified success${sep}TestPaneReadContextMarksUnidentifiedSuccessUnverified${sep}a success envelope with no valid native identity was reported as a VERIFIED pane read"
)

# compile_check proves the mutant builds. Its result is kept separately from
# the assertion evidence, because "the build broke" and "the test saw the
# guard" are different claims.
compile_check() {
	local pkg=$1 log=$2 exit_code=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -p 1 -c -o /dev/null "$pkg" ) >"$log" 2>&1 || exit_code=$?
	print -r -- "$exit_code"
}

# run_focused executes one -run selection serially and records machine-readable
# events. Mutants run ONLY their anchored killer and its subtests, so an
# unrelated test's failure or timeout cannot contaminate the verdict.
run_focused() {
	local pkg=$1 selector=$2 events=$3 console=$4 exit_code=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$pkg" ) >"$events" 2>"$console" || exit_code=$?
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
# grep -F -c counts lines, so two copies of an anchor on one line report 1 --
# and ${content//anchor/replacement} would then replace BOTH while the guard
# said the anchor was unique. Occurrences and lines are different numbers and
# only one of them is the safety property.
count_literal() {
	local anchor=$1 rest=$2 n=0
	[[ -n "$anchor" ]] || { print -u2 'error: empty anchor'; return 2; }
	while [[ "$rest" == *"$anchor"* ]]; do
		(( n += 1 ))
		rest=${rest#*"$anchor"}
	done
	print -r -- "$n"
}

patch_source() {
	local rel=$1 anchor=$2 replacement=$3 file=$work/$1 content found mutated
	content=$(read_source "$file") || return 2
	found=$(count_literal "$anchor" "$content") || return 2
	if [[ "$found" != 1 ]]; then
		print -u2 "error: anchor occurs $found time(s) in $rel, want exactly 1 (source drifted): $anchor"
		return 1
	fi
	print -r -- "${content//"$anchor"/"$replacement"}" >| "$file"
	mutated=$(git -C "$work" hash-object -- "$rel")
	[[ "$mutated" != "${pristine[$rel]}" ]] || { print -u2 'error: mutation did not change the source'; return 1; }
}

restore_source() {
	local rel=$1 now
	git -C "$work" checkout --quiet -- "$rel"
	now=$(git -C "$work" hash-object -- "$rel")
	[[ "$now" == "${pristine[$rel]}" ]] || { print -u2 "error: restore left $rel at $now, want ${pristine[$rel]}"; return 1; }
}

restore_all() {
	local rel
	for rel in "${sources[@]}"; do
		restore_source "$rel" || return 1
	done
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

# assert_required_identities proves, BY NAME, that every required test reported
# a top-level PASS and that none of them (or their subtests) was skipped.
#
# A count is not an identity. Six unrelated digest tests passing says nothing
# about whether THIS killer ran, and a required test that was skipped or
# renamed away would leave a count intact while the control it anchors silently
# stopped existing.
assert_required_identities() {
	local events=$1 label=$2; shift 2
	local -a names=("$@")
	local name missing=0 probe
	if ! events_valid "$events"; then
		note "$label: event stream did not parse - no identity can be read from it"
		return 1
	fi
	for name in "${names[@]}"; do
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
		note "$label: $missing of ${#names[@]} required identities were absent or skipped"
		return 1
	fi
	note "$label: all ${#names[@]} required identities passed"
	return 0
}

# run_all_suites executes every focused suite and proves its identities.
run_all_suites() {
	local label=$1 record fields pkg selector required suite_exit slug
	for record in "${suites[@]}"; do
		fields=("${(@ps:$sep:)record}")
		pkg=$fields[1]; selector=$fields[2]; required=$fields[3]
		slug=${${pkg//.\//}//\//-}
		slug=${slug%-}
		suite_exit=$(run_focused "$pkg" "$selector" "$run_dir/$label-$slug.json" "$run_dir/$label-$slug.err")
		if (( suite_exit != 0 )); then
			note "$label $pkg FAILED (exit $suite_exit) - the suite must pass before any mutant means anything"
			[[ -s "$run_dir/$label-$slug.err" ]] && tail -n 20 -- "$run_dir/$label-$slug.err" >&2
			return 1
		fi
		assert_required_identities "$run_dir/$label-$slug.json" "$label $pkg" ${=required} || return 1
	done
	return 0
}

note "pin $pin"
for rel in "${sources[@]}"; do
	note "source $rel (${pristine[$rel]})"
done
# The artifact records WHICH invocation, never where on the host it lives.
if [[ "$run_dir" == "$repo_root"/* ]]; then
	note "report ${run_dir#$repo_root/}"
else
	note "report ${run_dir:t} (under VERIFY_PROCESS_DIGEST_REPORT_DIR, outside the repository)"
fi

run_all_suites baseline || exit 1
note 'baseline PASS'

failures=0
index=0
for record in "${mutations[@]}"; do
	(( index += 1 ))
	fields=("${(@ps:$sep:)record}")
	id=$fields[1]; rel=$fields[2]; pkg=$fields[3]
	anchor=$fields[4]; replacement=$fields[5]
	killer=$fields[6]; want=$fields[7]
	stem=$run_dir/$(printf 'm%02d-%s' "$index" "$id")

	patch_source "$rel" "$anchor" "$replacement"
	compile_exit=$(compile_check "$pkg" "$stem.compile.log")
	if (( compile_exit != 0 )); then
		restore_source "$rel"
		# A mutant that does not build never reached the assertion, so it is a
		# broken run, not evidence about the guard either way.
		note "mutant $id: BROKEN-RUN (compile exit $compile_exit) - the mutant did not build, so it proves nothing"
		[[ -s "$stem.compile.log" ]] && tail -n 20 -- "$stem.compile.log" >&2
		(( failures += 1 ))
		continue
	fi
	# Anchored to this mutant's one killer, subtests included.
	run_exit=$(run_focused "$pkg" "^${killer}\$" "$stem.json" "$stem.err")
	verdict=$(classify_run "$run_exit" "$stem.json" "$killer" "$want")
	restore_source "$rel"

	note "mutant $id: compile PASS, run $verdict (killer $killer, exit $run_exit)"
	if [[ "$verdict" != KILLED ]]; then
		(( failures += 1 ))
		[[ -s "$stem.err" ]] && tail -n 20 -- "$stem.err" >&2
	fi
done

restore_all || exit 1
# The restored run must prove the SAME identities, not merely the same count:
# it is the evidence that the mutants were undone and the guards are back.
run_all_suites restored || { note 'restored baseline FAILED - the source did not come back clean'; exit 1; }
note 'restored baseline PASS'

if (( failures > 0 )); then
	note "$failures of ${#mutations[@]} controls did not kill their mutant"
	exit 1
fi
note "all ${#mutations[@]} controls compiled and died on their named assertion"
