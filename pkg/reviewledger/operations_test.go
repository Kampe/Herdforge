package reviewledger

import (
	"strings"
	"testing"
)

// FAC-656: Admit binds task, lease, patch id and verification digest, and on a
// live 2210-row ledger the keys "lease", "patch_url" and "verification_digest"
// appeared ZERO times. Admission was structurally unsatisfiable: required by
// every consumer, written by no producer, so a 1327-tip drain reported 318
// harvestable and act_harvests=0.
//
// The cause is ORDERING: the review launch writes its record row BEFORE it
// leases a pool slot, so no lease exists yet, and EnsureRecord no-ops on a
// second call so nothing could fill it in later.
func TestCompleteLaunchProvenanceAppendsASupersedingRecord(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	if err := l.EnsureRecord(RecordOpts{SHA: sha, Reviewer: "r1", Task: "feat/x", BuilderFamily: "openai"}); err != nil {
		t.Fatal(err)
	}
	// EnsureRecord cannot update: proving the original defect.
	if err := l.EnsureRecord(RecordOpts{SHA: sha, Reviewer: "r1", Task: "feat/x", BuilderFamily: "openai", Lease: "pool-03-99"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := l.AllRows()
	for _, r := range rows {
		if r.Event == string(EventRecord) && strings.TrimSpace(r.Lease) != "" {
			t.Fatal("precondition: EnsureRecord must NOT be able to add a lease, or this fix is unnecessary")
		}
	}

	if err := l.CompleteLaunchProvenance(RecordOpts{
		SHA: sha, Reviewer: "r1", Task: "feat/x", Lease: "pool-03-99", PatchURL: "abc123",
	}); err != nil {
		t.Fatalf("complete provenance: %v", err)
	}

	// Admission takes the LAST matching record row, so the completed one wins
	// while the original stays exactly as written: an append, not an amendment.
	rows, _ = l.AllRows()
	var last *LedgerRow
	for i := range rows {
		if rows[i].Event == string(EventRecord) && rows[i].SHA == sha {
			last = &rows[i]
		}
	}
	if last == nil {
		t.Fatal("no record row")
	}
	if last.Lease != "pool-03-99" || last.PatchURL != "abc123" {
		t.Fatalf("the superseding row must carry the bindings: %+v", last)
	}
	// Append-only: the ORIGINAL row survives untouched, still carrying the empty
	// lease it was written with. Completion supersedes it; it does not rewrite it.
	var recs []LedgerRow
	for _, r := range rows {
		if r.Event == string(EventRecord) && r.SHA == sha {
			recs = append(recs, r)
		}
	}
	if len(recs) != 2 {
		t.Fatalf("expected the original plus one completing row, got %d", len(recs))
	}
	if strings.TrimSpace(recs[0].Lease) != "" {
		t.Error("the original row was rewritten; this must be append-only")
	}
	// Provenance is inherited, never re-asserted, so completion cannot launder a
	// different builder family onto a candidate.
	if recs[1].BuilderFamily != recs[0].BuilderFamily {
		t.Errorf("completion changed builder family %q -> %q; that is a laundering path",
			recs[0].BuilderFamily, recs[1].BuilderFamily)
	}
}

// A row asserting an empty binding would satisfy the SHAPE of the admission gate
// while proving nothing, which is worse than the honest absence it replaces.
func TestCompleteLaunchProvenanceRefusesEmptyBindings(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, DefaultPath(dir))
	sha := strings.Repeat("b", 40)
	if err := l.EnsureRecord(RecordOpts{SHA: sha, Reviewer: "r1", Task: "feat/y", BuilderFamily: "openai"}); err != nil {
		t.Fatal(err)
	}
	err := l.CompleteLaunchProvenance(RecordOpts{SHA: sha, Reviewer: "r1"})
	if err == nil {
		t.Fatal("an empty lease AND empty patch id must be refused, not appended")
	}
	if !strings.Contains(err.Error(), "proving nothing") {
		t.Errorf("the refusal must say why an empty binding is worse than absence: %v", err)
	}
	if err := l.CompleteLaunchProvenance(RecordOpts{SHA: sha, Reviewer: "r1", Lease: "pool-01-1"}); err != nil {
		t.Errorf("a real lease alone must be recordable: %v", err)
	}
}

// FAC-659: a backfill may add EVIDENCE about a verdict; it may never change what
// the verdict SAID. A backfill that could turn a FAIL into a PASS would be far
// worse than the gap it closes, so the verdict value, reviewer and families are
// inherited from the row being completed and cannot be supplied by the caller.
func TestCompleteVerdictProvenanceCannotChangeTheVerdict(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("c", 40)
	if err := l.EnsureRecord(RecordOpts{SHA: sha, Reviewer: "r1", Task: "CHA-1", BuilderFamily: "openai"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "r1", Verdict: VerdictFAIL, BuilderFamily: "openai"}); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}

	if err := l.CompleteVerdictProvenance(sha, "r1", "CHA-1", "patch123", "digest456", ""); err != nil {
		t.Fatalf("complete: %v", err)
	}

	rows, _ := l.AllRows()
	var last *LedgerRow
	for i := range rows {
		if rows[i].Event == string(EventVerdict) && rows[i].SHA == sha && rows[i].Reviewer == "r1" {
			last = &rows[i]
		}
	}
	if last == nil {
		t.Fatal("no verdict row")
	}
	if last.Verdict != string(VerdictFAIL) {
		t.Fatalf("the verdict VALUE must be inherited, got %q from a FAIL", last.Verdict)
	}
	if last.PatchURL != "patch123" || last.VerificationDigest != "digest456" {
		t.Errorf("recoverable bindings must be written: %+v", last)
	}
	if strings.TrimSpace(last.Lease) != "" {
		t.Error("a historical lease is unrecoverable and must never be invented")
	}
	if last.Gate != GateBackfilledProvenance {
		t.Errorf("a reconstructed binding must be distinguishable from one recorded at launch, gate=%q", last.Gate)
	}
}

// Re-running a backfill must not grow the ledger once there is nothing new.
func TestCompleteVerdictProvenanceIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, DefaultPath(dir))
	sha := strings.Repeat("d", 40)
	l.EnsureRecord(RecordOpts{SHA: sha, Reviewer: "r1", Task: "CHA-1", BuilderFamily: "openai"})
	l.Verdict(VerdictOpts{SHA: sha, Reviewer: "r1", Verdict: VerdictPASS, BuilderFamily: "openai"})
	for i := 0; i < 3; i++ {
		if err := l.CompleteVerdictProvenance(sha, "r1", "CHA-1", "p", "d", ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := l.AllRows()
	var n int
	for _, r := range rows {
		if r.Event == string(EventVerdict) && r.SHA == sha {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected the original plus ONE completion, got %d; a repeated backfill must not grow the ledger", n)
	}
}
