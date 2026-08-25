package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestOneCanonicalLedgerAuthority is the FAC-565 regression.
//
// drainLedgerPath returned <XDG_STATE_HOME>/chainseer/herd/review-ledger.jsonl
// while review-ingest, merge-admit and the inspection commands used
// .herd/review-ledger.jsonl. So drain, pulse and board-done's legacy review
// route read a DIFFERENT ledger from the one that admits verdicts: on a live
// board the state ledger held 7040 unrelated rows and lacked the freshly
// admitted PASS entirely, so an admitted candidate looked unreviewed.
func TestOneCanonicalLedgerAuthority(t *testing.T) {
	t.Setenv("HERD_REVIEW_LEDGER", "")
	t.Setenv("HERD_STATE_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// FAC-643: the requirement here is that every consumer resolves the SAME
	// ledger, which this originally expressed as a hardcoded relative string.
	// That relative form turned out to be a second way for consumers to
	// disagree -- from a standing worktree it resolved a different (or absent)
	// ledger than the project checkout did -- so agreement is now asserted
	// directly, and the path must be root-anchored so it holds from any cwd.
	inspection, drain := reviewLedgerPath(), drainLedgerPath()
	if inspection != drain {
		t.Fatalf("consumers disagree: inspection=%q drain/pulse/board-done=%q", inspection, drain)
	}
	if filepath.Base(inspection) != "review-ledger.jsonl" {
		t.Fatalf("ledger path = %q, want a path ending in review-ledger.jsonl", inspection)
	}
	if !filepath.IsAbs(inspection) {
		t.Fatalf("ledger path %q is not root-anchored, so two cwds resolve two ledgers", inspection)
	}
	// The old path hardcoded a project name, which was wrong for every other
	// repository including this one.
	if strings.Contains(drainLedgerPath(), "chainseer") {
		t.Fatalf("ledger path must not hardcode a project name: %q", drainLedgerPath())
	}
}

// An explicit override still wins, and still resolves identically everywhere.
func TestLedgerOverrideAppliesToEveryConsumer(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "admitted.jsonl")
	t.Setenv("HERD_REVIEW_LEDGER", explicit)
	if got := reviewLedgerPath(); got != explicit {
		t.Fatalf("inspection ignored the override: %q", got)
	}
	if got := drainLedgerPath(); got != explicit {
		t.Fatalf("drain ignored the override: %q", got)
	}
}
