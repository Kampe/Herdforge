package candidateindex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// TestDefaultLedgerPathIsTheRealLedger is the FAC-575 gate.
//
// The default was .herd/review/ledger.jsonl, which nothing writes and which does
// not exist. The ledger scan builds a reviewledger.Ledger — the same type the
// real review ledger uses — so it was unambiguously meant to read THE review
// ledger and had been reading nothing. Step 3 of indexing was inert for every
// caller that did not pass an explicit path.
func TestDefaultLedgerPathIsTheRealLedger(t *testing.T) {
	root := t.TempDir()
	idx := New(IndexOptions{RepoRoot: root})
	opts := idx.opts

	want := reviewledger.DefaultPath(root)
	if opts.LedgerPath != want {
		t.Fatalf("default ledger = %q, want the real review ledger %q", opts.LedgerPath, want)
	}
	if strings.Contains(opts.LedgerPath, filepath.Join("review", "ledger.jsonl")) {
		t.Error("the default must not be the path nothing writes")
	}
	// The queue is only meaningful relative to its ledger.
	if got, want := opts.QueuePath, reviewledger.QueuePathFor(opts.LedgerPath); got != want {
		t.Errorf("queue = %q, want it derived from the ledger (%q)", got, want)
	}
}

// The behaviour change this repoint causes must be exercised: with real rows
// present, the index must actually see them. Before the fix it saw none, so a
// test asserting "no candidates" would have passed for the wrong reason.
func TestLedgerRowsAreIndexed(t *testing.T) {
	root := t.TempDir()
	ledgerPath := reviewledger.DefaultPath(root)
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := reviewledger.NewReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	const sha = "abc123abc123abc123abc123abc123abc123abcd"
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: "herd/fac-1", Task: "FAC-1", Reviewer: "r",
		BuilderFamily: "anthropic", ReviewerFamily: "openai",
		Gate: "independent", Tier: "R2",
	}); err != nil {
		t.Fatal(err)
	}

	opts := New(IndexOptions{RepoRoot: root}).opts
	if opts.LedgerPath != ledgerPath {
		t.Fatalf("fixture and default disagree: %q vs %q", opts.LedgerPath, ledgerPath)
	}
	// Reading the ledger through the same shape the index uses must surface the
	// row. If this returns nothing, the index is inert again.
	led := &reviewledger.Ledger{Path: opts.LedgerPath, QueuePath: opts.QueuePath}
	rows, err := led.AllRows()
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.SHA == sha {
			found = true
		}
	}
	if !found {
		t.Fatal("a recorded ledger row must be visible through the index's default path")
	}
}
