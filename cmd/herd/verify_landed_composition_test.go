package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-831 review 6c93cc2b, blocker 2. The shipped `harvest-merge --verify-landed`
// route observed a landing under one allowance, wrote a disposition, then
// reconciled under ANOTHER and proved the same landing again. These drive the
// REAL composition -- proveSealAndRecordLanded, exactly what
// runHarvestVerifyLanded calls once it has resolved the surface, candidate and
// request -- on the real hermetic origin/clone topology.
//
// The contract under test is an ORDERING one, and it is not satisfied by a
// shared allowance alone: observation can succeed, the disposition can be
// written, and the reconcile proof can then exhaust, leaving a recorded landing
// for a receipt that was never minted. The seal therefore happens BEFORE the
// disposition, so exhaustion at ANY stage leaves neither.

// compositionGate builds the gate the CLI would build, with an injectable
// allowance and a real ledger carrying an independent PASS for the candidate.
func compositionGate(t *testing.T, repo, candidate, tip string, budget mergeadmit.ProofBudget) *mergeadmit.Gate {
	t.Helper()
	ledger, err := reviewledger.NewReviewLedger(repo, filepath.Join(repo, "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if err := ledger.Record(reviewledger.RecordOpts{
		SHA: candidate, Reviewer: "reviewer-a", BuilderFamily: "anthropic", BuilderIdentity: "builder-1",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R3", Task: pinProofRef, Lease: "lease-1",
	}); err != nil {
		t.Fatalf("record launch: %v", err)
	}
	if _, err := ledger.Verdict(reviewledger.VerdictOpts{
		SHA: candidate, Reviewer: "reviewer-a", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic", Task: pinProofRef, Lease: "lease-1",
		PatchURL: "patch-1", VfyDigest: "vfy-1", Artifact: "verdict.md", CandidateSHA: candidate,
	}); err != nil {
		t.Fatalf("write verdict: %v", err)
	}
	return &mergeadmit.Gate{
		RepoDir: repo, Ledger: ledger, ProofBudget: budget,
		Policy: preflight.MergePolicy{
			Protected: true, RequiredChecks: []string{"Build, Preflight & Test Suite"},
			RequireDifferentFamilyReview: true, RequirePullRequestReviews: true,
		},
		Live: mergeadmit.LiveState{OriginMainAt: mergeadmit.StaticOriginProbe(tip)},
	}
}

// nothingRecorded is the whole point: neither artifact may exist when the
// invocation did not complete.
func nothingRecorded(t *testing.T, repo string) {
	t.Helper()
	if _, err := os.Stat(hsync.ReceiptPath(repo, pinProofRef)); err == nil {
		t.Fatal("a receipt was sealed by an invocation that did not complete")
	}
	if d, err := hsync.ReadLandedDisposition(repo, pinProofRef); err == nil && d != nil {
		t.Fatal("a landed disposition was recorded for a receipt that was never minted")
	}
}

// EARLY exhaustion: the allowance is spent during observation, before any proof
// completes. Nothing may be recorded.
func TestVerifyLandedCompositionStopsBeforeAnythingIsRecorded(t *testing.T) {
	repo, base, candidate, landed := landedPinFixture(t)
	t.Chdir(repo)
	gate := compositionGate(t, repo, candidate, landed, mergeadmit.ProofBudget{MaxCommands: 1})
	req := mergeadmit.Request{
		Ref: pinProofRef, BaseSHA: base, CandidateSHA: candidate,
		ReducedProvenance: &mergeadmit.ReducedProvenance{PullRequest: 1, VerifyLanded: true},
	}

	err := proveSealAndRecordLanded(gate, repo, "work", candidate, req)
	if err == nil {
		t.Fatal("an exhausted allowance completed a verify-landed invocation")
	}
	if !errors.Is(err, mergeadmit.ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands through errors.Is", err)
	}
	nothingRecorded(t, repo)
}

// LATE exhaustion, which a shared allowance alone does NOT prevent: enough
// budget to observe and prove, too little for the seal that follows. The
// disposition must still not exist -- that is what the seal-before-record order
// buys, and it is the case a no-disposition test placed only at observation
// would miss entirely.
func TestVerifyLandedCompositionRecordsNothingWhenTheSealExhausts(t *testing.T) {
	repo, base, candidate, landed := landedPinFixture(t)
	t.Chdir(repo)
	observed := observationCost(t, repo, base, candidate, landed)
	gate := compositionGate(t, repo, candidate, landed, mergeadmit.ProofBudget{MaxCommands: observed})
	req := mergeadmit.Request{
		Ref: pinProofRef, BaseSHA: base, CandidateSHA: candidate,
		ReducedProvenance: &mergeadmit.ReducedProvenance{PullRequest: 1, VerifyLanded: true},
	}

	err := proveSealAndRecordLanded(gate, repo, "work", candidate, req)
	if err == nil {
		t.Fatal("an allowance that could not reach the seal still completed the invocation")
	}
	nothingRecorded(t, repo)
}

// observationCost measures what observation alone spends, so the late-exhaustion
// case above is derived from the real path instead of a guessed number.
func observationCost(t *testing.T, repo, base, candidate, landed string) int {
	t.Helper()
	for n := 1; n < 64; n++ {
		gate := compositionGate(t, repo, candidate, landed, mergeadmit.ProofBudget{MaxCommands: n})
		req := mergeadmit.Request{Ref: pinProofRef, BaseSHA: base, CandidateSHA: candidate}
		ctx, cancel := gate.ProofContext()
		_, err := observeVerifyLanded(ctx, repo, gate, req)
		cancel()
		if err == nil {
			return n
		}
	}
	t.Fatal("observation never completed within a sane allowance")
	return 0
}
