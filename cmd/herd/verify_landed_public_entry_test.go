package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-831 review 6c93cc2b, blocker 2, through the PUBLIC ENTRY.
//
// runHarvestVerifyLanded is what ships: it resolves the surface, resolves the
// candidate, resolves the request, BUILDS THE GATE, and only then proves, seals
// and records. A fixture that calls the composition helper directly skips the
// constructor under test, so these drive the real entry and let the real
// buildMergeGate supply the allowance.
//
// The retired-carrier route is used deliberately: no worktree stands on the
// reviewed branch, the candidate is pinned explicitly, and the invoking checkout
// stands in -- which is the exact shape this repair was written for.

// publicEntryFixture prepares a repository where runHarvestVerifyLanded can run
// end to end: a real origin/clone with a squash landing, a reduced-provenance
// binding, and a ledger carrying an independent PASS for the candidate.
func publicEntryFixture(t *testing.T) (repo, candidate string, binding verifyLandedBinding) {
	t.Helper()
	repo, base, candidate, _ := landedPinFixture(t)
	t.Chdir(repo)

	// A missing config file yields the default protected policy, which is what
	// production uses when a repository has not overridden it.
	t.Setenv("HERD_CONFIG_PATH", filepath.Join(repo, "absent-herd.yaml"))
	ledgerPath := filepath.Join(repo, ".herd", "review-ledger.jsonl")
	t.Setenv("HERD_REVIEW_LEDGER", ledgerPath)
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}

	ledger, err := reviewledger.NewReviewLedger(repo, ledgerPath)
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

	// No admission record is written. resolveVerifyLandedRequest returns reduced
	// provenance directly from the binding when --pr is present, which is the
	// shape a post-merge verify-landed reconciliation actually carries, and it
	// keeps the fixture from depending on a second durable store whose absence
	// reads as "no merge-admission ... and --task-id is missing" (CI 34749406649).
	return repo, candidate, verifyLandedBinding{Ref: pinProofRef, Candidate: candidate, BaseSHA: base, PullRequest: 1}
}

func publicEntryArtifacts(t *testing.T, repo string) (receipt, disposition bool) {
	t.Helper()
	if _, err := os.Stat(hsync.ReceiptPath(repo, pinProofRef)); err == nil {
		receipt = true
	}
	if d, err := hsync.ReadLandedDisposition(repo, pinProofRef); err == nil && d != nil {
		disposition = true
	}
	return receipt, disposition
}

// POSITIVE, default allowance, through the public entry. Without this the
// negative below could pass on an unrelated prerequisite failure -- a missing
// ledger row or an unreadable admission would produce the same "no artifacts".
func TestRunHarvestVerifyLandedSealsAndRecordsOnTheDefaultAllowance(t *testing.T) {
	repo, _, binding := publicEntryFixture(t)
	cliProofBudget = mergeadmit.ProofBudget{}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	if err := runHarvestVerifyLanded("work", binding); err != nil {
		t.Fatalf("the public entry could not complete a landing it should seal: %v", err)
	}
	receipt, disposition := publicEntryArtifacts(t, repo)
	if !receipt || !disposition {
		t.Fatalf("a completed invocation left receipt=%v disposition=%v; both are required", receipt, disposition)
	}
}

// The contract the repair exists for, at the public entry: enough allowance to
// observe and prove, not enough to reach the seal. Neither artifact may exist.
//
// The boundary is DERIVED, not guessed: the sweep raises the allowance until the
// invocation completes, then re-runs one command short, so the failure is
// guaranteed to fall after observation and before the seal completes.
// The contract the repair exists for, at the public entry: an allowance that
// covers OBSERVATION and nothing more. Neither artifact may exist.
//
// The threshold is derived from an INDEPENDENT observation-only measurement, not
// from the whole composition. Measuring the composition and subtracting one is
// not an oracle: the very mutant this is meant to catch -- a seal that mints its
// own budget instead of spending this invocation's -- REDUCES what the whole
// composition costs, so the measured minimum collapses to the observation cost,
// "one less" then fails during observation with no artifacts, and the mutant
// survives a green test. A threshold that moves with the mutation cannot certify
// that the seal shared the original allowance.
//
// So the allowance is exactly what observation alone needs, and the test proves
// both halves explicitly: that observation COMPLETES on it, and that the
// invocation as a whole does NOT.
func TestRunHarvestVerifyLandedRecordsNothingWhenSealingExhausts(t *testing.T) {
	observation := observationOnlyCost(t)

	repo, _, binding := publicEntryFixture(t)
	// Half one, stated rather than assumed: on this allowance the observation
	// stage of this very fixture completes. Any failure below is therefore after
	// observation, which is what makes it a statement about the seal.
	if err := observationCompletesOn(t, binding, observation); err != nil {
		t.Fatalf("fixture invalid: observation does not complete on %d commands: %v", observation, err)
	}

	cliProofBudget = mergeadmit.ProofBudget{MaxCommands: observation}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	err := runHarvestVerifyLanded("work", binding)
	if err == nil {
		t.Fatal("the seal completed on an allowance that only covers observation; it did not spend this invocation's budget")
	}
	receipt, disposition := publicEntryArtifacts(t, repo)
	if receipt {
		t.Fatal("a receipt was sealed by an invocation that exhausted its allowance")
	}
	if disposition {
		t.Fatal("a landed disposition was recorded for a receipt that was never minted")
	}
}

// observationCompletesOn runs ONLY the observation stage of an already-prepared
// fixture, on the given allowance, with the gate the CLI itself builds.
func observationCompletesOn(t *testing.T, binding verifyLandedBinding, allowance int) error {
	t.Helper()
	prev := cliProofBudget
	cliProofBudget = mergeadmit.ProofBudget{MaxCommands: allowance}
	defer func() { cliProofBudget = prev }()

	req, err := resolveVerifyLandedRequest(binding, binding.Candidate)
	if err != nil {
		t.Fatalf("resolve request: %v", err)
	}
	gate, err := buildMergeGate(req.Ref, req.TaskID, 0)
	if err != nil {
		t.Fatalf("build gate: %v", err)
	}
	ctx, cancel := gate.ProofContext()
	defer cancel()
	_, err = observeVerifyLanded(ctx, ".", gate, req)
	return err
}

// observationOnlyCost is the smallest allowance on which the OBSERVATION stage
// alone completes, measured through the gate the CLI builds so the count matches
// what the real invocation spends before it seals.
//
// It is independent of the seal by construction: no control mutates
// observeVerifyLanded, so this number does not move when the seal does.
func observationOnlyCost(t *testing.T) int {
	t.Helper()
	for n := 1; n < 256; n++ {
		_, _, binding := publicEntryFixture(t)
		if observationCompletesOn(t, binding, n) == nil {
			return n
		}
	}
	t.Fatal("observation never completed within a sane allowance")
	return 0
}
