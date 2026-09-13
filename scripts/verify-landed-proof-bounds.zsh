#!/usr/bin/env zsh
# FAC-831: non-vacuity driver for the PRODUCER proof BOUNDS in pkg/mergeadmit.
#
# Its subject is the budget: every git command the producer entry points spend
# has to be charged to one shared allowance, and each charge site has to be
# shown to be load bearing on its own. The receipt selection guards are a
# different driver (verify-landed-receipt-guards.zsh) owned by the other lane;
# this one never reads or writes their files.
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
go_timeout=${VERIFY_LANDED_PROOF_BOUNDS_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 3 )) || (( go_timeout < 60 || go_timeout > 900 )); then
	print -u2 "warning: ignoring unusable VERIFY_LANDED_PROOF_BOUNDS_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
cleanup_timeout=60

# Reports are additive. This script creates ONE subdirectory it owns and never
# deletes a directory chosen by a caller: a path being well formed is not
# permission to destroy what is in it.
report_parent=${VERIFY_LANDED_PROOF_BOUNDS_REPORT_DIR:-$repo_root/.verify-landed-proof-bounds-logs}
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
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/verify-landed-proof-XXXXXX")
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
# Sources under mutation, DECLARED BEFORE ANYTHING READS THEM.
#
# This list and the pristine snapshot that follows are the first thing this
# driver establishes, because every later guarantee rests on them: a mutation is
# only reversible against a hash taken before it, and a restore that compares
# against a hash which was never captured proves nothing. An earlier revision
# declared this array AFTER the snapshot loop, so under `set -u` the loop hit
# "sources: parameter not set" and the run aborted immediately after creating its
# worktree -- no baseline, no mutant, no evidence. Order is the guarantee here,
# not a formatting preference.
#
# Scoped to this lane's ownership: pkg/mergeadmit only. The seeded copy of this
# file carried pkg/sync and cmd/herd entries from the receipt-guards driver;
# those belong to the other author and are removed rather than left to let this
# driver write into their files.
# ---------------------------------------------------------------------------
sources=(
	pkg/mergeadmit/proof_bounds.go
	pkg/mergeadmit/proof.go
	pkg/mergeadmit/squash_landing.go
	pkg/mergeadmit/reconcile.go
	pkg/mergeadmit/complete.go
	pkg/mergeadmit/reconstruction.go
)

# An EMPTY source set is not "nothing to protect": it is a driver that would run
# its mutation loop against files it never snapshotted. zsh iterates an empty
# array zero times without complaint, so the refusal has to be explicit.
(( ${#sources} > 0 )) || { print -u2 'error: no sources are declared; there is nothing to snapshot and nothing to restore'; exit 1; }

typeset -A pristine
for rel in "${sources[@]}"; do
	[[ -n "${pristine[$rel]-}" ]] && { print -u2 "error: $rel is declared twice in sources"; exit 1; }
	[[ -f "$work/$rel" ]] || { print -u2 "error: $rel does not exist in the mutation checkout"; exit 1; }
	h=$(git -C "$work" hash-object -- "$rel") || { print -u2 "error: cannot hash $rel"; exit 1; }
	[[ -n "$h" ]] || { print -u2 "error: empty hash for $rel"; exit 1; }
	pristine[$rel]=$h
done
# Count, not just per-file success: a snapshot that silently covered fewer files
# than were declared would leave some source mutable with no way back.
(( ${#pristine} == ${#sources} )) || { print -u2 "error: snapshotted ${#pristine} of ${#sources} sources"; exit 1; }

# ---------------------------------------------------------------------------
# Focused suites. ONE row, pkg/mergeadmit: this driver proves the producer budget
# guards and nothing else. It deliberately EXCLUDES the subprocess CLI fixtures,
# which build and run the whole binary and would be paid for again on every
# mutant; their coverage belongs to the ordinary test job. This job exists to
# prove the assertions below are not vacuous.
# ---------------------------------------------------------------------------
suites=(
"./pkg/mergeadmit/${sep}^(TestProofCommandBudgetRefusesInsteadOfRunningUnbounded|TestProofRangeBudgetRefusesAnOversizedRange|TestProofOutputBudgetRefusesOversizedCommandOutput|TestProofDeadlineIsSharedAndRefusesExpired|TestProofDefaultBudgetStillProvesAnOrdinaryLanding|TestEnsureProofBudgetInstallsOnceAndNeverReplaces|TestProofLedgerRefusesPastItsAllowance|TestBoundedBufferRefusesRatherThanTruncating|TestProofBudgetSurvivesRevisionResolution|TestContentPreservedAtAbortsOnBudgetRatherThanAnsweringFalse|TestReplayTreeContextCarriesAnAllowance|TestGitrootReplayRefusesNilRunnerAndEmptyIdentities|TestGateProveLandedCarriesItsInjectedBudget|TestGateProveLandedSucceedsOnTheDefaultBudget|TestGitOutBytesIsTheChargingBoundaryForGitReads|TestStablePatchIDChargesItsOwnCommand|TestAncestorProvenChargesItsOwnCommand|TestRepositoryIdentityIsChargedAndRefusalStaysRecognisable|TestRequireAncestorBoundedChargesAndSeparatesAbsenceFromRefusal|TestPublicProveMergeModeDoesNotReportBudgetRefusalAsNonAncestry|TestPublicProveMergeModeSucceedsOnTheDefaultBudget|TestGateProveLandedSpendsOneAllowanceAcrossTheInvocation|TestCompletePublicEntryStopsOnTheSharedAllowanceBeforeSealing|TestCompletePublicEntrySucceedsOnTheDefaultAllowance|TestReconcileLandedPublicEntryStopsOnTheSharedAllowance|TestReconcileLandedPublicEntrySucceedsOnTheDefaultAllowance|TestReconcileLandedReducedPublicEntryStopsOnTheSharedAllowance|TestReconcileLandedReducedPublicEntrySucceedsOnTheDefaultAllowance|TestFollowUpPublicEntryStopsOnTheSharedAllowance|TestFollowUpPublicEntrySucceedsOnTheDefaultAllowance|TestReconcileLandedReconstructionSucceedsOnTheDefaultAllowance|TestReconcileLandedReconstructionAncestrySpendsTheSharedAllowance)\$${sep}TestProofCommandBudgetRefusesInsteadOfRunningUnbounded TestProofRangeBudgetRefusesAnOversizedRange TestProofOutputBudgetRefusesOversizedCommandOutput TestProofDeadlineIsSharedAndRefusesExpired TestProofDefaultBudgetStillProvesAnOrdinaryLanding TestEnsureProofBudgetInstallsOnceAndNeverReplaces TestProofLedgerRefusesPastItsAllowance TestBoundedBufferRefusesRatherThanTruncating TestProofBudgetSurvivesRevisionResolution TestContentPreservedAtAbortsOnBudgetRatherThanAnsweringFalse TestReplayTreeContextCarriesAnAllowance TestGitrootReplayRefusesNilRunnerAndEmptyIdentities TestGateProveLandedCarriesItsInjectedBudget TestGateProveLandedSucceedsOnTheDefaultBudget TestGitOutBytesIsTheChargingBoundaryForGitReads TestStablePatchIDChargesItsOwnCommand TestAncestorProvenChargesItsOwnCommand TestRepositoryIdentityIsChargedAndRefusalStaysRecognisable TestRequireAncestorBoundedChargesAndSeparatesAbsenceFromRefusal TestPublicProveMergeModeDoesNotReportBudgetRefusalAsNonAncestry TestPublicProveMergeModeSucceedsOnTheDefaultBudget TestGateProveLandedSpendsOneAllowanceAcrossTheInvocation TestCompletePublicEntryStopsOnTheSharedAllowanceBeforeSealing TestCompletePublicEntrySucceedsOnTheDefaultAllowance TestReconcileLandedPublicEntryStopsOnTheSharedAllowance TestReconcileLandedPublicEntrySucceedsOnTheDefaultAllowance TestReconcileLandedReducedPublicEntryStopsOnTheSharedAllowance TestReconcileLandedReducedPublicEntrySucceedsOnTheDefaultAllowance TestFollowUpPublicEntryStopsOnTheSharedAllowance TestFollowUpPublicEntrySucceedsOnTheDefaultAllowance TestReconcileLandedReconstructionSucceedsOnTheDefaultAllowance TestReconcileLandedReconstructionAncestrySpendsTheSharedAllowance"
)

# ---------------------------------------------------------------------------
# Controls. Each mutates REAL production source and is killed by a test that
# names the site, not by a generic refusal.
#
# THREE INDEPENDENT CHARGE SITES, THREE CONTROLS. An end-to-end "the proof
# refuses" killer cannot attribute a charge: remove one site and the other two
# still exhaust the allowance and produce the identical refusal. That is exactly
# how the gitOutBytes mutant once survived while four patch-id commands paid for
# it. Each control below is killed by an observer that COUNTS the charge at its
# own site.
#
# What each one ACTUALLY proves, at its real strength:
#
#   gitoutbytes-not-charged
#                          every git READ goes through the charging boundary.
#                          Without it a read path spends no allowance and the
#                          budget can be walked straight past.
#   patchid-not-charged    the patch-id invocation is its own command, charged
#                          separately from the diff-tree read that feeds it. Its
#                          killer asserts the COUNT (2), so one charge standing
#                          in for two cannot pass.
#   ancestry-not-charged   each ancestry probe is charged, which is what bounds
#                          the selection loop that calls it repeatedly.
#   range-budget-not-enforced
#                          a range is refused before it is materialised, rather
#                          than after the memory has been spent.
#   entry-path-unbounded-again
#                          the PUBLIC entry installs the allowance. A mutant
#                          that swaps the budgeted context for a bare
#                          cancellable one reproduces the original defect: an
#                          entry point that proves a landing while ignoring the
#                          budget it was given.
#   budget-error-flattened
#                          a budget refusal stays recognisable through revision
#                          resolution instead of being reported as a revision
#                          that does not resolve. ONE control, not two: an
#                          earlier revision split this into a passthrough mutant
#                          and a %w mutant and NEITHER could kill, because each
#                          half alone still lets errors.Is reach the sentinel
#                          through the other. Only restoring the original
#                          flattening -- the passthrough gone AND the cause
#                          dropped -- reproduces the CI 34741746509 regression.
#                          It is anchored to the CHILD subtest that makes the
#                          assertion: Go emits an assertion's output event under
#                          the subtest that made it, so a parent killer would
#                          carry a fail action with no matching assertion output
#                          of its own. The parent stays in the suite row, so its
#                          top-level PASS is still required at baseline and
#                          after restore.
#
#   identity-outside-allowance
#                          the repository identity read goes through the shared
#                          runner-aware reader with THIS invocation's allowance.
#                          Its killer COUNTS the charge, so another budget path
#                          paying for it cannot mask the mutant.
#   ancestry-absence-vs-refusal
#                          a bounded ancestry probe separates "not an ancestor"
#                          from "could not answer". This is the single-hunk form
#                          of the merge-mode control: the two-hunk form the
#                          producer author proposed would have had to re-add the
#                          gitroot import to proof.go, and a mutant that does not
#                          build is BROKEN-RUN, not a kill.
#   reconcile-installs-second-budget
#                          the public ReconcileLanded entry installs the shared
#                          allowance rather than taking a fresh unbudgeted
#                          context. Anchored with the comment line above the
#                          call because `ctx, cancel := g.gateProofContext()`
#                          appears TWICE in reconcile.go -- the public entry and
#                          validatePriorReceipt -- and a two-site anchor is a
#                          control that cannot be applied.
#
#   complete-installs-second-budget
#                          the public Complete entry spends THIS invocation's
#                          allowance. The mutant takes a throwaway gate's instead,
#                          which is a DEFAULT budget, so an injected allowance of
#                          one is ignored and Complete seals anyway. Import-free
#                          on purpose: complete.go does not import context, and a
#                          mutant that does not build is BROKEN-RUN, not a kill.
#   reconstruction-ancestry-unbounded
#                          the reconstruction ancestry probes are charged to the
#                          shared allowance rather than run on a fresh context.
#                          Its killer does not hardcode a command count: it
#                          sweeps the allowance upward and stops at the first one
#                          whose refusal names the path-set read, so the boundary
#                          IS the number of commands charged before that stage.
#                          Correct source gives 2 (both probes charged); the
#                          mutant gives 1. Derived, not assumed.
#
# id | source | test package | anchor | replacement | killer | required assertion
# ---------------------------------------------------------------------------
mutations=(
# Three INDEPENDENT charge sites, three controls. An end-to-end "the proof
# refuses" killer cannot attribute a charge: remove one site and the other two
# still exhaust the allowance and produce the identical refusal, which is how
# the gitOutBytes mutant survived while four patch-id commands paid for it. Each
# control below is killed by an observer that COUNTS the charge at its own site.
"gitoutbytes-not-charged${sep}pkg/mergeadmit/proof.go${sep}./pkg/mergeadmit/${sep}	if err := ledger.spendCommand(args); err != nil {
		return nil, err
	}${sep}	_ = ledger // MUTANT: git reads are not charged against the budget${sep}TestGitOutBytesIsTheChargingBoundaryForGitReads${sep}gitOutBytes is not charging and the budget can be bypassed through it"
"patchid-not-charged${sep}pkg/mergeadmit/proof.go${sep}./pkg/mergeadmit/${sep}	if err := ledger.spendCommand([]string{\"patch-id\", \"--stable\"}); err != nil {
		return \"\", err
	}${sep}	_ = ledger // MUTANT: patch-id is not charged against the budget${sep}TestStablePatchIDChargesItsOwnCommand${sep}want 2 (the diff-tree read and the patch-id itself)"
"ancestry-not-charged${sep}pkg/mergeadmit/proof.go${sep}./pkg/mergeadmit/${sep}	if err := ledgerFrom(ctx).spendCommand([]string{\"merge-base\", \"--is-ancestor\"}); err != nil {
		return false, err
	}${sep}	_ = ctx // MUTANT: ancestry probes are not charged, the search loop is unbounded${sep}TestAncestorProvenChargesItsOwnCommand${sep}the search loop could then run unbounded"
"range-budget-not-enforced${sep}pkg/mergeadmit/proof.go${sep}./pkg/mergeadmit/${sep}			if len(commits) >= maxCommits {
				return nil, fmt.Errorf(\"%w: %s..%s holds more than %d commits\",
					ErrProofBudgetRange, short(base), short(tip), maxCommits)
			}${sep}			_ = maxCommits // MUTANT: the range is materialised without a bound${sep}TestProofRangeBudgetRefusesAnOversizedRange${sep}materialised a four-commit range"
"entry-path-unbounded-again${sep}pkg/mergeadmit/squash_landing.go${sep}./pkg/mergeadmit/${sep}	// used to be checked here has been dropped.
	ctx, cancel := g.gateProofContext()${sep}	// used to be checked here has been dropped.
	ctx, cancel := (&Gate{}).gateProofContext() // MUTANT: the entry spends a throwaway gate allowance, not this invocation${sep}TestGateProveLandedCarriesItsInjectedBudget${sep}ignored its injected allowance and proved a landing"
# ONE control, not two. An earlier revision split this into a passthrough mutant
# and a %w mutant; NEITHER could kill, because each half alone still lets
# errors.Is reach the sentinel through the other. Only restoring the original
# flattening -- the budget passthrough gone AND the cause dropped -- reproduces
# the CI 34741746509 regression, so that is the one mutation worth making.
"budget-error-flattened${sep}pkg/mergeadmit/proof_bounds.go${sep}./pkg/mergeadmit/${sep}	if isProofBudgetError(err) {
		return err
	}
	return fmt.Errorf(\"%s revision %q does not resolve to a commit in %s: %w\", role, rev, repoDir, err)${sep}	return fmt.Errorf(\"%s revision %q does not resolve to a commit in %s\", role, rev, repoDir) // MUTANT: the original flattening, cause and sentinel both lost${sep}TestProofBudgetSurvivesRevisionResolution/commands_exhausted_during_resolution${sep}a flattened budget error reads as an ordinary resolution failure"
"identity-outside-allowance${sep}pkg/mergeadmit/proof_bounds.go${sep}./pkg/mergeadmit/${sep}	return toolchild.RepositoryIdentityWithRunner(g.RepoDir, boundedGit(ctx, g.RepoDir))${sep}	return toolchild.RepositoryIdentity(g.RepoDir) // MUTANT: identity runs outside the allowance${sep}TestRepositoryIdentityIsChargedAndRefusalStaysRecognisable${sep}the identity read charged"
"ancestry-absence-vs-refusal${sep}pkg/mergeadmit/proof_bounds.go${sep}./pkg/mergeadmit/${sep}	if !proven {${sep}	if false && !proven { // MUTANT: absence and refusal conflated${sep}TestRequireAncestorBoundedChargesAndSeparatesAbsenceFromRefusal${sep}a non-ancestor was accepted"
"reconcile-installs-second-budget${sep}pkg/mergeadmit/reconcile.go${sep}./pkg/mergeadmit/${sep}	// those previously installed its own or ran outside any (review 212).
	ctx, cancel := g.gateProofContext()${sep}	// those previously installed its own or ran outside any (review 212).
	ctx, cancel := (&Gate{}).gateProofContext() // MUTANT: ReconcileLanded spends a throwaway gate allowance, not this invocation${sep}TestReconcileLandedPublicEntryStopsOnTheSharedAllowance${sep}ReconcileLanded sealed a receipt on an exhausted allowance"
"complete-installs-second-budget${sep}pkg/mergeadmit/complete.go${sep}./pkg/mergeadmit/${sep}	ctx, cancel := g.gateProofContext()${sep}	ctx, cancel := (\&Gate{}).gateProofContext() // MUTANT: Complete spends a throwaway gate allowance, not this invocation${sep}TestCompletePublicEntryStopsOnTheSharedAllowanceBeforeSealing${sep}Complete sealed a receipt on an exhausted allowance"
"reconstruction-ancestry-unbounded${sep}pkg/mergeadmit/reconstruction.go${sep}./pkg/mergeadmit/${sep}		if err := requireAncestorBounded(ctx, g.RepoDir, pair[0], pair[1], \"reconstruction base\"); err != nil {${sep}		if err := requireAncestorBounded(context.Background(), g.RepoDir, pair[0], pair[1], \"reconstruction base\"); err != nil { // MUTANT: reconstruction ancestry runs outside the shared allowance${sep}TestReconcileLandedReconstructionAncestrySpendsTheSharedAllowance${sep}the reconstruction ancestry probes did not spend the shared allowance:"
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
	note "report ${run_dir:t} (under VERIFY_LANDED_PROOF_BOUNDS_REPORT_DIR, outside the repository)"
fi


# Every control row is validated BEFORE the run starts, against the snapshot
# taken above. A row with the wrong shape, or naming a file this driver never
# hashed, would be mutated with no pristine hash to restore from -- and the
# lookup that would otherwise catch it only happens during mutation, after the
# baseline suite and its child workload have already been paid for.
#
# This block is IDENTICAL in both drivers on purpose. They drifted once: one
# gained a test-package check the other did not have, and a registry defect that
# one driver refused, the other carried into a baseline. Keeping one text in two
# places is a duplicate rule, and the honest mitigation is that it is byte-for-
# byte the same and audited as a pair, not paraphrased in each.
#
# It is placed AFTER the helpers it calls and BEFORE run_all_suites. The order
# is load bearing in both directions: calling count_literal before it is defined
# aborts under set -e, and validating after the baseline is the defect this
# exists to remove.
(( ${#mutations} > 0 )) || { print -u2 'error: no controls are declared; this driver would report success having proven nothing'; exit 1; }
typeset -A control_ids
for record in "${mutations[@]}"; do
	fields=("${(@ps:$sep:)record}")
	if (( ${#fields} != 7 )); then
		print -u2 "error: control row ${fields[1]:-<unnamed>} has ${#fields} fields, want 7"
		exit 1
	fi
	id=$fields[1]
	# An unnamed or repeated id makes the evidence ambiguous: every line in the
	# summary and every artifact stem is keyed on it, and a verdict that cannot
	# be attributed to one control is not evidence about that control.
	[[ -n "$id" ]] || { print -u2 'error: a control row has an empty id; its verdict would name nothing'; exit 1; }
	[[ -z "${control_ids[$id]-}" ]] || { print -u2 "error: control id $id is declared twice; two verdicts would report under one name"; exit 1; }
	control_ids[$id]=1
	rel=$fields[2]
	[[ -n "$rel" ]] || { print -u2 "error: control $id names no source to mutate"; exit 1; }
	if [[ -z "${pristine[$rel]-}" ]]; then
		print -u2 "error: control $id mutates $rel, which is not in sources and has no pristine hash"
		exit 1
	fi
	[[ -n "$fields[3]" ]] || { print -u2 "error: control $id has no test package to run"; exit 1; }
	[[ -n "$fields[4]" ]] || { print -u2 "error: control $id has an empty anchor"; exit 1; }
	# Uniqueness is checked against the ACTUAL source, here rather than at
	# mutation time: an anchor that drifted, or that matches two sites, is a
	# control that cannot be applied, and learning that from CI an hour later is
	# the avoidable half of the cost.
	occurrences=$(count_literal "$fields[4]" "$(<$work/$rel)") || exit 1
	if [[ "$occurrences" != 1 ]]; then
		print -u2 "error: control $id anchor occurs $occurrences time(s) in $rel, want exactly 1"
		exit 1
	fi
	[[ -n "$fields[5]" ]] || { print -u2 "error: control $id has an empty replacement"; exit 1; }
	[[ "$fields[4]" != "$fields[5]" ]] || { print -u2 "error: control $id replacement equals its anchor"; exit 1; }
	[[ -n "$fields[6]" ]] || { print -u2 "error: control $id names no killer test"; exit 1; }
	[[ -n "$fields[7]" ]] || { print -u2 "error: control $id names no required assertion"; exit 1; }
done

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

	if ! patch_source "$rel" "$anchor" "$replacement"; then
		# An anchor that no longer matches is source drift, not evidence about
		# the guard. Say so in the artifact and stop: continuing would run the
		# remaining controls against a checkout whose state is unknown.
		note "mutant $id: ANCHOR DRIFT in $rel - the control could not be applied"
		exit 1
	fi
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
