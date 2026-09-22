package reviewledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoordinatorAbortIsNotAVerdict(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	if err := l.CoordinatorAbort(AbortOpts{
		SHA: sha, Reviewer: "review-fac-851", Lease: "lease-1", SessionID: "sess-1",
		Reason: "quota-exhausted", Pane: "wK:p1EQ", Task: "FAC-851",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Event != string(EventCoordinatorAbort) || rows[0].Verdict != "" {
		t.Fatalf("abort must not be a verdict row: %+v", rows)
	}
	if _, ok := MatchingCoordinatorAbort(rows, sha, "review-fac-851", "lease-1", "sess-1"); !ok {
		t.Fatal("abort identity not matched")
	}
	if _, ok := MatchingCoordinatorAbort(rows, sha, "review-fac-851", "lease-1", "other-sess"); ok {
		t.Fatal("session mismatch must not match")
	}
}

func TestCoordinatorAbortRefusesPASS(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	if _, err := l.Verdict(VerdictOpts{SHA: sha, CandidateSHA: sha, Reviewer: "r1", Verdict: VerdictPASS, ReviewerFamily: "google", BuilderFamily: "openai", ArtifactDigest: strings.Repeat("c", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := l.CoordinatorAbort(AbortOpts{SHA: sha, Reviewer: "r1", Lease: "l1", SessionID: "s1", Reason: "quota-exhausted"}); err == nil {
		t.Fatal("abort must refuse PASS")
	}
}

func TestCoordinatorAbortIdempotent(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	opts := AbortOpts{SHA: strings.Repeat("d", 40), Reviewer: "r", Lease: "l", SessionID: "s", Reason: "quota-exhausted"}
	if err := l.CoordinatorAbort(opts); err != nil {
		t.Fatal(err)
	}
	if err := l.CoordinatorAbort(opts); err != nil {
		t.Fatal(err)
	}
	rows, _ := l.AllRows()
	n := 0
	for _, r := range rows {
		if r.Event == string(EventCoordinatorAbort) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("abort rows=%d want 1", n)
	}
}

func TestCoordinatorAbortIncompleteIdentity(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CoordinatorAbort(AbortOpts{SHA: "a", Reviewer: "r", Lease: "l", Reason: "x"}); err == nil {
		t.Fatal("missing session must refuse")
	}
}

func TestCoordinatorAbortDoesNotEnqueueHarvest(t *testing.T) {
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.jsonl")
	l, err := NewReviewLedger(dir, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CoordinatorAbort(AbortOpts{
		SHA: strings.Repeat("e", 40), Reviewer: "r", Lease: "l", SessionID: "s", Reason: "quota-exhausted",
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(QueuePathFor(ledgerPath))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "" {
		t.Fatalf("harvest queue must stay empty after abort, got %s", data)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Event == string(EventEnqueue) || r.Event == string(EventVerdict) {
			t.Fatalf("abort leaked event %s", r.Event)
		}
	}
}

func TestCheckCoordinatorAbortRefusesPASSWithoutWrite(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("f", 40)
	if _, err := l.Verdict(VerdictOpts{SHA: sha, CandidateSHA: sha, Reviewer: "r1", Verdict: VerdictPASS, ReviewerFamily: "google", BuilderFamily: "openai", ArtifactDigest: strings.Repeat("c", 64)}); err != nil {
		t.Fatal(err)
	}
	before, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CheckCoordinatorAbort(AbortOpts{SHA: sha, Reviewer: "r1", Lease: "l1", SessionID: "s1", Reason: "quota-exhausted"}); err == nil {
		t.Fatal("check must refuse PASS")
	}
	after, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("check must not append")
	}
}
