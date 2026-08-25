package main

import (
	"path/filepath"
	"testing"
)

// FAC-643: this test previously asserted the bare cwd-relative
// filepath.Join(".herd", "review-ledger.jsonl"). That pinned a defect rather
// than a requirement -- it restated the implementation with no reason why the
// caller's cwd should decide which ledger the fleet reads. herd-smith disproved
// it with one binary and two cwds seconds apart: from a standing worktree
// inbox_uningested=0, from the project checkout 102, with 123 files in the
// canonical inbox. The default must be root-anchored so every consumer (pulse,
// drain, candidate) resolves the same ledger.
func TestReviewLedgerPathDefaultsToRepositoryLedger(t *testing.T) {
	t.Setenv("HERD_REVIEW_LEDGER", "")
	got := reviewLedgerPath()
	if got == filepath.Join(".herd", "review-ledger.jsonl") {
		t.Fatal("default must not be cwd-relative: a caller below the project root then resolves a different ledger than the sweep does")
	}
	if filepath.Base(got) != "review-ledger.jsonl" {
		t.Fatalf("reviewLedgerPath() = %q, want a path ending in review-ledger.jsonl", got)
	}
}

func TestReviewLedgerPathHonorsExplicitOverride(t *testing.T) {
	const want = "./state/reviews.jsonl"
	t.Setenv("HERD_REVIEW_LEDGER", want)
	if got := reviewLedgerPath(); got != want {
		t.Fatalf("reviewLedgerPath() = %q, want %q", got, want)
	}
}
