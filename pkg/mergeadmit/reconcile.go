package mergeadmit

import (
	"fmt"
	"os"
	"strings"

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

	proof, err := ProveEquivalentLanded(g.RepoDir, ProofRequest{
		BaseSHA:      req.BaseSHA,
		CandidateSHA: req.CandidateSHA,
		LandedSHA:    landed,
	})
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
		if existing.Digest != receipt.Digest {
			return nil, fmt.Errorf("herd-merge-reconcile: %s already holds a different receipt (%s) than this reconcile produced (%s); refusing to overwrite",
				path, short(existing.Digest), short(receipt.Digest))
		}
	} else if !os.IsNotExist(rootCause(loadErr)) {
		if _, statErr := os.Stat(path); statErr == nil {
			return nil, fmt.Errorf("herd-merge-reconcile: %s exists but could not be read: %w", path, loadErr)
		}
	}

	if err := writeReceipt(g.RepoDir, receipt); err != nil {
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
	result, ledgerErr := g.Ledger.AdmitReduced(reviewledger.ReducedAdmissionOpts{CandidateSHA: req.CandidateSHA})
	if result == nil || !result.Admitted {
		reason := "review ledger refused this candidate"
		if result != nil && result.Reason != "" {
			reason = result.Reason
		} else if ledgerErr != nil {
			reason = ledgerErr.Error()
		}
		return nil, fmt.Errorf("herd-merge-reconcile: %s: %s", CodeLedgerRefused, reason)
	}
	landed, err := g.Live.OriginMain.Read("origin_main_post_merge")
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: %w", err)
	}
	proof, err := ProveEquivalentLanded(g.RepoDir, ProofRequest{BaseSHA: req.BaseSHA, CandidateSHA: req.CandidateSHA, LandedSHA: landed})
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: %s: %w", CodeProofFailed, err)
	}
	repoID, err := toolchild.RepositoryIdentity(g.RepoDir)
	if err != nil {
		return nil, fmt.Errorf("herd-merge-reconcile: resolve repository identity: %w", err)
	}
	receipt := &hsync.CompletionReceipt{RepoID: repoID, TaskRef: hsync.NormalizeRef(req.Ref), BaseSHA: proof.BaseSHA, CandidateSHA: proof.CandidateSHA, MergeSHA: proof.MergeSHA, PatchID: proof.PatchID, VerificationDigest: result.VerificationDigest, RiskTier: result.Tier, AuthorFamily: result.AuthorFamily, ReviewerFamily: result.ReviewerFamily, Verdict: "PASS", IntegrationResult: hsync.IntegrationMerged, ProvenanceMode: hsync.ProvenanceReduced, PullRequest: rp.PullRequest}
	receipt.Seal()
	path := hsync.ReceiptPath(g.RepoDir, receipt.TaskRef)
	if existing, loadErr := hsync.LoadReceipt(path); loadErr == nil {
		if existing.Digest != receipt.Digest {
			return nil, fmt.Errorf("herd-merge-reconcile: %s already holds a different receipt; refusing to overwrite", path)
		}
		return existing, nil
	} else if !os.IsNotExist(rootCause(loadErr)) {
		return nil, fmt.Errorf("herd-merge-reconcile: %s exists but could not be read: %w", path, loadErr)
	}
	if err := writeReceipt(g.RepoDir, receipt); err != nil {
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

	want, err := patchIDs(repoDir, candidateCommits)
	if err != nil {
		return nil, err
	}
	got, err := patchIDs(repoDir, landedCommits)
	if err != nil {
		return nil, err
	}

	mergeSHA, err := matchOrderedPatchSubsequence(want, got, landedCommits)
	if err != nil {
		return nil, fmt.Errorf("equivalent-patch proof failed: %w", err)
	}

	pid, err := commitPatchID(repoDir, mergeSHA)
	if err != nil {
		return nil, fmt.Errorf("patch id for proved merge commit %s: %w", short(mergeSHA), err)
	}

	return &Proof{
		Mode:         ModeRebase,
		BaseSHA:      base,
		CandidateSHA: candidate,
		LandedSHA:    landed,
		MergeSHA:     mergeSHA,
		PatchID:      pid,
		Method:       "ordered-patch-subsequence-on-landed",
	}, nil
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
