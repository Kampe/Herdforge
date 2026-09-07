package mergeadmit

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// ReconcileLanded mints (or reconciles) the sealed task-bound completion
// receipt when an exact reviewed candidate's patch is already represented on
// origin/main under a possibly different merge SHA.
//
// FAC-379: harvest-merge --verify-landed can prove landing via LandedProof
// (rebase + empty tree) even when GitHub rebase-merged or cherry-picked the
// candidate onto a new object id. Approve still needs the same sealed receipt
// Complete would have written. Admit cannot re-run after the base advanced, so
// this path re-reads the fail-closed review ledger for the exact candidate and
// proves patch-equivalence on the landed history without requiring ancestry or
// tip-tree identity.
//
// Idempotent: an identical sealed receipt already on disk finishes the job
// (including ledger consume) rather than minting a second one.
func (g *Gate) ReconcileLanded(req Request) (*hsync.CompletionReceipt, error) {
	if g == nil {
		return nil, fmt.Errorf("herd-merge-reconcile: no gate configured")
	}
	if g.Ledger == nil {
		return nil, fmt.Errorf("herd-merge-reconcile: no review ledger configured; with no ledger there is no verdict, and no verdict is not a PASS")
	}
	if req.PriorReceiptDigest != "" {
		if err := g.validatePriorReceipt(req); err != nil {
			return nil, err
		}
	}
	if req.Reconstruction != nil && req.ReducedProvenance == nil {
		return nil, fmt.Errorf("reconstruction requires explicit reduced-provenance reconciliation")
	}
	if req.ReducedProvenance != nil {
		return g.reconcileLandedReduced(req)
	}
	for _, f := range []struct{ name, val string }{
		{"ref", req.Ref},
		{"task_id", req.TaskID},
		{"provider_revision", req.ProviderRevision},
		{"acceptance_digest", req.AcceptanceDigest},
		{"candidate_sha", req.CandidateSHA},
		{"base_sha", req.BaseSHA},
		{"lease", req.Lease},
		{"patch_url", req.PatchURL},
		{"author_family", req.AuthorFamily},
		{"author_identity", req.AuthorIdentity},
	} {
		if strings.TrimSpace(f.val) == "" {
			return nil, fmt.Errorf("herd-merge-reconcile: %s is required; a field the caller cannot supply is a claim it cannot make", f.name)
		}
	}
	if req.LeaseGeneration <= 0 {
		return nil, fmt.Errorf("herd-merge-reconcile: lease_generation is required and must be positive")
	}
	if rep := preflight.CheckMergePolicy(g.Policy); !rep.OK {
		return nil, fmt.Errorf("herd-merge-reconcile: autonomous merge refused: %s", strings.Join(rep.Reasons, "; "))
	}
	wantDigest := ComputeAcceptanceDigest(req.Ref, req.TaskID, req.ProviderRevision)
	if req.AcceptanceDigest != wantDigest {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: acceptance digest %s does not bind this card at revision %s (expected %s)",
			CodeAcceptance, short(req.AcceptanceDigest), short(req.ProviderRevision), short(wantDigest))
	}

	// Fail-closed ledger read for the EXACT reviewed candidate. This is Admit's
	// durable half without the live "base has not advanced" probe — the whole
	// point of reconcile is that the base already moved when the equivalent
	// patch landed.
	result, ledgerErr := g.Ledger.Admit(reviewledger.AdmissionOpts{
		CandidateSHA:   req.CandidateSHA,
		Task:           req.Ref,
		Lease:          req.Lease,
		PatchURL:       req.PatchURL,
		AuthorFamily:   req.AuthorFamily,
		AuthorIdentity: req.AuthorIdentity,
	})
	if result == nil || !result.Admitted {
		reason := "review ledger refused this candidate"
		if result != nil && result.Reason != "" {
			reason = result.Reason
		} else if ledgerErr != nil {
			reason = ledgerErr.Error()
		}
		// Idempotent replay: after a prior success the admission is spent, but
		// the sealed receipt on disk is the durable proof. Return it when it
		// still binds this exact candidate and lease generation.
		if strings.Contains(reason, "already consumed") {
			if existing, loadErr := hsync.LoadReceipt(hsync.ReceiptPath(g.RepoDir, hsync.NormalizeRef(req.Ref))); loadErr == nil {
				if existing.Digest != "" && existing.Digest == existing.ComputeDigest() &&
					sameSHA(existing.CandidateSHA, req.CandidateSHA) &&
					existing.LeaseGeneration == req.LeaseGeneration &&
					strings.EqualFold(existing.TaskID, req.TaskID) {
					if req.PriorReceiptDigest != "" {
						if err := g.persistReconciledReceipt(req, existing); err != nil {
							return nil, err
						}
					}
					return existing, nil
				}
			}
		}
		return nil, fmt.Errorf("herd-merge-reconcile: %s: %s", CodeLedgerRefused, reason)
	}
	if strings.TrimSpace(result.VerificationDigest) == "" {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: admitted verdict carries no verification digest", CodeLedgerRefused)
	}
	if strings.TrimSpace(result.Tier) == "" {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: admitted verdict carries no risk tier", CodeLedgerRefused)
	}
	if strings.TrimSpace(result.ReviewerFamily) == "" {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: admitted verdict carries no reviewer family", CodeLedgerRefused)
	}

	landed, err := g.Live.OriginMain.Read("origin_main_post_merge")
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: %w", err)
	}

	proof, err := g.ProveLanded(req, landed)
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: %w", CodeProofFailed, err)
	}

	repoID, err := toolchild.RepositoryIdentity(g.RepoDir)
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: resolve repository identity: %w", err)
	}

	receipt := &hsync.CompletionReceipt{
		RepoID:             repoID,
		TaskRef:            hsync.NormalizeRef(req.Ref),
		TaskID:             req.TaskID,
		ProviderRevision:   req.ProviderRevision,
		LeaseGeneration:    req.LeaseGeneration,
		BaseSHA:            proof.BaseSHA,
		CandidateSHA:       proof.CandidateSHA,
		MergeSHA:           proof.MergeSHA,
		PatchID:            proof.PatchID,
		AcceptanceDigest:   req.AcceptanceDigest,
		VerificationDigest: result.VerificationDigest,
		RiskTier:           result.Tier,
		AuthorFamily:       req.AuthorFamily,
		ReviewerFamily:     result.ReviewerFamily,
		Verdict:            "PASS",
		IntegrationResult:  hsync.IntegrationMerged,
	}
	receipt.Seal()

	path := hsync.ReceiptPath(g.RepoDir, receipt.TaskRef)
	if existing, loadErr := hsync.LoadReceipt(path); loadErr == nil {
		if existing.Digest != receipt.Digest && req.PriorReceiptDigest == "" {
			return nil, fmt.Errorf("herd-merge-reconcile: %s already holds a different receipt (%s) than this reconcile produced (%s); refusing to overwrite",
				path, short(existing.Digest), short(receipt.Digest))
		}
	} else if !os.IsNotExist(rootCause(loadErr)) {
		if _, statErr := os.Stat(path); statErr == nil {
			return nil, fmt.Errorf("herd-merge-reconcile: %s exists but could not be read: %w", path, loadErr)
		}
	}

	if err := g.persistReconciledReceipt(req, receipt); err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: persist receipt: %w", err)
	}

	back, err := hsync.LoadReceipt(path)
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: receipt did not read back from %s: %w", CodeReceiptReadback, path, err)
	}
	if back.Digest != receipt.Digest || back.Digest != back.ComputeDigest() {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: receipt at %s reads back as %s, not the sealed %s",
			CodeReceiptReadback, path, short(back.Digest), short(receipt.Digest))
	}

	if err := g.Ledger.Consumed(req.CandidateSHA, proof.MergeSHA); err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: mark admission consumed: %w", err)
	}
	return back, nil
}

func (g *Gate) reconcileLandedReduced(req Request) (*hsync.CompletionReceipt, error) {
	rp := req.ReducedProvenance
	if strings.TrimSpace(req.Ref) == "" || strings.TrimSpace(req.CandidateSHA) == "" {
		return nil, fmt.Errorf("herd-merge-reconcile: reduced provenance requires verdict ref and exact candidate sha")
	}
	if rp.PullRequest <= 0 || !rp.VerifyLanded {
		return nil, fmt.Errorf("herd-merge-reconcile: reduced provenance requires a positive pull request number and verify-landed proof")
	}
	if strings.TrimSpace(req.BaseSHA) == "" {
		return nil, fmt.Errorf("herd-merge-reconcile: reduced provenance requires the reviewed base sha")
	}
	if rep := preflight.CheckMergePolicy(g.Policy); !rep.OK {
		return nil, fmt.Errorf("herd-merge-reconcile: autonomous merge refused: %s", strings.Join(rep.Reasons, "; "))
	}
	admission := reviewledger.ReducedAdmissionOpts{CandidateSHA: req.CandidateSHA}
	if req.Reconstruction != nil || req.PriorReceiptDigest != "" {
		existing, err := hsync.LoadReceipt(hsync.ReceiptPath(g.RepoDir, hsync.NormalizeRef(req.Ref)))
		if err == nil && existing.Digest != "" && existing.Digest == existing.ComputeDigest() &&
			existing.TaskRef == hsync.NormalizeRef(req.Ref) && existing.CandidateSHA == req.CandidateSHA &&
			existing.BaseSHA == req.BaseSHA && existing.PullRequest == rp.PullRequest &&
			((req.Reconstruction == nil && existing.ReconstructedSHA == "") || (req.Reconstruction != nil && existing.ReconstructedSHA == req.Reconstruction.SHA &&
				existing.ReconstructionBaseSHA == req.Reconstruction.BaseSHA && existing.ReconstructionDigest == req.Reconstruction.AttestationDigest)) {
			admission.ReconcileConsumedMergeSHA = existing.MergeSHA
		}
	}
	result, ledgerErr := g.Ledger.AdmitReduced(admission)
	if result == nil || !result.Admitted {
		reason := "review ledger refused this candidate"
		if result != nil && result.Reason != "" {
			reason = result.Reason
		} else if ledgerErr != nil {
			reason = ledgerErr.Error()
		}
		return nil, fmt.Errorf("herd-merge-reconcile: %s: %s", CodeLedgerRefused, reason)
	}
	if req.Reconstruction != nil || req.PriorReceiptDigest != "" {
		verdict, found, err := g.Ledger.VerdictForReviewer(req.CandidateSHA, result.Reviewer)
		if err != nil || !found || reviewledger.CloseableCardRef(req.Ref) == "" || reviewledger.CloseableCardRef(verdict.Task) != reviewledger.CloseableCardRef(req.Ref) {
			return nil, fmt.Errorf("herd-merge-reconcile: review consent does not name this task")
		}
		readiness, err := g.Ledger.MergeReadinessFor(req.CandidateSHA)
		if err != nil {
			return nil, fmt.Errorf("herd-merge-reconcile: reconstruction readiness: %w", err)
		}
		if !readiness.Ready {
			return nil, fmt.Errorf("herd-merge-reconcile: reconstruction current verdict gate refused: %s", readiness.Reason)
		}
	}
	landed, err := g.Live.OriginMain.Read("origin_main_post_merge")
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: %w", err)
	}
	proof, err := g.ProveLanded(req, landed)
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: %w", CodeProofFailed, err)
	}
	repoID, err := toolchild.RepositoryIdentity(g.RepoDir)
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: resolve repository identity: %w", err)
	}
	receipt := &hsync.CompletionReceipt{RepoID: repoID, TaskRef: hsync.NormalizeRef(req.Ref), BaseSHA: proof.BaseSHA, CandidateSHA: proof.CandidateSHA, MergeSHA: proof.MergeSHA, PatchID: proof.PatchID, VerificationDigest: result.VerificationDigest, RiskTier: result.Tier, AuthorFamily: result.AuthorFamily, ReviewerFamily: result.ReviewerFamily, Verdict: "PASS", IntegrationResult: hsync.IntegrationMerged, ProvenanceMode: hsync.ProvenanceReduced, PullRequest: rp.PullRequest}
	if req.Reconstruction != nil {
		receipt.CandidateSHA = req.CandidateSHA
		receipt.BaseSHA = req.BaseSHA
		receipt.ReconstructedSHA = proof.CandidateSHA
		receipt.ReconstructionBaseSHA = proof.BaseSHA
		receipt.ReconstructionDigest = req.Reconstruction.AttestationDigest
	}
	receipt.Seal()
	path := hsync.ReceiptPath(g.RepoDir, receipt.TaskRef)
	if existing, loadErr := hsync.LoadReceipt(path); loadErr == nil {
		if existing.Digest != receipt.Digest && req.PriorReceiptDigest == "" {
			return nil, fmt.Errorf("herd-merge-reconcile: %s already holds a different receipt; refusing to overwrite", path)
		}
		if req.PriorReceiptDigest == "" {
			return existing, nil
		}
	} else if !os.IsNotExist(rootCause(loadErr)) {
		return nil, fmt.Errorf("herd-merge-reconcile: %s exists but could not be read: %w", path, loadErr)
	}
	if err := g.persistReconciledReceipt(req, receipt); err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: persist receipt: %w", err)
	}
	back, err := hsync.LoadReceipt(path)
	if err != nil || back.Digest != receipt.Digest || back.Digest != back.ComputeDigest() {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: reduced receipt failed read-back", CodeReceiptReadback)
	}
	if err := g.Ledger.Consumed(req.CandidateSHA, proof.MergeSHA); err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: mark admission consumed: %w", err)
	}
	return back, nil
}

// ProveEquivalentLanded establishes that every commit in base..candidate has a
// patch-identical counterpart on the landed history (an ordered subsequence of
// base..landed). The merge SHA is the last matching landed commit — which may
// differ from the reviewed candidate SHA after a rebase or cherry-pick.
//
// Unlike ModeRebase Prove, this permits additional commits on the landed side
// after the equivalent patch (main advanced). Tip-tree identity is therefore
// not required; content identity is carried by stable patch ids.
func ProveEquivalentLanded(repoDir string, req ProofRequest) (*Proof, error) {
	base, err := resolveCommit(repoDir, req.BaseSHA, "base")
	if err != nil {
		return nil, err
	}
	candidate, err := resolveCommit(repoDir, req.CandidateSHA, "candidate")
	if err != nil {
		return nil, err
	}
	landed, err := resolveCommit(repoDir, req.LandedSHA, "landed")
	if err != nil {
		return nil, err
	}

	candidateCommits, err := rangeCommits(repoDir, base, candidate)
	if err != nil {
		return nil, err
	}
	if len(candidateCommits) == 0 {
		return nil, fmt.Errorf("candidate %s adds no commits over base %s: an empty candidate has no content to prove landed",
			short(candidate), short(base))
	}

	landedCommits, err := rangeCommits(repoDir, base, landed)
	if err != nil {
		return nil, err
	}
	if len(landedCommits) == 0 {
		return nil, fmt.Errorf("landed %s adds no commits over base %s", short(landed), short(base))
	}

	candidateContent, err := nonEmptyCommits(repoDir, candidateCommits)
	if err != nil {
		return nil, err
	}
	if len(candidateContent) == 0 {
		return nil, fmt.Errorf("candidate %s adds no content over base %s: an empty candidate has no content to prove landed",
			short(candidate), short(base))
	}

	// FAC-736: GitHub merge-commit landing (and empty worktree anchors) leave
	// administrative commits with no patch content on the landed range. Patch
	// identity is associated with the content-bearing counterpart, never the
	// empty merge tip.
	landedContent, err := nonEmptyCommits(repoDir, landedCommits)
	if err != nil {
		return nil, err
	}
	if len(landedContent) == 0 {
		return nil, fmt.Errorf("landed %s adds no content over base %s", short(landed), short(base))
	}

	want, err := patchIDs(repoDir, candidateContent)
	if err != nil {
		return nil, err
	}
	got, err := patchIDs(repoDir, landedContent)
	if err == nil {
		mergeSHA, matchErr := matchOrderedPatchSubsequence(want, got, landedContent)
		if matchErr == nil {
			return equivalentLandedProof(repoDir, base, candidate, landed, mergeSHA,
				"ordered-patch-subsequence-on-landed")
		}
		err = matchErr
	}

	// Keep the established single-content-commit predicate unchanged. The
	// combined fallback exists only for a reviewed stack whose intermediate
	// patch context was rewritten by a newer main.
	if len(candidateContent) == 1 {
		return nil, fmt.Errorf("equivalent-patch proof failed: %w", err)
	}

	if mergeSHA, found, squashErr := matchSquashRangeReplay(repoDir, base, candidate, landedContent); squashErr != nil {
		return nil, fmt.Errorf("squash proof failed: %w", squashErr)
	} else if found {
		proof, proofErr := equivalentLandedProof(repoDir, base, candidate, landed, mergeSHA, "squash-range-patch+replay-tree")
		if proof != nil {
			proof.Mode = ModeSquash
		}
		return proof, proofErr
	}

	mergeSHA, replayErr := matchCombinedRangeReplay(repoDir, base, candidate, candidateContent, landedContent)
	if replayErr != nil {
		return nil, fmt.Errorf("equivalent-patch proof failed: ordered proof: %v; combined proof: %w", err, replayErr)
	}
	return equivalentLandedProof(repoDir, base, candidate, landed, mergeSHA,
		"combined-range-replay-on-landed")
}

func equivalentLandedProof(repoDir, base, candidate, landed, mergeSHA, method string) (*Proof, error) {
	pid, err := commitPatchID(repoDir, mergeSHA)
	if err != nil {
		return nil, fmt.Errorf("patch id for proved merge commit %s: %w", short(mergeSHA), err)
	}
	return &Proof{
		Mode: ModeRebase, BaseSHA: base, CandidateSHA: candidate,
		LandedSHA: landed, MergeSHA: mergeSHA, PatchID: pid, Method: method,
	}, nil
}

// nonEmptyCommits removes generated administrative anchors while preserving
// the order of every content-bearing commit. A wholly empty range is still a
// hard refusal, enforced by the caller.
func nonEmptyCommits(repoDir string, commits []string) ([]string, error) {
	content := make([]string, 0, len(commits))
	for _, commit := range commits {
		diff, err := gitOutBytes(repoDir, "diff-tree", "-p", "--no-color", commit)
		if err != nil {
			return nil, fmt.Errorf("inspect patch for %s: %w", short(commit), err)
		}
		if len(bytes.TrimSpace(diff)) != 0 {
			content = append(content, commit)
		}
	}
	return content, nil
}

// matchCombinedRangeReplay proves the context-rewritten GitHub rebase shape.
// The candidate tip patch anchors the ordered stack endpoint. For each
// candidate-sized contiguous landed window ending at that patch, git
// merge-tree replays the exact reviewed base..candidate result onto the
// window's parent using the exact reviewed base as the merge base. Only one
// window may produce the exact landed end tree; zero is missing/altered and
// more than one is an ambiguous reconstruction.
func matchCombinedRangeReplay(repoDir, base, candidate string, candidateContent, landedCommits []string) (string, error) {
	stackLen := len(candidateContent)
	if stackLen < 2 {
		return "", fmt.Errorf("combined replay requires a multi-commit candidate")
	}
	tipPatch, err := commitPatchID(repoDir, candidateContent[stackLen-1])
	if err != nil {
		return "", fmt.Errorf("candidate tip patch: %w", err)
	}

	var matches []string
	for end := stackLen - 1; end < len(landedCommits); end++ {
		landedTipPatch, patchErr := commitPatchID(repoDir, landedCommits[end])
		if patchErr != nil || landedTipPatch != tipPatch {
			continue
		}
		start := end - stackLen + 1
		windowBase := base
		if start > 0 {
			windowBase = landedCommits[start-1]
		}
		replayedTree, mergeErr := replayReviewedTree(repoDir, base, windowBase, candidate)
		if mergeErr != nil {
			continue
		}
		landedTree, treeErr := gitOut(repoDir, "rev-parse", "--verify", "-q", landedCommits[end]+"^{tree}")
		if treeErr != nil {
			return "", fmt.Errorf("resolve landed tree %s: %w", short(landedCommits[end]), treeErr)
		}
		if replayedTree == landedTree {
			matches = append(matches, landedCommits[end])
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no candidate-sized landed stack with the reviewed tip patch reproduces the combined reviewed change")
	default:
		return "", fmt.Errorf("ambiguous reconstruction: %d landed stacks reproduce the combined reviewed change", len(matches))
	}
}

// matchOrderedPatchSubsequence finds want as an ordered subsequence of got and
// returns the landed commit corresponding to the last matched patch.
func matchOrderedPatchSubsequence(want, got, landedCommits []string) (string, error) {
	if len(want) == 0 {
		return "", fmt.Errorf("candidate range carries no patches")
	}
	if len(got) != len(landedCommits) {
		return "", fmt.Errorf("internal: landed patch count %d != commit count %d", len(got), len(landedCommits))
	}
	j := 0
	var lastMatch string
	for i, pid := range want {
		for j < len(got) && got[j] != pid {
			j++
		}
		if j >= len(got) {
			return "", fmt.Errorf("candidate commit %d (patch %s) has no patch-equivalent counterpart on the landed history",
				i+1, short(pid))
		}
		lastMatch = landedCommits[j]
		j++
	}
	return lastMatch, nil
}

// replayReviewedTree applies precisely the reviewed base-to-candidate delta to
// a landed parent. Both rebased-stack and squash proofs use this predicate.
func replayReviewedTree(repoDir, base, parent, candidate string) (string, error) {
	return gitOut(repoDir, "merge-tree", gitroot.MergeTreeWriteFlag, "--merge-base", base, parent, candidate)
}

// validatePriorReceipt does not grant review consent: the normal admission
// path still runs afterward, and persistence performs a second exact CAS.
func (g *Gate) validatePriorReceipt(req Request) error {
	if reviewledger.CloseableCardRef(req.Ref) == "" {
		return fmt.Errorf("follow-up requires an exact closeable task ref")
	}
	prior, err := hsync.LoadPriorReceipt(g.RepoDir, req.Ref, req.PriorReceiptDigest)
	if err != nil {
		return fmt.Errorf("prior receipt: %w", err)
	}
	repoID, err := toolchild.RepositoryIdentity(g.RepoDir)
	if err != nil {
		return err
	}
	if prior.Version != hsync.CompletionReceiptVersion || prior.RepoID != repoID || (prior.TaskID != "" && req.TaskID != prior.TaskID) || prior.Verdict != "PASS" || prior.IntegrationResult != hsync.IntegrationMerged {
		return fmt.Errorf("prior receipt repository/task/completion identity mismatch")
	}
	if prior.CandidateSHA == req.CandidateSHA {
		return fmt.Errorf("follow-up must name a new candidate")
	}
	if _, err := resolveCommit(g.RepoDir, prior.MergeSHA, "prior merge"); err != nil {
		return err
	}
	if _, err := resolveCommit(g.RepoDir, req.BaseSHA, "follow-up reviewed base"); err != nil {
		return err
	}
	if err := gitroot.RequireAncestor(g.RepoDir, prior.MergeSHA, req.BaseSHA); err != nil {
		return fmt.Errorf("follow-up reviewed base does not contain prior delivery: %w", err)
	}
	// Even idempotent consumed replays must not ignore a later dissent.
	ready, err := g.Ledger.MergeReadinessFor(req.CandidateSHA)
	if err != nil {
		return err
	}
	if !ready.Ready {
		return fmt.Errorf("follow-up review is not ready: %s", ready.Reason)
	}
	return nil
}

func (g *Gate) persistReconciledReceipt(req Request, receipt *hsync.CompletionReceipt) error {
	if req.PriorReceiptDigest == "" {
		return writeReceipt(g.RepoDir, receipt)
	}
	return hsync.SupersedeReceipt(g.RepoDir, receipt, req.PriorReceiptDigest)
}
