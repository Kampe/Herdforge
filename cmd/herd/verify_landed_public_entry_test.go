package main

import (
	"encoding/json"
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
// end to end: a real origin/clone with a squash landing, an admission record for
// the ref, and a ledger carrying an independent PASS for the candidate.
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

	// The durable admission the request resolver prefers. Reduced provenance is
	// the shape a post-merge verify-landed reconciliation actually carries.
	rec := admissionRecord{Request: mergeadmit.Request{
		Ref: pinProofRef, CandidateSHA: candidate, BaseSHA: base,
		ReducedProvenance: &mergeadmit.ReducedProvenance{PullRequest: 1, VerifyLanded: true},
	}}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	path := admissionRecordPath(".", pinProofRef)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, candidate, verifyLandedBinding{Ref: pinProofRef, Candidate: candidate}
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
func TestRunHarvestVerifyLandedRecordsNothingWhenSealingExhausts(t *testing.T) {
	full := publicEntryAllowanceForSuccess(t)
	if full < 2 {
		t.Fatalf("a complete invocation spent %d commands; too few to place a late exhaustion", full)
	}

	repo, _, binding := publicEntryFixture(t)
	cliProofBudget = mergeadmit.ProofBudget{MaxCommands: full - 1}
	t.Cleanup(func() { cliProofBudget = mergeadmit.ProofBudget{} })

	err := runHarvestVerifyLanded("work", binding)
	if err == nil {
		t.Fatal("an allowance one command short of the whole invocation still completed it")
	}
	receipt, disposition := publicEntryArtifacts(t, repo)
	if receipt {
		t.Fatal("a receipt was sealed by an invocation that exhausted its allowance")
	}
	if disposition {
		t.Fatal("a landed disposition was recorded for a receipt that was never minted")
	}
}

// publicEntryAllowanceForSuccess finds the smallest allowance the whole public
// invocation completes on, using a fresh fixture per attempt so no attempt sees
// another's artifacts.
func publicEntryAllowanceForSuccess(t *testing.T) int {
	t.Helper()
	for n := 1; n < 256; n++ {
		ok := func() bool {
			_, _, binding := publicEntryFixture(t)
			cliProofBudget = mergeadmit.ProofBudget{MaxCommands: n}
			defer func() { cliProofBudget = mergeadmit.ProofBudget{} }()
			return runHarvestVerifyLanded("work", binding) == nil
		}()
		if ok {
			return n
		}
	}
	t.Fatal("the public entry never completed within a sane allowance")
	return 0
}
