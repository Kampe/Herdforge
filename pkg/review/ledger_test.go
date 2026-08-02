package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewReviewLedger(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "test-ledger.jsonl"))
	if err != nil {
		t.Fatalf("NewReviewLedger: %v", err)
	}
	if l.Path != filepath.Join(dir, "test-ledger.jsonl") {
		t.Errorf("Path = %s, want %s", l.Path, filepath.Join(dir, "test-ledger.jsonl"))
	}
	if l.QueuePath != filepath.Join(dir, "harvest-queue.jsonl") {
		t.Errorf("QueuePath = %s, want %s", l.QueuePath, filepath.Join(dir, "harvest-queue.jsonl"))
	}
	if _, err := os.Stat(l.Path); os.IsNotExist(err) {
		t.Error("ledger file not created")
	}
	if _, err := os.Stat(l.QueuePath); os.IsNotExist(err) {
		t.Error("queue file not created")
	}
}

func TestRecordAndShow(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))
	l.Now = func() time.Time { return fixed }

	if err := l.Record(RecordOpts{SHA: "abc123", Branch: "main", BuilderFamily: "anthropic"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	rows, err := l.AllRows()
	if err != nil {
		t.Fatalf("AllRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Event != "record" {
		t.Errorf("Event = %s, want record", r.Event)
	}
	if r.SHA != "abc123" {
		t.Errorf("SHA = %s", r.SHA)
	}
	if r.BuilderFamily != "anthropic" {
		t.Errorf("BuilderFamily = %s", r.BuilderFamily)
	}
	if r.Timestamp != fixed.Format(time.RFC3339) {
		t.Errorf("Timestamp = %s", r.Timestamp)
	}
}

func TestVerdictPASSSideWritesQueue(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))
	l.Now = func() time.Time { return fixed }

	enqueued, err := l.Verdict(VerdictOpts{
		SHA:      "abc123",
		Reviewer: "worker-1",
		Verdict:  VerdictPASS,
		Branch:   "feat/test",
	})
	if err != nil {
		t.Fatalf("Verdict: %v", err)
	}
	if !enqueued {
		t.Error("expected enqueued=true for PASS")
	}

	// Check ledger
	rows, _ := l.AllRows()
	if len(rows) != 1 || rows[0].Verdict != "PASS" {
		t.Errorf("ledger verdict = %s", rows[0].Verdict)
	}

	// Check queue
	qrows, _ := l.QueueRows()
	if len(qrows) != 1 {
		t.Fatalf("queue has %d rows, want 1", len(qrows))
	}
	if qrows[0].Event != "enqueue" {
		t.Errorf("queue event = %s, want enqueue", qrows[0].Event)
	}
	if qrows[0].Status != "queued" {
		t.Errorf("queue status = %s", qrows[0].Status)
	}
}

func TestVerdictFAILWritesRevoked(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))
	l.Now = func() time.Time { return fixed }

	enqueued, err := l.Verdict(VerdictOpts{
		SHA:      "abc123",
		Reviewer: "worker-1",
		Verdict:  VerdictFAIL,
	})
	if err != nil {
		t.Fatalf("Verdict: %v", err)
	}
	if enqueued {
		t.Error("expected enqueued=false for FAIL")
	}

	qrows, _ := l.QueueRows()
	if len(qrows) != 1 {
		t.Fatalf("queue has %d rows", len(qrows))
	}
	if qrows[0].Event != "revoked" {
		t.Errorf("queue event = %s, want revoked", qrows[0].Event)
	}
}

func TestConsumed(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))
	l.Now = func() time.Time { return fixed }

	if err := l.Consumed("abc123", "def456"); err != nil {
		t.Fatalf("Consumed: %v", err)
	}

	rows, _ := l.AllRows()
	if len(rows) != 1 || rows[0].Event != "consumed" || rows[0].MergeSHA != "def456" {
		t.Errorf("ledger: %+v", rows[0])
	}

	qrows, _ := l.QueueRows()
	if len(qrows) != 1 || qrows[0].Status != "consumed" {
		t.Errorf("queue: %+v", qrows[0])
	}
}

func TestRepair(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))
	l.Now = func() time.Time { return fixed }

	if err := l.Repair(RepairOpts{SHA: "abc123", RepairAuthor: "worker-1", Branch: "fix/test", RepairFamily: "anthropic"}); err != nil {
		t.Fatalf("Repair: %v", err)
	}

	rows, _ := l.AllRows()
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Event != "repair" || rows[0].RepairAuthor != "worker-1" || rows[0].RepairFamily != "anthropic" {
		t.Errorf("repair row: %+v", rows[0])
	}
}

func TestEnqueue(t *testing.T) {
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))
	l.Now = func() time.Time { return fixed }

	if err := l.Enqueue(EnqueueOpts{SHA: "abc123", Reviewer: "manual", Branch: "feat/test"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	qrows, _ := l.QueueRows()
	if len(qrows) != 1 || qrows[0].Event != "enqueue" || qrows[0].Status != "queued" {
		t.Errorf("queue row: %+v", qrows[0])
	}
}

func TestPending(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	// Record without verdict
	if err := l.Record(RecordOpts{SHA: "abc123", Reviewer: "worker-1"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	p, err := l.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(p) != 1 {
		t.Errorf("pending count = %d, want 1", len(p))
	}

	// Now add verdict
	l.Now = func() time.Time { return now.Add(time.Hour) }
	if _, err := l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "worker-1", Verdict: VerdictPASS}); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	p2, _ := l.Pending()
	if len(p2) != 0 {
		t.Errorf("pending after verdict = %d, want 0", len(p2))
	}
}

func TestEligible_NoRecord(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	// No data at all — Eligible should return false (no verdicts)
	ok, err := l.Eligible("abc123", "")
	if err == nil {
		t.Errorf("expected error for unknown sha, got ok=%v", ok)
	}
	if ok {
		t.Error("expected not eligible")
	}
}

func TestEligible_SimplePass(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	// Record with allowlisted builder family
	if err := l.Record(RecordOpts{SHA: "abc123", Reviewer: "worker-1", BuilderFamily: "anthropic"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// PASS verdict
	if _, err := l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "worker-1", Verdict: VerdictPASS}); err != nil {
		t.Fatalf("Verdict: %v", err)
	}

	ok, err := l.Eligible("abc123", "")
	if err != nil {
		t.Fatalf("Eligible: %v", err)
	}
	if !ok {
		t.Error("expected eligible for PASS with allowlisted builder")
	}
}

func TestEligible_FailVetos(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	l.Record(RecordOpts{SHA: "abc123", Reviewer: "worker-1", BuilderFamily: "anthropic"})
	l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "worker-1", Verdict: VerdictFAIL})

	ok, err := l.Eligible("abc123", "")
	if err == nil {
		t.Error("expected error (refuse) for FAIL verdict")
	}
	if ok {
		t.Error("expected not eligible when FAIL present")
	}
}

func TestEligible_CoordinatorSelfVerifyBlocked(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))
	l.Coordinators = map[string]struct{}{"chainseer-orchestrator": {}}

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	l.Record(RecordOpts{SHA: "abc123", Reviewer: "chainseer-orchestrator", BuilderFamily: "anthropic"})
	l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "chainseer-orchestrator", Verdict: VerdictPASS})

	ok, err := l.Eligible("abc123", "")
	if err == nil {
		t.Error("expected error for coordinator self-verification")
	}
	if !strings.Contains(err.Error(), "self-verification") {
		t.Errorf("expected self-verification error, got: %v", err)
	}
	if ok {
		t.Error("expected not eligible")
	}
}

func TestConsumedExcludesFromEligible(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	l.Record(RecordOpts{SHA: "abc123", Reviewer: "worker-1", BuilderFamily: "anthropic"})
	l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "worker-1", Verdict: VerdictPASS})
	l.Consumed("abc123", "def456")

	ok, _ := l.Eligible("abc123", "")
	if ok {
		t.Error("expected not eligible after consumed")
	}
}

func TestPassSHAs(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	// sha1: PASS
	l.Record(RecordOpts{SHA: "sha1", Reviewer: "worker-1", BuilderFamily: "anthropic"})
	l.Verdict(VerdictOpts{SHA: "sha1", Reviewer: "worker-1", Verdict: VerdictPASS})
	// sha2: FAIL (should not appear)
	l.Record(RecordOpts{SHA: "sha2", Reviewer: "worker-1", BuilderFamily: "anthropic"})
	l.Verdict(VerdictOpts{SHA: "sha2", Reviewer: "worker-1", Verdict: VerdictFAIL})

	pass, err := l.PassSHAs()
	if err != nil {
		t.Fatalf("PassSHAs: %v", err)
	}
	if len(pass) != 1 || pass[0] != "sha1" {
		t.Errorf("pass = %v, want [sha1]", pass)
	}
}

func TestVetoSHAs(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	l.Record(RecordOpts{SHA: "sha1", Reviewer: "worker-1", BuilderFamily: "anthropic"})
	l.Verdict(VerdictOpts{SHA: "sha1", Reviewer: "worker-1", Verdict: VerdictFAIL})

	v, err := l.VetoSHAs()
	if err != nil {
		t.Fatalf("VetoSHAs: %v", err)
	}
	if len(v) != 1 || v[0] != "sha1" {
		t.Errorf("veto = %v, want [sha1]", v)
	}
}

func TestTier(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	l.Record(RecordOpts{SHA: "abc123", Tier: "R1", BuilderFamily: "anthropic"})

	tier, err := l.Tier("abc123")
	if err != nil {
		t.Fatalf("Tier: %v", err)
	}
	if tier != "R1" {
		t.Errorf("tier = %s, want R1", tier)
	}
}

func TestReadRowsMissingFile(t *testing.T) {
	rows, err := readRows("/nonexistent/path.jsonl")
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty, got %d rows", len(rows))
	}
}

func TestReadRowsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.jsonl")
	os.WriteFile(p, []byte("{bad json}\n{\"event\":\"record\",\"sha\":\"abc\"}\n"), 0644)

	rows, err := readRows(p)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 valid row, got %d", len(rows))
	}
}

func TestEligible_DifferentFamilyFilter(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	l.Record(RecordOpts{SHA: "abc123", Reviewer: "worker-1",
		BuilderFamily: "anthropic", ReviewerFamily: "openai"})
	l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "worker-1",
		Verdict: VerdictPASS, ReviewerFamily: "openai"})

	// If we ask eligible with builderFamily="openai", since reviewerFamily !=
	// builderFamily, the reviewer is from a DIFFERENT family — so the sha IS
	// eligible (cross-family review qualifies).
	ok, err := l.Eligible("abc123", "anthropic")
	if err != nil {
		t.Fatalf("Eligible: %v", err)
	}
	if !ok {
		t.Error("expected eligible: openai reviewer passed for anthropic builder")
	}
}

func TestFamilyAllowlist(t *testing.T) {
	allowed := []string{"anthropic", "openai", "google", "xai", "zhipu",
		"moonshot", "alibaba", "deepseek", "open-weight", "antigravity", "proxy"}
	for _, f := range allowed {
		if !LedgerFamilyAllowlist[f] {
			t.Errorf("expected %q in allowlist", f)
		}
	}
	if LedgerFamilyAllowlist["grok"] {
		t.Error("grok should NOT be in ledger allowlist")
	}
}

func TestNewestBy(t *testing.T) {
	rows := []LedgerRow{
		{SHA: "a", Event: "record", Timestamp: "2025-01-01T00:00:00Z"},
		{SHA: "a", Event: "verdict", Timestamp: "2025-01-02T00:00:00Z"},
		{SHA: "b", Event: "record", Timestamp: "2025-01-03T00:00:00Z"},
	}
	m := newestBy(rows, func(r *LedgerRow) string { return r.SHA })
	if len(m) != 2 {
		t.Errorf("map size = %d, want 2", len(m))
	}
	if m["a"].Event != "verdict" {
		t.Errorf("newest for 'a' should be verdict, got %s", m["a"].Event)
	}
}

func TestNormalizeSHA_Passthrough(t *testing.T) {
	// When git is unavailable or sha is not a valid ref, NormalizeSHA
	// returns the input unchanged.
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))
	result := l.NormalizeSHA("deadbeef")
	if result != "deadbeef" {
		t.Errorf("expected passthrough, got %s", result)
	}
}

func TestQueuedWithPassVerdict(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	// Enqueue manually
	if err := l.Enqueue(EnqueueOpts{SHA: "abc123", Reviewer: "manual", Branch: "feat/test"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Record with PASS
	l.Record(RecordOpts{SHA: "abc123", Reviewer: "worker-1", BuilderFamily: "anthropic"})
	now = now.Add(time.Minute)
	l.Now = func() time.Time { return now }
	l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "worker-1", Verdict: VerdictPASS})

	// Queued should return the enqueued row since sha is harvestable
	q, err := l.Queued()
	if err != nil {
		t.Fatalf("Queued: %v", err)
	}
	if len(q) != 1 {
		t.Errorf("queued count = %d, want 1", len(q))
	}
	if len(q) > 0 && q[0].SHA != "abc123" {
		t.Errorf("queued sha = %s", q[0].SHA)
	}
}

func TestQueuedSkipsConsumed(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	l.Enqueue(EnqueueOpts{SHA: "abc123", Reviewer: "manual"})
	l.Record(RecordOpts{SHA: "abc123", Reviewer: "worker-1", BuilderFamily: "anthropic"})
	l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "worker-1", Verdict: VerdictPASS})
	l.Consumed("abc123", "")

	q, _ := l.Queued()
	if len(q) != 0 {
		t.Errorf("expected empty queue after consumed, got %d", len(q))
	}
}

func TestQueuedSkipsFailVerdict(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }

	l.Enqueue(EnqueueOpts{SHA: "abc123", Reviewer: "manual"})
	l.Record(RecordOpts{SHA: "abc123", Reviewer: "worker-1", BuilderFamily: "anthropic"})
	l.Verdict(VerdictOpts{SHA: "abc123", Reviewer: "worker-1", Verdict: VerdictFAIL})

	q, _ := l.Queued()
	if len(q) != 0 {
		t.Errorf("expected empty queue after FAIL, got %d", len(q))
	}
}

func TestQueueEmptyExit(t *testing.T) {
	dir := t.TempDir()
	l, _ := NewReviewLedger(dir, filepath.Join(dir, "r.jsonl"))

	q, err := l.Queued()
	if err != nil {
		t.Fatalf("Queued: %v", err)
	}
	if len(q) != 0 {
		t.Errorf("expected empty, got %d", len(q))
	}
}
