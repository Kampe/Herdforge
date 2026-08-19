package main

import (
	"path/filepath"
	"testing"
)

func TestReviewLedgerPathDefaultsToRepositoryLedger(t *testing.T) {
	t.Setenv("HERD_REVIEW_LEDGER", "")
	if got, want := reviewLedgerPath(), filepath.Join(".herd", "review-ledger.jsonl"); got != want {
		t.Fatalf("reviewLedgerPath() = %q, want %q", got, want)
	}
}

func TestReviewLedgerPathHonorsExplicitOverride(t *testing.T) {
	const want = "./state/reviews.jsonl"
	t.Setenv("HERD_REVIEW_LEDGER", want)
	if got := reviewLedgerPath(); got != want {
		t.Fatalf("reviewLedgerPath() = %q, want %q", got, want)
	}
}
