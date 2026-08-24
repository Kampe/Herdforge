package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The ledger records the artifact path as it was at ingest time, and reviewers on
// a second host write under a different absolute prefix. Membership must be
// decided on the basename, or every remote verdict re-ingests on every sweep.
func TestSweepUningested_MatchesOnBasenameNotFullPath(t *testing.T) {
	root := t.TempDir()
	reviewRoot := filepath.Join(root, ".herd", "review")
	inbox := filepath.Join(reviewRoot, "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"seen.md", "fresh.md"} {
		if err := os.WriteFile(filepath.Join(inbox, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ledger := filepath.Join(root, "ledger.jsonl")
	// Deliberately a DIFFERENT absolute prefix, as a second host would write.
	line := `{"event":"verdict","artifact":"/other/host/.herd/review/inbox/seen.md"}` + "\n"
	if err := os.WriteFile(ledger, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := sweepUningestedArtifacts(reviewRoot, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "fresh.md" {
		t.Fatalf("sweep = %v, want only fresh.md (seen.md is already in the ledger under another prefix)", got)
	}
}

// An unreadable ledger must not look like "nothing has ever been ingested",
// which would re-ingest the entire corpus.
func TestSweepUningested_UnreadableLedgerFailsClosed(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "a.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory where the ledger file should be: open succeeds, read fails.
	badLedger := filepath.Join(root, "ledger-as-dir")
	if err := os.MkdirAll(badLedger, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := sweepUningestedArtifacts(filepath.Join(root, ".herd", "review"), badLedger); err == nil {
		t.Fatal("an unreadable ledger must be an error, not an empty seen-set")
	}
}

// A missing inbox is a real, readable state: nothing to sweep, no error.
func TestSweepUningested_MissingInboxIsEmptyNotError(t *testing.T) {
	root := t.TempDir()
	got, err := sweepUningestedArtifacts(filepath.Join(root, ".herd", "review"), filepath.Join(root, "l.jsonl"))
	if err != nil {
		t.Fatalf("missing inbox must not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %v", got)
	}
}
