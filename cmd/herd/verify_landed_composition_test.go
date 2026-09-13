package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// Under .herd/, which landedPinFixture excludes from git: a ledger file at
	// the repository root made the worktree DIRTY, and the landing proof's
	// dirty-worktree refusal then stood in for the budget refusal these tests
	// are about (CI 34749406649).
	if err := os.MkdirAll(filepath.Join(repo, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	ledger, err := reviewledger.NewReviewLedger(repo, filepath.Join(repo, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	// Same evidence shape as the public fixture: the branch and artifact name
	// the ref, which is what the shipped review authority matches on.
	if err := ledger.Record(reviewledger.RecordOpts{
		SHA: candidate, Reviewer: "reviewer-a", BuilderFamily: "anthropic", BuilderIdentity: "builder-1",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R3", Task: pinProofRef, Lease: "lease-1",
		Branch: pinProofBranch,
	}); err != nil {
		t.Fatalf("record launch: %v", err)
	}
	if _, err := ledger.Verdict(reviewledger.VerdictOpts{
		SHA: candidate, Reviewer: "reviewer-a", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic", Task: pinProofRef, Lease: "lease-1",
		PatchURL: "patch-1", VfyDigest: "vfy-1", CandidateSHA: candidate,
		Branch: pinProofBranch, Artifact: pinProofRef + "-verdict.md",
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

	ctx, cancel := gate.ProofContext()
	defer cancel()
	err := proveSealAndRecordLanded(ctx, gate, repo, pinProofBranch, req)
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

	ctx, cancel := gate.ProofContext()
	defer cancel()
	err := proveSealAndRecordLanded(ctx, gate, repo, pinProofBranch, req)
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

// SWITCHING ORIGIN. The observation and the seal read the integration tip
// separately, so origin can move between them. Before review b70054a6 the
// disposition was written from the FIRST reading while the receipt was sealed
// against the SECOND, and the success line presented the two as one result.
//
// The contract now: the receipt is the authority, the disposition is derived
// from it, and the two readings must agree BEFORE anything is recorded. A moved
// origin is a refusal with no disposition -- never a disposition that
// contradicts the receipt it claims to describe.
//
// The two tips carry DIFFERENT INTEGRATIONS of the same reviewed work, not a
// tip that merely advanced: an empty commit on top of the same squash still
// proves the same integration commit, so the two reads would agree and the
// scenario would assert nothing. The second line re-lands the identical patch
// as a different commit, which is what makes the readings diverge.
func relandedElsewhere(t *testing.T, repo, base string) string {
	t.Helper()
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = repo
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("checkout", "-q", "-b", "relanded", base)
	git("merge", "--squash", pinProofBranch)
	// A different message yields a different object for the same tree and
	// parent, so this is a genuinely distinct integration commit.
	git("commit", "-q", "-m", "relanded elsewhere")
	tip := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")
	return tip
}

func TestVerifyLandedCompositionRefusesWhenOriginMovesBetweenReads(t *testing.T) {
	repo, base, candidate, landed := landedPinFixture(t)
	t.Chdir(repo)
	elsewhere := relandedElsewhere(t, repo, base)
	if elsewhere == landed {
		t.Fatal("fixture invalid: the second landing is the same commit as the first")
	}

	reads := 0
	gate := compositionGate(t, repo, candidate, landed, mergeadmit.ProofBudget{})
	gate.Live.OriginMainAt = func(context.Context) (string, error) {
		reads++
		if reads == 1 {
			return landed, nil // what the observation proves
		}
		return elsewhere, nil // what the seal would bind
	}
	req := mergeadmit.Request{
		Ref: pinProofRef, BaseSHA: base, CandidateSHA: candidate,
		ReducedProvenance: &mergeadmit.ReducedProvenance{PullRequest: 1, VerifyLanded: true},
	}

	ctx, cancel := gate.ProofContext()
	defer cancel()
	err := proveSealAndRecordLanded(ctx, gate, repo, pinProofBranch, req)
	if err == nil {
		t.Fatal("a moved origin produced a result presenting two different integrations as one")
	}
	if !strings.Contains(err.Error(), "LANDING MOVED") {
		t.Fatalf("err = %v, want the moved-origin refusal", err)
	}
	if reads < 2 {
		t.Fatalf("fixture invalid: the tip was read %d time(s), so the two reads never diverged", reads)
	}
	if d, derr := hsync.ReadLandedDisposition(repo, pinProofRef); derr == nil && d != nil {
		t.Fatalf("a disposition was recorded for an integration the receipt does not bind: %+v", d)
	}

	// THE SEAL STILL HAPPENED, and it binds the SECOND reading. Asserting the
	// persisted artifact -- not merely that the invocation refused -- is what
	// makes this a statement about the two readings diverging: a route that
	// refused for any other reason would have left no receipt at all, and a
	// receipt binding the first reading would mean the scenario never diverged.
	raw, rerr := os.ReadFile(hsync.ReceiptPath(repo, pinProofRef))
	if rerr != nil {
		t.Fatalf("the seal that decides the disposition did not happen: %v", rerr)
	}
	var receipt hsync.CompletionReceipt
	if uerr := json.Unmarshal(raw, &receipt); uerr != nil {
		t.Fatalf("receipt unreadable: %v", uerr)
	}
	if receipt.MergeSHA != elsewhere {
		t.Fatalf("receipt binds integration %s, want the second reading %s", receipt.MergeSHA, elsewhere)
	}
	if receipt.CandidateSHA != candidate {
		t.Fatalf("receipt binds candidate %s, want the requested %s", receipt.CandidateSHA, candidate)
	}
}
