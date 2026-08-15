package mergeadmit

import (
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/preflight"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// writeReceipt is the persistence seam. It exists so a test can simulate the
// failure the read-back below is FOR — a write that reports success and does
// not persist what it claimed. Without the seam that check has no way to fail
// and is therefore not really tested. Same pattern as herdSubprocess in
// cmd/herd and execCommandContext in pkg/harvest.
var writeReceipt = hsync.WriteReceipt

// Complete is the post-merge half of the authority. Admit decides whether the
// merge may happen; Complete proves it DID happen, as the same content, and
// mints the durable receipt that is the only thing pkg/sync.BoardDone accepts
// as closing authority.
//
// Before FAC-156 this half did not exist: pkg/sync.WriteReceipt had no
// production caller at all, so `herd approve` could never close a card from
// evidence and every closure went through the manual override path. A gate
// nothing can satisfy is not a gate — it is a detour sign.
//
// Complete is idempotent. Re-running it after a crash between the write and
// the ledger consume re-reads the existing receipt and finishes the job rather
// than minting a second one.
func (g *Gate) Complete(d *Decision, req Request) (*hsync.CompletionReceipt, error) {
	if g == nil {
		return nil, fmt.Errorf("herd-merge-completion: no gate configured")
	}
	// A receipt may only be minted from an admitted decision. This is the
	// check that stops a caller from merging by hand and then asking for the
	// paperwork afterwards.
	if d == nil || !d.Admitted {
		return nil, fmt.Errorf("herd-merge-completion: refuse to mint a receipt without an admitted decision")
	}
	if !sameSHA(d.CandidateSHA, req.CandidateSHA) || !sameSHA(d.BaseSHA, req.BaseSHA) {
		return nil, fmt.Errorf("herd-merge-completion: decision covers candidate %s on base %s, not %s on %s",
			short(d.CandidateSHA), short(d.BaseSHA), short(req.CandidateSHA), short(req.BaseSHA))
	}
	if strings.TrimSpace(d.PolicyRevision) == "" {
		return nil, fmt.Errorf("herd-merge-completion: admitted decision carries no policy revision")
	}
	if current := preflight.PolicyRevision(g.Policy); current != d.PolicyRevision {
		return nil, fmt.Errorf("herd-merge-completion: merge policy changed after admission (admitted %s, current %s); re-run admission",
			short(d.PolicyRevision), short(current))
	}
	if strings.TrimSpace(d.VerificationDigest) == "" {
		return nil, fmt.Errorf("herd-merge-completion: admitted verdict carries no verification digest")
	}

	// Re-read the integration tip AFTER the merge. This is the same live probe
	// Admit used to assert the base had not moved; now its whole job is to
	// report where the merge actually put things.
	landed, err := g.Live.OriginMain.Read("origin_main_post_merge")
	if err != nil {
		return nil, fmt.Errorf("herd-merge-completion: %w", err)
	}

	proof, err := Prove(g.RepoDir, ProofRequest{
		Mode:         d.Mode,
		BaseSHA:      req.BaseSHA,
		CandidateSHA: req.CandidateSHA,
		LandedSHA:    landed,
	})
	if err != nil {
		return nil, fmt.Errorf("herd-merge-completion: %s: %w", CodeProofFailed, err)
	}

	repoID, err := toolchild.RepositoryIdentity(g.RepoDir)
	if err != nil {
		return nil, fmt.Errorf("herd-merge-completion: resolve repository identity: %w", err)
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
		VerificationDigest: d.VerificationDigest,
		RiskTier:           d.Tier,
		AuthorFamily:       req.AuthorFamily,
		ReviewerFamily:     d.ReviewerFam,
		Verdict:            "PASS",
		IntegrationResult:  hsync.IntegrationMerged,
	}
	receipt.Seal()

	// Idempotency: an identical sealed receipt already on disk means this ran
	// before. Anything else on disk for the same ref is a conflict and must
	// not be silently overwritten — two different receipts for one card means
	// something is wrong upstream and overwriting hides it.
	path := hsync.ReceiptPath(g.RepoDir, receipt.TaskRef)
	if existing, loadErr := hsync.LoadReceipt(path); loadErr == nil {
		if existing.Digest != receipt.Digest {
			return nil, fmt.Errorf("herd-merge-completion: %s already holds a different receipt (%s) than this merge produced (%s); refusing to overwrite",
				path, short(existing.Digest), short(receipt.Digest))
		}
	} else if !os.IsNotExist(rootCause(loadErr)) {
		// A receipt we cannot read is not a receipt we may replace.
		if _, statErr := os.Stat(path); statErr == nil {
			return nil, fmt.Errorf("herd-merge-completion: %s exists but could not be read: %w", path, loadErr)
		}
	}

	if err := writeReceipt(g.RepoDir, receipt); err != nil {
		return nil, fmt.Errorf("herd-merge-completion: persist receipt: %w", err)
	}

	// READ-BACK. A write that reported success but did not persist is the
	// exact failure mode BoardDone's provider read-back exists for; the same
	// discipline applies to our own filesystem.
	back, err := hsync.LoadReceipt(path)
	if err != nil {
		return nil, fmt.Errorf("herd-merge-completion: %s: receipt did not read back from %s: %w", CodeReceiptReadback, path, err)
	}
	if back.Digest != receipt.Digest || back.Digest != back.ComputeDigest() {
		return nil, fmt.Errorf("herd-merge-completion: %s: receipt at %s reads back as %s, not the sealed %s",
			CodeReceiptReadback, path, short(back.Digest), short(receipt.Digest))
	}

	// Spend the admission exactly once. Ordering is deliberate: the receipt is
	// durable and read back BEFORE the ledger is marked consumed, so a crash
	// between the two replays safely (Consumed is idempotent) rather than
	// leaving a spent admission with no receipt to show for it.
	if g.Ledger != nil {
		if err := g.Ledger.Consumed(req.CandidateSHA, proof.MergeSHA); err != nil {
			return nil, fmt.Errorf("herd-merge-completion: mark admission consumed: %w", err)
		}
	}
	return back, nil
}

// rootCause unwraps to the innermost error so os.IsNotExist can see through
// the fmt.Errorf wrapping LoadReceipt applies.
func rootCause(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok || u.Unwrap() == nil {
			return err
		}
		err = u.Unwrap()
	}
}
