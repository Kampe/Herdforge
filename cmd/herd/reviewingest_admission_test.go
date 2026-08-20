package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

type fakeReviewIngestLedger struct {
	readable bool
	reviewer string
	ingests  int
}

func (f *fakeReviewIngestLedger) Ingest(reviewledger.IngestOpts) (bool, error) {
	f.ingests++
	return true, nil
}

func (f *fakeReviewIngestLedger) VerdictFor(sha string) (reviewledger.LedgerRow, bool, error) {
	if !f.readable {
		return reviewledger.LedgerRow{}, false, errors.New("simulated ledger read failure")
	}
	return reviewledger.LedgerRow{Event: string(reviewledger.EventVerdict), SHA: sha, Reviewer: f.reviewer, Verdict: "PASS"}, true, nil
}

func (f *fakeReviewIngestLedger) VerdictForReviewer(sha, reviewer string) (reviewledger.LedgerRow, bool, error) {
	return f.VerdictFor(sha)
}

func TestReviewIngestDuplicateDoesNotMoveArtifact(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "review.md")
	if err := os.WriteFile(source, []byte("verdict"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := &fakeReviewIngestLedger{readable: true, reviewer: "reviewer"}
	got, err := admitVerdictAndMove(ledger, reviewledger.IngestOpts{Verdict: reviewledger.VerdictOpts{Reviewer: "reviewer"}}, source, "review.md")
	if err != nil {
		t.Fatal(err)
	}
	if got || ledger.ingests != 0 {
		t.Fatalf("duplicate outcome = %v, ingests=%d; want false and no write", got, ledger.ingests)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("duplicate source artifact was consumed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ingested", "review.md")); !os.IsNotExist(err) {
		t.Fatalf("duplicate artifact moved to ingested: %v", err)
	}
}

func TestReviewIngestAdmissionAndMove(t *testing.T) {
	const sha = "0123456789012345678901234567890123456789"
	for _, tc := range []struct {
		name     string
		readable bool
		wantErr  bool
	}{
		{name: "read-back succeeds", readable: true},
		{name: "read-back fails closed", readable: false, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "review.md")
			if err := os.WriteFile(source, []byte("verdict"), 0o600); err != nil {
				t.Fatal(err)
			}
			ledger := &fakeReviewIngestLedger{readable: tc.readable}
			_, err := admitVerdictAndMove(ledger, reviewledger.IngestOpts{}, source, "review.md")
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "read back") {
					t.Fatalf("expected read-back failure, got %v", err)
				}
				if _, statErr := os.Stat(source); statErr != nil {
					t.Fatalf("source moved after unreadable ledger row: %v", statErr)
				}
				if _, statErr := os.Stat(filepath.Join(root, "ingested", "review.md")); !os.IsNotExist(statErr) {
					t.Fatalf("artifact was admitted despite unreadable ledger row: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, statErr := os.Stat(filepath.Join(root, "ingested", "review.md")); statErr != nil {
				t.Fatalf("artifact was not moved after readable ledger row: %v", statErr)
			}
		})
	}
}

func TestReviewIngestCollisionIsRefusedBeforeLedgerMutation(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "review.md")
	destination := filepath.Join(root, "ingested", "same-sha-new-reviewer.md")
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new verdict"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("older verdict"), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := &fakeReviewIngestLedger{readable: true}
	if _, err := admitVerdictAndMove(ledger, reviewledger.IngestOpts{}, source, filepath.Base(destination)); err == nil || !strings.Contains(err.Error(), "preflight") {
		t.Fatalf("collision error = %v, want preflight refusal", err)
	}
	if ledger.ingests != 0 {
		t.Fatalf("collision wrote %d ledger rows, want 0", ledger.ingests)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("collision consumed source: %v", err)
	}
}
