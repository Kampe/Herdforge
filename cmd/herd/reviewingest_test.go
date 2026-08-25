package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-647: the sweep must judge ingestion by the ledger's own admission
// identity (sha+reviewer), not by artifact FILENAME. Matching basenames made a
// verdict admitted under a different filename -- a retry, a re-push from another
// host, a rename in transport -- count as un-ingested forever. Measured live: of
// 599 inbox files, 596 had a verdict row, only 300 matched by basename, and 296
// were reported as a backlog that did not exist. The sweep printed
// "admitted=296" while every line said DUPLICATE and the count never moved.
func TestSweepTreatsAVerdictedSHAAsIngestedRegardlessOfFilename(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := "abcdef123456"
	// The artifact on disk carries a DIFFERENT filename than the ledger recorded.
	onDisk := sha + "-review-retry-r3-" + sha + ".md"
	if err := os.WriteFile(filepath.Join(inbox, onDisk), []byte("verdict"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "review-ledger.jsonl")
	row := `{"event":"verdict","sha":"` + sha + `0000000000000000000000000000","reviewer":"r1","verdict":"PASS","artifact":"` + sha + `-review-original-name.md"}`
	if err := os.WriteFile(ledger, []byte(row+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := sweepUningestedArtifacts(dir, ledger)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a verdicted SHA must count as ingested even under a different filename, got %v", got)
	}
}

// A SHA with no verdict row anywhere is still a genuine backlog item, so the fix
// cannot be satisfied by reporting an empty sweep.
func TestSweepStillReportsAnArtifactWithNoVerdictRow(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "fedcba654321-review-never-ingested.md"
	if err := os.WriteFile(filepath.Join(inbox, name), []byte("verdict"), 0o644); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, "review-ledger.jsonl")
	if err := os.WriteFile(ledger, []byte(`{"event":"verdict","sha":"1111111111111111","reviewer":"r1","verdict":"PASS"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sweepUningestedArtifacts(dir, ledger)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("an artifact with no verdict row is a real backlog item, got %v", got)
	}
}

// FAC-647: a hedged family claim ("openai -- inferred, not fabricated: ...") is
// honest provenance. It routes to `unrecorded` rather than being accepted as
// proven, because the gate proves disjointness and an inference is not a proof.
func TestHedgedFamilyClaimIsHonestlyUnrecordedNotRefused(t *testing.T) {
	raw := "openai — inferred, not fabricated: both new describe-block additions name their tests `security-sentinel: ...`. No stronger per-commit attribution exists."
	family, honest := honestlyUnrecordedFamily(raw)
	if !honest {
		t.Fatal("a hedged family claim must be admitted as honestly unrecorded, not refused as unprovable")
	}
	if family != reviewledger.FamilyUnrecorded {
		t.Fatalf("a hedged claim must NOT be upgraded to a proven family, got %q", family)
	}
}

// A clean bare family must not be laundered into `unrecorded`.
func TestBareFamilyIsNotTreatedAsUnrecorded(t *testing.T) {
	for _, f := range []string{"openai", "anthropic", "xai", "google"} {
		if _, honest := honestlyUnrecordedFamily(f); honest {
			t.Errorf("bare family %q must resolve normally, not as unrecorded", f)
		}
	}
	// And a near-miss typo is still refused (the FAC-628 guarantee).
	if _, honest := honestlyUnrecordedFamily("anthropc"); honest {
		t.Error("a typo must not be accepted as honest provenance")
	}
}
