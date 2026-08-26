package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

const hmSHA = "d11b37b0000000000000000000000000000000bb"

// FAC-156: `herd harvest-merge --verdict PASS` used to be merge authority. An
// operator typing PASS on the command line IS the human-supplied provenance
// this card removes — plan.Validate() checked only that the STRING said PASS,
// never that a reviewer had said so.
func TestHarvestMergeRefusesOperatorSuppliedPass(t *testing.T) {
	for _, consent := range []string{"PASS", "pass", " Pass "} {
		v, err := harvestMergeVerdict(hmSHA, consent, false)
		if err == nil {
			t.Fatalf("--verdict %q was accepted as merge consent (got verdict %q)", consent, v)
		}
		if !strings.Contains(err.Error(), "review ledger") {
			t.Fatalf("refusal did not point the operator at the ledger: %v", err)
		}
	}
}

// A refusal from the operator is honoured in the one safe direction: stop.
func TestHarvestMergeHonoursOperatorVeto(t *testing.T) {
	for _, veto := range []string{"FAIL", "BLOCKED", "blocked"} {
		if _, err := harvestMergeVerdict(hmSHA, veto, false); err == nil {
			t.Fatalf("operator veto %q did not refuse the merge", veto)
		}
	}
}

// With no operator opinion, consent must come from the ledger — and an empty
// ledger is a refusal, not a default yes.
func TestHarvestMergeRefusesWithoutAnAdmissibleLedgerVerdict(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".herd", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if _, err := harvestMergeVerdict(hmSHA, "", false); err == nil {
		t.Fatal("an empty review ledger read as consent to merge")
	}

	// A verdict for a DIFFERENT sha is not consent for this one.
	l, err := reviewledger.NewReviewLedger(".", filepath.Join(".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	const otherSHA = "0149e5e0000000000000000000000000000000aa"
	if err := l.Record(reviewledger.RecordOpts{
		SHA: otherSHA, Reviewer: "reviewer-a", BuilderFamily: "anthropic",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R3",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: otherSHA, Reviewer: "reviewer-a", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic",
	}); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if _, err := harvestMergeVerdict(hmSHA, "", false); err == nil {
		t.Fatal("a PASS for another sha read as consent for this candidate")
	}
}

// The positive path: a cross-family independent PASS for the EXACT candidate
// is the only thing that yields consent. Without this the test above could
// pass simply because harvestMergeVerdict always refuses.
func TestHarvestMergeAcceptsLedgerPassForExactCandidate(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(".herd", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	l, err := reviewledger.NewReviewLedger(".", filepath.Join(".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	if err := l.Record(reviewledger.RecordOpts{
		SHA: hmSHA, Reviewer: "reviewer-a", BuilderFamily: "anthropic",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R3",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: hmSHA, Reviewer: "reviewer-a", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic",
	}); err != nil {
		t.Fatalf("verdict: %v", err)
	}

	v, err := harvestMergeVerdict(hmSHA, "", false)
	if err != nil {
		t.Fatalf("an admissible independent PASS was refused: %v", err)
	}
	if string(v) != "PASS" {
		t.Fatalf("verdict = %q, want PASS", v)
	}

	// And a later veto for the same candidate takes it away again.
	if err := l.Record(reviewledger.RecordOpts{
		SHA: hmSHA, Reviewer: "reviewer-b", BuilderFamily: "anthropic",
		ReviewerFamily: "google", Gate: "independent", Tier: "R3",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: hmSHA, Reviewer: "reviewer-b", Verdict: reviewledger.VerdictFAIL,
		ReviewerFamily: "google", BuilderFamily: "anthropic",
	}); err != nil {
		t.Fatalf("veto verdict: %v", err)
	}
	if _, err := harvestMergeVerdict(hmSHA, "", false); err == nil {
		t.Fatal("an unsuperseded FAIL for the exact candidate still yielded consent")
	}
}
