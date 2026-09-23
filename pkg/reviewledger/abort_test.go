package reviewledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedAbortLaunch(t *testing.T, l *Ledger, opts AbortOpts) {
	t.Helper()
	if err := l.Record(RecordOpts{
		SHA: opts.SHA, Reviewer: opts.Reviewer, Lease: opts.Lease,
		Pane: opts.Pane, Task: opts.Task, SessionID: opts.SessionID,
		BuilderFamily: "google",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorAbortIsNotAVerdict(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	opts := AbortOpts{
		SHA: sha, Reviewer: "review-fac-851", Lease: "lease-1", SessionID: "sess-1",
		Reason: "quota-exhausted", Pane: "wK:p1EQ", Task: "FAC-851",
	}
	seedAbortLaunch(t, l, opts)
	if err := l.CoordinatorAbort(opts); err != nil {
		t.Fatal(err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	abortRows := 0
	for _, r := range rows {
		if r.Event == string(EventCoordinatorAbort) {
			abortRows++
			if r.Verdict != "" {
				t.Fatalf("abort must not be a verdict row: %+v", r)
			}
		}
	}
	if abortRows != 1 {
		t.Fatalf("abort rows=%d want 1 from %+v", abortRows, rows)
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
	opts := AbortOpts{SHA: sha, Reviewer: "r1", Lease: "l1", SessionID: "s1", Reason: "quota-exhausted", Pane: "p", Task: "T"}
	seedAbortLaunch(t, l, opts)
	if _, err := l.Verdict(VerdictOpts{SHA: sha, CandidateSHA: sha, Reviewer: "r1", Verdict: VerdictPASS, ReviewerFamily: "google", BuilderFamily: "openai", ArtifactDigest: strings.Repeat("c", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := l.CoordinatorAbort(opts); err == nil {
		t.Fatal("abort must refuse PASS")
	}
}

func TestCoordinatorAbortIdempotent(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	opts := AbortOpts{SHA: strings.Repeat("d", 40), Reviewer: "r", Lease: "l", SessionID: "s", Reason: "quota-exhausted", Pane: "p", Task: "T"}
	seedAbortLaunch(t, l, opts)
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
	opts := AbortOpts{
		SHA: strings.Repeat("e", 40), Reviewer: "r", Lease: "l", SessionID: "s", Reason: "quota-exhausted", Pane: "p", Task: "T",
	}
	seedAbortLaunch(t, l, opts)
	if err := l.CoordinatorAbort(opts); err != nil {
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
	opts := AbortOpts{SHA: sha, Reviewer: "r1", Lease: "l1", SessionID: "s1", Reason: "quota-exhausted", Pane: "p", Task: "T"}
	seedAbortLaunch(t, l, opts)
	if _, err := l.Verdict(VerdictOpts{SHA: sha, CandidateSHA: sha, Reviewer: "r1", Verdict: VerdictPASS, ReviewerFamily: "google", BuilderFamily: "openai", ArtifactDigest: strings.Repeat("c", 64)}); err != nil {
		t.Fatal(err)
	}
	before, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CheckCoordinatorAbort(opts); err == nil {
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

func TestCoordinatorAbortRefusesNonexistentLaunch(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	err = l.CoordinatorAbort(AbortOpts{
		SHA: strings.Repeat("1", 40), Reviewer: "r", Lease: "l", SessionID: "s", Reason: "quota-exhausted", Pane: "p", Task: "T",
	})
	if err == nil || !strings.Contains(err.Error(), "canonical launch record") {
		t.Fatalf("want nonexistent launch refuse, got %v", err)
	}
}

func TestCoordinatorAbortRefusesWrongLease(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	opts := AbortOpts{SHA: strings.Repeat("2", 40), Reviewer: "r", Lease: "lease-want", SessionID: "s", Reason: "quota-exhausted", Pane: "p", Task: "T"}
	wrong := opts
	wrong.Lease = "lease-other"
	seedAbortLaunch(t, l, wrong)
	err = l.CoordinatorAbort(opts)
	if err == nil || !strings.Contains(err.Error(), "lease does not bind") {
		t.Fatalf("want wrong lease refuse, got %v", err)
	}
}

func TestCoordinatorAbortRefusesRecordedPaneMismatch(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	opts := AbortOpts{SHA: strings.Repeat("4", 40), Reviewer: "r", Lease: "l", SessionID: "s", Reason: "quota-exhausted", Pane: "wK:p1EQ", Task: "FAC-851"}
	wrong := opts
	wrong.Pane = "wK:pOTHER"
	seedAbortLaunch(t, l, wrong)
	err = l.CheckCoordinatorAbort(opts)
	if err == nil || !strings.Contains(err.Error(), "pane does not bind") {
		t.Fatalf("want recorded pane mismatch refuse, got %v", err)
	}
}

func TestCoordinatorAbortAllowsUnrecordedLaunchPaneWhenAbortProvidesRosterPane(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	opts := AbortOpts{SHA: strings.Repeat("5", 40), Reviewer: "review-fac-851-ad78e2762013", Lease: "pool-02-1790108554212514000", SessionID: "b9e586e4-410b-47d8-8725-20ab904f4105", Reason: "quota-exhausted", Pane: "wK:p1EQ", Task: "FAC-851"}
	if err := l.Record(RecordOpts{
		SHA: opts.SHA, Reviewer: opts.Reviewer, Lease: opts.Lease, Task: opts.Task, BuilderFamily: "xai",
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.CheckCoordinatorAbort(opts); err != nil {
		t.Fatalf("unrecorded launch pane must not refuse roster pane_id: %v", err)
	}
}

func TestCoordinatorAbortRefusesWrongSession(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	opts := AbortOpts{SHA: strings.Repeat("3", 40), Reviewer: "r", Lease: "l", SessionID: "sess-want", Reason: "quota-exhausted", Pane: "p", Task: "T"}
	wrong := opts
	wrong.SessionID = "sess-other"
	seedAbortLaunch(t, l, wrong)
	err = l.CoordinatorAbort(opts)
	if err == nil || !strings.Contains(err.Error(), "session does not bind") {
		t.Fatalf("want wrong session refuse, got %v", err)
	}
}
