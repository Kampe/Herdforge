#!/usr/bin/env zsh
# FAC-831: non-vacuity driver for the landed-receipt selection guards.
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
go_timeout=${VERIFY_LANDED_RECEIPT_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 3 )) || (( go_timeout < 60 || go_timeout > 900 )); then
	print -u2 "warning: ignoring unusable VERIFY_LANDED_RECEIPT_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
cleanup_timeout=60

# Reports are additive. This script creates ONE subdirectory it owns and never
# deletes a directory chosen by a caller: a path being well formed is not
# permission to destroy what is in it.
report_parent=${VERIFY_LANDED_RECEIPT_REPORT_DIR:-$repo_root/.verify-landed-receipt-logs}
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
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/verify-landed-receipt-XXXXXX")
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
	pkg/mergeadmit/landed_integration.go
	pkg/mergeadmit/reconcile.go
	pkg/sync/donereceipt.go
	cmd/herd/verify_landed_surface.go
	cmd/herd/verify_landed_candidate.go
	cmd/herd/reviewingest.go
	pkg/mergeadmit/probe.go
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
# Focused suites. The pkg/sync row is the CONSUMER half: a receipt the producer
# seals is only useful if the shipped consumer accepts it, and the FAC-831
# carrier is meaningless unless the gate that reads it is proven too. These
# deliberately EXCLUDE the subprocess CLI fixtures: they
# build and run the whole binary, which this driver would then pay for on every
# mutant. Their coverage belongs to the ordinary test job; this job exists to
# prove the assertions below are not vacuous.
# ---------------------------------------------------------------------------
suites=(
"./pkg/mergeadmit/${sep}^(TestIntegrationCommitForPromotesCarrierToTheMergeCommit|TestIntegrationCommitForLeavesAnOrdinaryLandingAlone|TestIntegrationCommitForRefusesWhenNothingIntegratesTheBase|TestEquivalentLandedProofSealsIntegrationCommitAndKeepsContentPatchID|TestEquivalentLandedProofIsUnchangedForAnOrdinaryLanding|TestEquivalentLandedProofRefusesRatherThanSealAnUnapprovableReceipt|TestIntegrationCommitForRefusesAnOursMergeThatDiscardedTheContent|TestIntegrationCommitForRefusesAMergeThatAlteredTheReviewedContent|TestContentPreservedAtSeparatesAnHonestMergeFromAnOursMerge|TestIntegrationCommitForSelectsTheMergeEvenWhenALaterCommitRevertsIt|TestProofRoutesRefuseAValueOnlyOriginProbe|TestProofRoutesAcceptAStaticContextProbe|TestProofRoutesRefuseAnEmptyOriginReading|TestReconcileLandedSealsThePullRequestCarrierAndPublicValidateAcceptsIt|TestReconcileLandedReducedSealsThePullRequestCarrierAndPublicValidateAcceptsIt)\$${sep}TestIntegrationCommitForPromotesCarrierToTheMergeCommit TestIntegrationCommitForLeavesAnOrdinaryLandingAlone TestIntegrationCommitForRefusesWhenNothingIntegratesTheBase TestEquivalentLandedProofSealsIntegrationCommitAndKeepsContentPatchID TestEquivalentLandedProofIsUnchangedForAnOrdinaryLanding TestEquivalentLandedProofRefusesRatherThanSealAnUnapprovableReceipt TestIntegrationCommitForRefusesAnOursMergeThatDiscardedTheContent TestIntegrationCommitForRefusesAMergeThatAlteredTheReviewedContent TestContentPreservedAtSeparatesAnHonestMergeFromAnOursMerge TestIntegrationCommitForSelectsTheMergeEvenWhenALaterCommitRevertsIt TestProofRoutesRefuseAValueOnlyOriginProbe TestProofRoutesAcceptAStaticContextProbe TestProofRoutesRefuseAnEmptyOriginReading TestReconcileLandedSealsThePullRequestCarrierAndPublicValidateAcceptsIt TestReconcileLandedReducedSealsThePullRequestCarrierAndPublicValidateAcceptsIt"
"./cmd/herd/${sep}^(TestResolveVerifyLandedSurfaceUsesALiveCarrierUnchanged|TestResolveVerifyLandedSurfaceRefusesRetiredCarrierWithoutAPin|TestResolveVerifyLandedSurfaceAcceptsAPinnedCandidate|TestResolveVerifyLandedSurfaceSelectsWithoutProvingTheRepository|TestPinnedCandidateForPrefersExplicitAndNeverUsesABranchHead|TestRequirePinnedCandidateProvedBindsTheFallbackToThePin|TestVerifyLandedGateProvesAPinnedRetiredCandidate|TestVerifyLandedGateRefusesAnAbsentPin|TestVerifyLandedGateRefusesANonCommitPin|TestVerifyLandedGateRefusesAnEmptyPinAsAMissingPin|TestVerifyLandedGateStopsOnAnExhaustedAllowance|TestRunHarvestVerifyLandedSealsAndRecordsOnTheDefaultAllowance|TestRunHarvestVerifyLandedRecordsNothingWhenSealingExhausts|TestVerifyLandedCompositionStopsBeforeAnythingIsRecorded|TestVerifyLandedCompositionRecordsNothingWhenTheSealExhausts|TestCandidateResolutionRefusesLandedHead|TestExplicitCandidateWins|TestUnmergedHeadStillResolves|TestCandidateResolutionRefusesWhenOriginCannotBeFetched|TestCandidateResolutionSpendsTheCallersAllowance|TestCandidateResolutionStopsOnACancelledRun|TestObserveVerifyLandedSquashPreservesCandidate|TestRunHarvestVerifyLandedResolvesAnOmittedCandidateAndSeals|TestRunHarvestVerifyLandedSealsFromALiveCarrierWithAnOmittedCandidate|TestRunHarvestVerifyLandedChargesCandidateResolutionToTheSharedAllowance|TestRunHarvestVerifyLandedRefusesResolutionWhenOriginCannotBeFetched|TestRunHarvestVerifyLandedRefusesACarrierHeadAlreadyContainedInOriginMain|TestVerifyLandedCompositionRefusesWhenOriginMovesBetweenReads|TestResolveVerifyLandedSurfaceRefusesAFailedCarrierLookup|TestWorktreeForBranchSeparatesAnAbsentCarrierFromAFailedLookup|TestRunHarvestVerifyLandedRefusesWhenTheCarrierLookupCannotRun)\$${sep}TestResolveVerifyLandedSurfaceUsesALiveCarrierUnchanged TestResolveVerifyLandedSurfaceRefusesRetiredCarrierWithoutAPin TestResolveVerifyLandedSurfaceAcceptsAPinnedCandidate TestResolveVerifyLandedSurfaceSelectsWithoutProvingTheRepository TestPinnedCandidateForPrefersExplicitAndNeverUsesABranchHead TestRequirePinnedCandidateProvedBindsTheFallbackToThePin TestVerifyLandedGateProvesAPinnedRetiredCandidate TestVerifyLandedGateRefusesAnAbsentPin TestVerifyLandedGateRefusesANonCommitPin TestVerifyLandedGateRefusesAnEmptyPinAsAMissingPin TestVerifyLandedGateStopsOnAnExhaustedAllowance TestRunHarvestVerifyLandedSealsAndRecordsOnTheDefaultAllowance TestRunHarvestVerifyLandedRecordsNothingWhenSealingExhausts TestVerifyLandedCompositionStopsBeforeAnythingIsRecorded TestVerifyLandedCompositionRecordsNothingWhenTheSealExhausts TestCandidateResolutionRefusesLandedHead TestExplicitCandidateWins TestUnmergedHeadStillResolves TestCandidateResolutionRefusesWhenOriginCannotBeFetched TestCandidateResolutionSpendsTheCallersAllowance TestCandidateResolutionStopsOnACancelledRun TestObserveVerifyLandedSquashPreservesCandidate TestRunHarvestVerifyLandedResolvesAnOmittedCandidateAndSeals TestRunHarvestVerifyLandedSealsFromALiveCarrierWithAnOmittedCandidate TestRunHarvestVerifyLandedChargesCandidateResolutionToTheSharedAllowance TestRunHarvestVerifyLandedRefusesResolutionWhenOriginCannotBeFetched TestRunHarvestVerifyLandedRefusesACarrierHeadAlreadyContainedInOriginMain TestVerifyLandedCompositionRefusesWhenOriginMovesBetweenReads TestResolveVerifyLandedSurfaceRefusesAFailedCarrierLookup TestWorktreeForBranchSeparatesAnAbsentCarrierFromAFailedLookup TestRunHarvestVerifyLandedRefusesWhenTheCarrierLookupCannotRun"
"./pkg/sync/${sep}^(TestValidateAcceptsSealedCarrierForAPullRequestLanding|TestValidateStillBindsContentToTheMergeWhenNoCarrierIsSealed|TestValidateRefusesForgedAndPatchMismatchedCarriers|TestSealedCarrierIsCoveredByTheDigest|TestValidateAcceptsALaterRevisionOfTheCarriersOwnPath|TestValidateRefusesAMergeThatDiscardedTheReviewedContent|TestValidateFollowsAReviewedRenameToItsDestination|TestValidateAcceptsARenameThatEditsInTheSameReviewedCommit|TestValidateRefusesAnAlteredRenameDestination|TestValidateAcceptsIndependentMainAndReviewedHunks|TestValidateRefusesASubstitutedSealedCandidate|TestValidateRefusesTheIntegrationCommitAsItsOwnCandidate|TestValidateStopsInTheIdentityStageWhenTheBudgetIsGone|TestValidateStopsInTheIntegrationAncestryStageWhenTheBudgetIsGone|TestValidateStopsInTheCarrierAncestryStageWhenTheBudgetIsGone|TestValidateStopsInTheReplayStageWhenTheBudgetIsGone|TestValidateStopsInThePatchStageWhenTheBudgetIsGone|TestValidateSpendsOneSharedBudgetAndNeverResetsIt|TestPatchIDRefusesAnOversizeDiff|TestContentProofRefusesAnOversizePatchInput|TestContentProofRefusesAfterItsDeadline|TestContentProofRefusesWhenTheCommandBudgetIsSpent|TestContentProofRefusesOversizeCommandOutput|TestBoundedOutputRefusesToGrowPastItsCap|TestBoundedGitIsBuiltForAnOwnedProcessGroup|TestValidateDoesNotReportAStoppedProbeAsANonAncestor|TestValidateRefusesACarrierThisRepositoryDoesNotHave)\$${sep}TestValidateAcceptsSealedCarrierForAPullRequestLanding TestValidateStillBindsContentToTheMergeWhenNoCarrierIsSealed TestValidateRefusesForgedAndPatchMismatchedCarriers TestSealedCarrierIsCoveredByTheDigest TestValidateAcceptsALaterRevisionOfTheCarriersOwnPath TestValidateRefusesAMergeThatDiscardedTheReviewedContent TestValidateFollowsAReviewedRenameToItsDestination TestValidateAcceptsARenameThatEditsInTheSameReviewedCommit TestValidateRefusesAnAlteredRenameDestination TestValidateAcceptsIndependentMainAndReviewedHunks TestValidateRefusesASubstitutedSealedCandidate TestValidateRefusesTheIntegrationCommitAsItsOwnCandidate TestValidateStopsInTheIdentityStageWhenTheBudgetIsGone TestValidateStopsInTheIntegrationAncestryStageWhenTheBudgetIsGone TestValidateStopsInTheCarrierAncestryStageWhenTheBudgetIsGone TestValidateStopsInTheReplayStageWhenTheBudgetIsGone TestValidateStopsInThePatchStageWhenTheBudgetIsGone TestValidateSpendsOneSharedBudgetAndNeverResetsIt TestPatchIDRefusesAnOversizeDiff TestContentProofRefusesAnOversizePatchInput TestContentProofRefusesAfterItsDeadline TestContentProofRefusesWhenTheCommandBudgetIsSpent TestContentProofRefusesOversizeCommandOutput TestBoundedOutputRefusesToGrowPastItsCap TestBoundedGitIsBuiltForAnOwnedProcessGroup TestValidateDoesNotReportAStoppedProbeAsANonAncestor TestValidateRefusesACarrierThisRepositoryDoesNotHave"
)

# ---------------------------------------------------------------------------
# Controls. Each is the subject of a review finding, and each mutates REAL
# production source.
#
# What each one ACTUALLY proves, stated at its real strength:
#
#   integration-commit-required
#                          the producer promotes a patch carrier that sits off
#                          the integrated line to the commit that INTEGRATED it.
#                          Without the promotion the carrier is sealed as
#                          MergeSHA and the consumer refuses a landing that
#                          genuinely happened (PR836).
#   integration-content-replay-required
#                          selection is a CONTENT claim, not a graph claim: a
#                          merge that kept the reviewed commit as an ancestor
#                          while discarding every reviewed hunk must not be
#                          selected. Ancestry alone accepts it; the replay is
#                          what rejects it.
#   retired-carrier-pin-required
#                          a retired carrier with no pinned candidate is refused
#                          rather than handed to the invoker as a proof surface.
#   sealed-carrier-copied-into-the-receipt
#                          the full-provenance producer COPIES the proved
#                          content carrier into the receipt it seals. This is
#                          the exact field-copy defect the FAC-831 review found:
#                          Proof carried ContentSHA and CompletionReceipt did
#                          not, so a pull-request landing sealed content against
#                          a merge commit with no patch of its own and public
#                          Validate refused it. The killer runs the real
#                          ReconcileLanded and then the shipped consumer gate.
#   sealed-carrier-copied-into-the-reduced-receipt
#                          the same copy in the SEPARATE reduced-provenance
#                          receipt literal, which is the path an actual
#                          `--verify-landed` pull request reconciliation takes.
#                          It is its own copy site and the full-provenance
#                          control cannot speak for it.
#
#   sealed-carrier-requires-the-replay
#                          a sealed carrier is not accepted on ancestry and a
#                          matching patch id alone. Delete the replay and an ours
#                          merge -- carrier still an ancestor, every reviewed hunk
#                          gone -- validates.
#   replayed-tree-must-equal-the-merged-tree
#                          the replayed reviewed result must BE what landed.
#                          Computing it and not comparing it accepts a merge
#                          amended to substitute different bytes at a renamed
#                          destination, which the old path-scoped comparison
#                          could not see at all.
#   candidate-may-not-be-the-integration-commit
#                          a receipt may not name the merge as its own reviewed
#                          candidate. Replaying a commit onto its own parent
#                          reproduces its tree, so the claim would prove itself:
#                          this is the refusal that stops the sealed candidate
#                          from being replaced by the thing it is supposed to
#                          justify.
#   proof-command-budget-enforced
#   proof-deadline-enforced
#   proof-output-budget-enforced
#                          the three physical limits of the consumer proof are
#                          load bearing: total subprocesses, the one shared
#                          deadline, and the bytes a single command may return.
#                          The shared replay primitive runs through this same
#                          runner and starts no process of its own, so these
#                          bound it too. Their killers drive an INJECTED command,
#                          so they prove the guard rather than the speed or size
#                          of whatever ran CI that day.
#
#   validation-budget-never-reset
#                          the ONE allowance is cumulative across the whole
#                          public validation. The mutant makes the content half
#                          mint its own, which is the same defect the review
#                          found in a different shape: every stage bounded, the
#                          validation not. Its killer gives the entry an
#                          allowance one command short of the whole run, which
#                          only passes if some stage started counting again.
#   identity-read-must-be-bounded
#                          the repository identity read goes through the shared
#                          runner-aware reader with THIS validation's budget.
#                          The mutant restores the unbounded
#                          toolchild.RepositoryIdentity, and the killer notices
#                          because no identity command reaches the runner at all.
#   ancestry-budget-is-not-an-answer
#                          an exhausted budget in an ancestry probe reaches the
#                          caller as the sentinel. The mutant lets it answer
#                          "no", which is how a stopped validation gets recorded
#                          as a commit that is not an ancestor -- a content
#                          verdict invented out of a killed process.
#   process-input-measured-before-start
#                          input handed to a process is measured BEFORE it
#                          starts. The patch pipeline cannot exceed it today
#                          because the diff it feeds is already capped on the way
#                          out; the guard is what keeps that true for any later
#                          caller that passes stdin.
#
#   git-runs-in-an-owned-process-group
#                          every command this package hands git is built by
#                          procsignal.CommandContext, so a cancelled deadline
#                          kills the GROUP. exec.CommandContext with a WaitDelay
#                          signals the direct child only, and a git that spawned
#                          a helper keeps running with the pipe open after the
#                          deadline said stop. The killer asserts the wiring
#                          STRUCTURALLY and starts no process: the behaviour of
#                          the group kill is already proven once at the
#                          primitive, by procsignal's own descendant-kill test,
#                          and a second copy of that claim here would be the
#                          duplication this repository gates against. What is
#                          proven here is that THIS package's route goes through
#                          it.
#
# Every control's assertion is emitted by its killer test ITSELF: no killer in
# this file declares a subtest, and the shared helpers that assert for them
# (assertIntegrationContract, assertSealedCarrierReceipt) run on the killer's own
# *testing.T, so their output carries the killer's exact .Test name. Nothing here
# depends on prefix matching to be attributable.
#
# Gate.Complete's copy of the same field has NO control here, deliberately: its
# producer is Prove, which never sets ContentSHA on any of its three modes, so
# no test can distinguish the copy from its absence. A control that cannot kill
# reports coverage that does not exist.
# id | source | test package | anchor | replacement | killer | required assertion
# ---------------------------------------------------------------------------
mutations=(
"integration-commit-required${sep}pkg/mergeadmit/landed_integration.go${sep}./pkg/mergeadmit/${sep}	if carrierHasBase {
		return carrier, nil
	}${sep}	if true || carrierHasBase { // MUTANT: promotion removed, the carrier is always sealed
		return carrier, nil
	}${sep}TestIntegrationCommitForPromotesCarrierToTheMergeCommit${sep}selected the patch carrier"
# REMOVED, deliberately: an earlier revision carried a "no-false-integration-proof"
# control that mutated the base-ancestry test. With the content replay in place
# it could not be shown to be independently load bearing -- every fixture that
# would exercise it is already refused by the replay, so the mutant SURVIVED.
# A control that cannot kill is worse than no control, because it reports
# coverage that does not exist. Base ancestry remains enforced in source and is
# asserted directly by the tests; it simply has no honest mutant here.
"integration-content-replay-required${sep}pkg/mergeadmit/landed_integration.go${sep}./pkg/mergeadmit/${sep}		if preserved {
			return sha, nil
		}${sep}		if true || preserved { // MUTANT: content replay no longer required, ancestry alone selects
			return sha, nil
		}${sep}TestIntegrationCommitForRefusesAnOursMergeThatDiscardedTheContent${sep}an ours merge that discarded every reviewed hunk was sealed"
"retired-carrier-pin-required${sep}cmd/herd/verify_landed_surface.go${sep}./cmd/herd/${sep}	if pinned == \"\" {
		return verifyLandedSurface{}, errRetiredCarrierUnpinned
	}${sep}	if false && pinned == \"\" { // MUTANT: an unpinned retired carrier falls through to the invoker
		return verifyLandedSurface{}, errRetiredCarrierUnpinned
	}${sep}TestResolveVerifyLandedSurfaceRefusesRetiredCarrierWithoutAPin${sep}as a proof surface with no pinned candidate"
"sealed-carrier-copied-into-the-receipt${sep}pkg/mergeadmit/reconcile.go${sep}./pkg/mergeadmit/${sep}		MergeSHA:           proof.MergeSHA,
		ContentSHA:         proof.ContentSHA,${sep}		MergeSHA:           proof.MergeSHA,
		// MUTANT: the sealed carrier is dropped, so the receipt binds content to the merge alone${sep}TestReconcileLandedSealsThePullRequestCarrierAndPublicValidateAcceptsIt${sep}full-provenance producer did not seal the content carrier"
"sealed-carrier-copied-into-the-reduced-receipt${sep}pkg/mergeadmit/reconcile.go${sep}./pkg/mergeadmit/${sep}MergeSHA: proof.MergeSHA, ContentSHA: proof.ContentSHA, PatchID: proof.PatchID,${sep}MergeSHA: proof.MergeSHA, /* MUTANT: the reduced receipt drops the sealed carrier */ PatchID: proof.PatchID,${sep}TestReconcileLandedReducedSealsThePullRequestCarrierAndPublicValidateAcceptsIt${sep}reduced-provenance producer did not seal the content carrier"
"proof-command-budget-enforced${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	if p.commands >= p.maxCommands {${sep}	if false && p.commands >= p.maxCommands { // MUTANT: the subprocess count is no longer bounded${sep}TestContentProofRefusesWhenTheCommandBudgetIsSpent${sep}the command budget did not stop the proof after"
"proof-deadline-enforced${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	if err := p.ctx.Err(); err != nil {${sep}	if err := error(nil); err != nil { // MUTANT: the shared deadline no longer stops the proof${sep}TestContentProofRefusesAfterItsDeadline${sep}an expired deadline must stop the proof before it starts a process"
"proof-output-budget-enforced${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	if len(out) > contentProofMaxOutputBytes {${sep}	if false && len(out) > contentProofMaxOutputBytes { // MUTANT: command output is no longer bounded${sep}TestContentProofRefusesOversizeCommandOutput${sep}output larger than the proof budget was accepted"
"sealed-carrier-requires-the-replay${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}		if err := p.mergedTreeIsTheReviewedResult(r); err != nil {
			return err
		}${sep}		if err := error(nil); err != nil { // MUTANT: a sealed carrier no longer has to replay to the merged tree
			return err
		}${sep}TestValidateRefusesAMergeThatDiscardedTheReviewedContent${sep}a merge that discarded every reviewed hunk was accepted"
"replayed-tree-must-equal-the-merged-tree${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	if !strings.EqualFold(replayed, landedTree) {${sep}	if false && !strings.EqualFold(replayed, landedTree) { // MUTANT: the replayed reviewed result no longer has to be what landed${sep}TestValidateRefusesAnAlteredRenameDestination${sep}an altered rename destination was accepted"
"candidate-may-not-be-the-integration-commit${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	if strings.EqualFold(candidate, r.MergeSHA) {${sep}	if false && strings.EqualFold(candidate, r.MergeSHA) { // MUTANT: a receipt may name the merge as its own reviewed candidate${sep}TestValidateRefusesTheIntegrationCommitAsItsOwnCandidate${sep}the integration commit was accepted as its own reviewed candidate"
"validation-budget-never-reset${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	if err := r.validateContentBinding(proof); err != nil {${sep}	if err := r.validateContentBindingFresh(repoDir); err != nil { // MUTANT: the content half re-mints its own allowance${sep}TestValidateSpendsOneSharedBudgetAndNeverResetsIt${sep}so a stage re-minted the budget"
"identity-read-must-be-bounded${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	return repositoryIdentityWith(p.repoDir, p.run)${sep}	return toolchild.RepositoryIdentity(p.repoDir) // MUTANT: the identity read escapes the validation budget${sep}TestValidateStopsInTheIdentityStageWhenTheBudgetIsGone${sep}belongs to the identity stage"
"stopped-probe-is-not-an-answer${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}		if probeStopped(err) {
			return err
		}${sep}		if false && probeStopped(err) { // MUTANT: a probe that never answered is reported as a proved non-ancestor
			return err
		}${sep}TestValidateDoesNotReportAStoppedProbeAsANonAncestor${sep}was reported as a proved non-ancestor"
"process-input-measured-before-start${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	if len(stdin) > contentProofMaxOutputBytes {${sep}	if false && len(stdin) > contentProofMaxOutputBytes { // MUTANT: process input is no longer measured before the process starts${sep}TestContentProofRefusesAnOversizePatchInput${sep}oversize input was accepted into a process"
"git-runs-in-an-owned-process-group${sep}pkg/sync/donereceipt.go${sep}./pkg/sync/${sep}	cmd := procsignal.CommandContext(ctx, \"git\", args...)${sep}	_ = procsignal.CommandContext // MUTANT: the owned-group constructor is bypassed, so only the direct child is signalled
	cmd := exec.CommandContext(ctx, \"git\", args...)${sep}TestBoundedGitIsBuiltForAnOwnedProcessGroup${sep}git is not started in an owned process group"
"retired-carrier-pin-binding${sep}cmd/herd/verify_landed_surface.go${sep}./cmd/herd/${sep}	got, want := strings.TrimSpace(candidate), strings.TrimSpace(surface.PinnedCandidate)
	if got != want {${sep}	got, want := strings.TrimSpace(candidate), strings.TrimSpace(surface.PinnedCandidate)
	if false && got != want { // MUTANT: a pin authorises any candidate${sep}TestRequirePinnedCandidateProvedBindsTheFallbackToThePin${sep}a retired-carrier fallback authorised by one candidate proved a different one"
"landed-proof-resets-the-injected-allowance${sep}pkg/mergeadmit/reconcile.go${sep}./cmd/herd/${sep}	// one; only an unbudgeted caller gets the defaults installed here.
	ctx, cancel := ensureProofBudget(ctx)${sep}	// one; only an unbudgeted caller gets the defaults installed here.
	ctx, cancel := withProofBudget(context.Background(), ProofBudget{}) // MUTANT: the caller allowance is discarded${sep}TestVerifyLandedGateStopsOnAnExhaustedAllowance${sep}an exhausted allowance proved a landing"
"origin-read-must-be-context-aware${sep}pkg/mergeadmit/probe.go${sep}./pkg/mergeadmit/${sep}	if g == nil || g.Live.OriginMainAt == nil {${sep}	if g != nil && g.Live.OriginMainAt == nil { // MUTANT: a proof route falls back to the value-only probe
		return g.Live.OriginMain.Read(role)
	}
	if g == nil || g.Live.OriginMainAt == nil {${sep}TestProofRoutesRefuseAValueOnlyOriginProbe${sep}a proof route accepted a value-only probe"
"cli-seal-must-spend-the-same-allowance${sep}cmd/herd/reviewingest.go${sep}./cmd/herd/${sep}	receipt, err := gate.ReconcileLandedContext(ctx, req)${sep}	receipt, err := gate.ReconcileLanded(req) // MUTANT: the seal mints a fresh allowance instead of spending this invocation${sep}TestRunHarvestVerifyLandedRecordsNothingWhenSealingExhausts${sep}it did not spend this invocation's budget"
"disposition-must-follow-the-seal${sep}cmd/herd/reviewingest.go${sep}./cmd/herd/${sep}	receipt, err := gate.ReconcileLandedContext(ctx, req)
	if err != nil {
		return fmt.Errorf(\"receipt reconcile: %w\", err)
	}${sep}	if _, e := hsync.WriteLandedDisposition(\".\", hsync.LandedDisposition{Ref: req.Ref, CandidateSHA: req.CandidateSHA, MergeSHA: proof.MergeSHA, Branch: branch, Method: proof.Method}); e != nil { // MUTANT: the disposition is recorded before the seal
		return e
	}
	receipt, err := gate.ReconcileLandedContext(ctx, req)
	if err != nil {
		return fmt.Errorf(\"receipt reconcile: %w\", err)
	}${sep}TestRunHarvestVerifyLandedRecordsNothingWhenSealingExhausts${sep}was recorded for a receipt that was never minted"
"candidate-tip-read-must-be-bounded${sep}cmd/herd/verify_landed_candidate.go${sep}./cmd/herd/${sep}	git := mergeadmit.BoundedGit(ctx, wtDir)
	head, err := git(\"rev-parse\", \"HEAD\")${sep}	git := mergeadmit.BoundedGit(context.Background(), wtDir) // MUTANT: the candidate tip read escapes the caller's run
	head, err := git(\"rev-parse\", \"HEAD\")${sep}TestCandidateResolutionStopsOnACancelledRun${sep}want the candidate tip read to stop on a cancelled run"
"containment-guard-must-be-bounded${sep}cmd/herd/verify_landed_candidate.go${sep}./cmd/herd/${sep}	git := mergeadmit.BoundedGit(ctx, wtDir)
	if _, err := git(\"fetch\", \"-q\", \"origin\", \"main\"); err != nil {${sep}	git := mergeadmit.BoundedGit(context.Background(), wtDir) // MUTANT: the containment guard carries an allowance of its own
	if _, err := git(\"fetch\", \"-q\", \"origin\", \"main\"); err != nil {${sep}TestCandidateResolutionSpendsTheCallersAllowance${sep}want the allowance to be refused at the containment guard's own fetch"
"failed-origin-fetch-is-a-refusal${sep}cmd/herd/verify_landed_candidate.go${sep}./cmd/herd/${sep}	if _, err := git(\"fetch\", \"-q\", \"origin\", \"main\"); err != nil {${sep}	if _, err := git(\"fetch\", \"-q\", \"origin\", \"main\"); false && err != nil { // MUTANT: a failed fetch is discarded, so a stale tracking ref answers the guard${sep}TestCandidateResolutionRefusesWhenOriginCannotBeFetched${sep}refusal must name the cause and the remedy"
"moved-origin-refused-before-any-record${sep}cmd/herd/reviewingest.go${sep}./cmd/herd/${sep}	if !strings.EqualFold(receipt.MergeSHA, proof.MergeSHA) {${sep}	if false && !strings.EqualFold(receipt.MergeSHA, proof.MergeSHA) { // MUTANT: a moved origin is no longer refused before the disposition is recorded${sep}TestVerifyLandedCompositionRefusesWhenOriginMovesBetweenReads${sep}a moved origin produced a result presenting two different integrations as one"
"carrier-lookup-failure-is-not-an-absence${sep}cmd/herd/reviewingest.go${sep}./cmd/herd/${sep}	out, err := mergeadmit.BoundedGit(ctx, repoRoot)(\"worktree\", \"list\", \"--porcelain\")
	if err != nil {
		return \"\", fmt.Errorf(\"list the worktrees of the invoking repository: %w\", err)
	}${sep}	out, err := mergeadmit.BoundedGit(ctx, repoRoot)(\"worktree\", \"list\", \"--porcelain\")
	if err != nil {
		return \"\", nil // MUTANT: a lookup that could not run is reported as an absent carrier
	}${sep}TestWorktreeForBranchSeparatesAnAbsentCarrierFromAFailedLookup${sep}as an absent carrier"
"carrier-lookup-runs-inside-the-invocation${sep}cmd/herd/reviewingest.go${sep}./cmd/herd/${sep}	out, err := mergeadmit.BoundedGit(ctx, repoRoot)(\"worktree\", \"list\", \"--porcelain\")${sep}	outBytes, err := exec.Command(\"git\", \"-C\", repoRoot, \"worktree\", \"list\", \"--porcelain\").Output() // MUTANT: the carrier lookup escapes this invocation's allowance
	out := string(outBytes)${sep}TestRunHarvestVerifyLandedRefusesWhenTheCarrierLookupCannotRun${sep}want the refusal to name the unanswered carrier lookup"
"failed-carrier-lookup-refuses-selection${sep}cmd/herd/verify_landed_surface.go${sep}./cmd/herd/${sep}	found, err := lookup(ctx, branch)
	if err != nil {${sep}	found, err := lookup(ctx, branch)
	if false && err != nil { // MUTANT: a failed lookup falls through as an absent carrier${sep}TestResolveVerifyLandedSurfaceRefusesAFailedCarrierLookup${sep}a failed carrier lookup authorised"
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
	note "report ${run_dir:t} (under VERIFY_LANDED_RECEIPT_REPORT_DIR, outside the repository)"
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
