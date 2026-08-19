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
}

func (f *fakeReviewIngestLedger) Ingest(reviewledger.IngestOpts) (bool, error) {
	return true, nil
}

func (f *fakeReviewIngestLedger) VerdictFor(sha string) (reviewledger.LedgerRow, bool, error) {
	if !f.readable {
		return reviewledger.LedgerRow{}, false, errors.New("simulated ledger read failure")
	}
	return reviewledger.LedgerRow{Event: string(reviewledger.EventVerdict), SHA: sha, Verdict: "PASS"}, true, nil
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
			_, err := admitVerdictAndMove(ledger, reviewledger.IngestOpts{}, source, sha)
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
