package reviewledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time {
	return time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
}

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "test-ledger.jsonl"))
	if err != nil {
		t.Fatalf("NewReviewLedger: %v", err)
	}
	l.Now = fixedNow
	return l
}

func mustErr(err error) {
	if err != nil {
		panic(err)
	}
}

func must2(_ bool, err error) {
	if err != nil {
		panic(err)
	}
}

func TestNewReviewLedger(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatalf("NewReviewLedger: %v", err)
	}
	if l.Path != filepath.Join(dir, "ledger.jsonl") {
		t.Errorf("Path = %s, want %s", l.Path, filepath.Join(dir, "ledger.jsonl"))
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

func TestRecord(t *testing.T) {
	tests := []struct {
		name    string
		opts    RecordOpts
		wantErr bool
	}{
		{
			name: "valid independent record",
			opts: RecordOpts{
				SHA: "abc123", Branch: "main",
				BuilderFamily: "anthropic", Reviewer: "reviewer-1",
			},
			wantErr: false,
		},
		{
			name: "reject missing builder family",
			opts: RecordOpts{
				SHA: "abc123", Branch: "main",
				Reviewer: "reviewer-1",
			},
			wantErr: true,
		},
		{
			name: "reject unknown family",
			opts: RecordOpts{
				SHA: "abc123", Branch: "main",
				BuilderFamily: "unicorn", Reviewer: "reviewer-1",
			},
			wantErr: true,
		},
		{
			name: "mechanical record no family",
			opts: RecordOpts{
				SHA: "abc123", Branch: "main",
				Gate: "mechanical", Reviewer: "reviewer-1",
			},
			wantErr: false,
		},
		{
			name: "mechanical record with non-mechanical family rejected",
			opts: RecordOpts{
				SHA: "abc123", Branch: "main",
				Gate: "mechanical", Reviewer: "reviewer-1",
				BuilderFamily: "anthropic",
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLedger(t)
			err := l.Record(tc.opts)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr {
				return
			}
			rows, _ := l.AllRows()
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1", len(rows))
			}
			r := rows[0]
			if r.Event != "record" {
				t.Errorf("Event = %s", r.Event)
			}
			if r.SHA != tc.opts.SHA {
				t.Errorf("SHA = %s", r.SHA)
			}
			if r.BuilderFamily != tc.opts.BuilderFamily {
				t.Errorf("BuilderFamily = %s", r.BuilderFamily)
			}
		})
	}
}

func TestEnsureRecordIsIdempotent(t *testing.T) {
	l := newTestLedger(t)
	opts := RecordOpts{SHA: "abc123", Branch: "herd/fac-345", BuilderFamily: "openai", ReviewerFamily: "anthropic", Reviewer: "reviewer-1", Gate: "independent"}
	if err := l.EnsureRecord(opts); err != nil {
		t.Fatalf("first EnsureRecord: %v", err)
	}
	if err := l.EnsureRecord(opts); err != nil {
		t.Fatalf("second EnsureRecord: %v", err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		if row.Event == string(EventRecord) && row.SHA == opts.SHA && row.Reviewer == opts.Reviewer {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("record count = %d, want 1", count)
	}
}

func TestIngestEnsuresExactAdmissionBeforePassAndIsIdempotent(t *testing.T) {
	l := newTestLedger(t)
	const sha = "0123456789012345678901234567890123456789"
	opts := IngestOpts{
		Record: RecordOpts{
			SHA: sha, Branch: "herd/fac-372", BuilderFamily: "anthropic",
			ReviewerFamily: "openai", Reviewer: "reviewer-foreign",
			Artifact: ".herd/review/inbox/verdict.md", Gate: "independent",
		},
		Verdict: VerdictOpts{
			SHA: sha, Reviewer: "reviewer-foreign", Verdict: VerdictPASS,
			ReviewerFamily: "openai", BuilderFamily: "anthropic",
			Artifact: ".herd/review/inbox/verdict.md", Branch: "herd/fac-372",
		},
	}
	for attempt := 0; attempt < 2; attempt++ {
		enqueued, err := l.Ingest(opts)
		if err != nil {
			t.Fatalf("ingest attempt %d: %v", attempt+1, err)
		}
		if attempt == 0 && !enqueued {
			t.Fatalf("first ingest did not report PASS queued")
		}
		if attempt == 1 && enqueued {
			t.Fatalf("duplicate ingest reported PASS queued")
		}
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	records, verdicts := 0, 0
	for _, row := range rows {
		if row.SHA != sha || row.Reviewer != opts.Verdict.Reviewer {
			continue
		}
		switch row.Event {
		case string(EventRecord):
			records++
		case string(EventVerdict):
			verdicts++
		}
	}
	if records != 1 || verdicts != 1 {
		t.Fatalf("exact admission/verdict rows = %d/%d, want 1/1; rows=%+v", records, verdicts, rows)
	}
}

func TestIngestRefusesPassWithoutAuthenticatedExactProvenance(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*IngestOpts)
	}{
		{"sha mismatch", func(o *IngestOpts) { o.Verdict.SHA = "ffffffffffffffffffffffffffffffffffffffff" }},
		{"reviewer mismatch", func(o *IngestOpts) { o.Verdict.Reviewer = "other-reviewer" }},
		{"missing branch", func(o *IngestOpts) { o.Record.Branch = "" }},
		{"missing artifact", func(o *IngestOpts) { o.Record.Artifact = "" }},
		{"artifact mismatch", func(o *IngestOpts) { o.Verdict.Artifact = ".herd/review/inbox/other.md" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLedger(t)
			opts := IngestOpts{
				Record:  RecordOpts{SHA: "0123456789012345678901234567890123456789", Branch: "herd/fac-372", BuilderFamily: "anthropic", ReviewerFamily: "openai", Reviewer: "reviewer-foreign", Artifact: ".herd/review/inbox/verdict.md", Gate: "independent"},
				Verdict: VerdictOpts{SHA: "0123456789012345678901234567890123456789", Reviewer: "reviewer-foreign", Verdict: VerdictPASS, ReviewerFamily: "openai", BuilderFamily: "anthropic", Artifact: ".herd/review/inbox/verdict.md"},
			}
			tc.mutate(&opts)
			if _, err := l.Ingest(opts); err == nil {
				t.Fatal("invalid provenance admitted")
			}
			rows, err := l.AllRows()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("invalid provenance wrote ledger rows: %+v", rows)
			}
		})
	}
}

func TestVerdict(t *testing.T) {
	t.Run("PASS enqueues and queue row is written", func(t *testing.T) {
		l := newTestLedger(t)
		enqueued, err := l.Verdict(VerdictOpts{
			SHA: "abc123", Reviewer: "worker-1",
			Verdict: VerdictPASS, Branch: "feat/test",
		})
		if err != nil {
			t.Fatalf("Verdict: %v", err)
		}
		if !enqueued {
			t.Error("expected enqueued=true for PASS")
		}
		rows, _ := l.AllRows()
		if len(rows) != 1 || rows[0].Verdict != "PASS" {
			t.Errorf("ledger row = %+v", rows[0])
		}
		qrows, _ := l.QueueRows()
		if len(qrows) != 1 {
			t.Fatalf("queue has %d rows, want 1", len(qrows))
		}
		if qrows[0].Event != "enqueue" || qrows[0].Status != "queued" {
			t.Errorf("queue row = %+v", qrows[0])
		}
	})

	t.Run("FAIL revokes and no enqueue", func(t *testing.T) {
		l := newTestLedger(t)
		enqueued, err := l.Verdict(VerdictOpts{
			SHA: "abc123", Reviewer: "worker-1",
			Verdict: VerdictFAIL,
		})
		if err != nil {
			t.Fatalf("Verdict: %v", err)
		}
		if enqueued {
			t.Error("expected enqueued=false for FAIL")
		}
		qrows, _ := l.QueueRows()
		if len(qrows) != 1 {
			t.Fatalf("queue has %d rows, want 1", len(qrows))
		}
		if qrows[0].Event != "revoked" || qrows[0].Status != "revoked" {
			t.Errorf("queue row = %+v", qrows[0])
		}
	})

	t.Run("family fields are preserved in row", func(t *testing.T) {
		l := newTestLedger(t)
		_, err := l.Verdict(VerdictOpts{
			SHA: "abc123", Reviewer: "worker-1",
			Verdict:        VerdictPASS,
			ReviewerFamily: "google",
			BuilderFamily:  "anthropic",
		})
		if err != nil {
			t.Fatalf("Verdict: %v", err)
		}
		rows, _ := l.AllRows()
		if rows[0].ReviewerFamily != "google" {
			t.Errorf("ReviewerFamily = %s", rows[0].ReviewerFamily)
		}
		if rows[0].BuilderFamily != "anthropic" {
			t.Errorf("BuilderFamily = %s", rows[0].BuilderFamily)
		}
	})
}

func TestEligible(t *testing.T) {
	passSHA := "abc123"
	failSHA := "def456"
	mechSHA := "ghi789"
	unreviewedSHA := "jkl012"
	coordSHA := "mno345"
	sameFamilySHA := "pqr678"

	tests := []struct {
		name          string
		setup         func(t *testing.T, l *Ledger)
		sha           string
		builderFamily string
		wantEligible  bool
		wantErr       bool
		wantErrMsg    string
	}{
		{
			name: "PASS from cross-family reviewer is eligible",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: passSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
				must2(l.Verdict(VerdictOpts{SHA: passSHA, Reviewer: "reviewer-1", Verdict: VerdictPASS, ReviewerFamily: "google"}))
			},
			sha: passSHA, wantEligible: true,
		},
		{
			name: "FAIL blocks eligibility",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: failSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
				must2(l.Verdict(VerdictOpts{SHA: failSHA, Reviewer: "reviewer-1", Verdict: VerdictFAIL, ReviewerFamily: "google"}))
			},
			sha: failSHA, wantEligible: false, wantErr: true, wantErrMsg: "veto",
		},
		{
			name: "mechanical PASS is eligible",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: mechSHA, Branch: "main", Gate: "mechanical", Reviewer: "mechanical"}))
				must2(l.Verdict(VerdictOpts{SHA: mechSHA, Reviewer: "mechanical", Verdict: VerdictPASS}))
			},
			sha: mechSHA, wantEligible: true,
		},
		{
			name: "no verdict at all is not eligible",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: unreviewedSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
			},
			sha: unreviewedSHA, wantEligible: false, wantErr: true,
		},
		{
			name: "coordinator self-verification is refused",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: coordSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "chainseer-orchestrator"}))
				must2(l.Verdict(VerdictOpts{SHA: coordSHA, Reviewer: "chainseer-orchestrator", Verdict: VerdictPASS}))
			},
			sha: coordSHA, wantEligible: false, wantErr: true, wantErrMsg: "coordinator",
		},
		{
			name: "same family review is not eligible for that family",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: sameFamilySHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
				must2(l.Verdict(VerdictOpts{SHA: sameFamilySHA, Reviewer: "reviewer-1", Verdict: VerdictPASS, ReviewerFamily: "anthropic"}))
			},
			sha: sameFamilySHA, builderFamily: "anthropic", wantEligible: false, wantErr: true,
		},
		{
			name: "same-family PASS not eligible when builderFamily query is empty",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: passSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
				must2(l.Verdict(VerdictOpts{SHA: passSHA, Reviewer: "reviewer-1", Verdict: VerdictPASS, ReviewerFamily: "anthropic"}))
			},
			sha: passSHA, wantEligible: false, wantErr: true,
		},
		{
			name: "PASS from cross-family + FAIL from another blocks eligibility (veto overrides PASS)",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: passSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
				must2(l.Verdict(VerdictOpts{SHA: passSHA, Reviewer: "reviewer-1", Verdict: VerdictPASS, ReviewerFamily: "google"}))
				mustErr(l.Record(RecordOpts{SHA: passSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-2"}))
				must2(l.Verdict(VerdictOpts{SHA: passSHA, Reviewer: "reviewer-2", Verdict: VerdictFAIL, ReviewerFamily: "google"}))
			},
			sha: passSHA, wantEligible: false, wantErr: true, wantErrMsg: "veto",
		},
		{
			name: "BLOCKED from one reviewer blocks eligibility",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: passSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
				must2(l.Verdict(VerdictOpts{SHA: passSHA, Reviewer: "reviewer-1", Verdict: VerdictBLOCKED, ReviewerFamily: "google"}))
			},
			sha: passSHA, wantEligible: false, wantErr: true, wantErrMsg: "veto",
		},
		{
			name: "consumed sha is not eligible",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: passSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
				must2(l.Verdict(VerdictOpts{SHA: passSHA, Reviewer: "reviewer-1", Verdict: VerdictPASS, ReviewerFamily: "google"}))
				mustErr(l.Consumed(passSHA, "deadbeef"))
			},
			sha: passSHA, wantEligible: false, wantErr: true, wantErrMsg: "consumed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLedger(t)
			tc.setup(t, l)
			eligible, err := l.Eligible(tc.sha, tc.builderFamily)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if eligible != tc.wantEligible {
				t.Errorf("eligible = %v, want %v", eligible, tc.wantEligible)
			}
			if tc.wantErrMsg != "" && err != nil && !strings.Contains(err.Error(), tc.wantErrMsg) {
				t.Errorf("error = %q, want contains %q", err.Error(), tc.wantErrMsg)
			}
		})
	}
}

func TestPending(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Record(RecordOpts{SHA: "abc123", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
	mustErr(l.Record(RecordOpts{SHA: "def456", Branch: "main", BuilderFamily: "google", Reviewer: "reviewer-2"}))
	must2(l.Verdict(VerdictOpts{SHA: "def456", Reviewer: "reviewer-2", Verdict: VerdictPASS}))

	pending, err := l.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending, want 1", len(pending))
	}
	if pending[0].SHA != "abc123" {
		t.Errorf("pending SHA = %s, want abc123", pending[0].SHA)
	}
}

func TestRetireCoordinatorRationaleSettlesBranchWithoutReviewVerdict(t *testing.T) {
	l := newTestLedger(t)
	sha := "retired-sha"
	mustErr(l.Record(RecordOpts{SHA: sha, Branch: "herd/fac-411", BuilderFamily: "anthropic", Reviewer: "worker-1"}))
	if err := l.Retire(RetireOpts{
		SHA: sha, Branch: "herd/fac-411", Authority: "coordinator",
		Reason:   "all commits are already landed or destructive stale content",
		Artifact: ".herd/review/blocked/fac-411-RETIRED-coordinator-rationale.md",
	}); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	rows, err := l.AllRows()
	if err != nil {
		t.Fatalf("AllRows: %v", err)
	}
	if len(rows) != 2 || rows[1].Event != string(EventRetired) {
		t.Fatalf("rows = %+v, want record followed by retired event", rows)
	}
	if rows[1].Authority != "coordinator" || rows[1].Reason == "" || rows[1].Artifact == "" {
		t.Fatalf("retirement provenance not preserved: %+v", rows[1])
	}
	if pending, err := l.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("Pending = %+v, %v; retired branch must be settled", pending, err)
	}
	if queued, err := l.Queued(); err != nil || len(queued) != 0 {
		t.Fatalf("Queued = %+v, %v; retired branch must not drain", queued, err)
	}
	if eligible, err := l.Eligible(sha, ""); err == nil || eligible || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("Eligible = %v, %v; want retired refusal", eligible, err)
	}
}

func TestPendingOrder(t *testing.T) {
	l := newTestLedger(t)
	now := fixedNow()
	l.Now = func() time.Time { return now }
	mustErr(l.Record(RecordOpts{SHA: "aaaa", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
	l.Now = func() time.Time { return now.Add(time.Hour) }
	mustErr(l.Record(RecordOpts{SHA: "bbbb", Branch: "main", BuilderFamily: "google", Reviewer: "reviewer-2"}))

	pending, _ := l.Pending()
	if len(pending) != 2 {
		t.Fatalf("got %d pending, want 2", len(pending))
	}
	if pending[0].SHA != "aaaa" || pending[1].SHA != "bbbb" {
		t.Errorf("order: %s %s, want aaaa bbbb", pending[0].SHA, pending[1].SHA)
	}
}

func TestQueued(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Record(RecordOpts{SHA: "abc123", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
	must2(l.Verdict(VerdictOpts{
		SHA: "abc123", Reviewer: "reviewer-1",
		Verdict: VerdictPASS, ReviewerFamily: "google",
		Branch: "main",
	}))
	mustErr(l.Record(RecordOpts{SHA: "fail123", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-2"}))
	must2(l.Verdict(VerdictOpts{SHA: "fail123", Reviewer: "reviewer-2", Verdict: VerdictFAIL}))

	queued, err := l.Queued()
	if err != nil {
		t.Fatalf("Queued: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("got %d queued, want 1", len(queued))
	}
	if queued[0].SHA != "abc123" {
		t.Errorf("queued SHA = %s, want abc123", queued[0].SHA)
	}
}

func TestConsumed(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Record(RecordOpts{SHA: "abc123", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
	must2(l.Verdict(VerdictOpts{
		SHA: "abc123", Reviewer: "reviewer-1",
		Verdict: VerdictPASS, ReviewerFamily: "google",
	}))
	mustErr(l.Consumed("abc123", "deadbeef"))

	eligible, err := l.Eligible("abc123", "")
	if err == nil || !strings.Contains(err.Error(), "consumed") {
		t.Fatalf("Eligible = %v, %v; want consumed refusal", eligible, err)
	}
	if eligible {
		t.Error("consumed sha is still eligible")
	}

	queued, _ := l.Queued()
	for _, q := range queued {
		if q.SHA == "abc123" {
			t.Error("consumed sha appears in Queued")
		}
	}
}

func TestRepair(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Repair(RepairOpts{
		SHA: "abc123", RepairAuthor: "fixer-1",
		Branch: "fix/thing", RepairFamily: "anthropic",
	}))
	rows, _ := l.AllRows()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Event != "repair" {
		t.Errorf("Event = %s", rows[0].Event)
	}
	if rows[0].RepairAuthor != "fixer-1" {
		t.Errorf("RepairAuthor = %s", rows[0].RepairAuthor)
	}
	if rows[0].RepairFamily != "anthropic" {
		t.Errorf("RepairFamily = %s", rows[0].RepairFamily)
	}
}

func TestEnqueue(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Enqueue(EnqueueOpts{SHA: "abc123", Reviewer: "manual", Branch: "main"}))
	qrows, _ := l.QueueRows()
	if len(qrows) != 1 {
		t.Fatalf("got %d queue rows, want 1", len(qrows))
	}
	if qrows[0].Event != "enqueue" || qrows[0].Status != "queued" {
		t.Errorf("queue row = %+v", qrows[0])
	}
}

func TestEnqueueDefaultReviewer(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Enqueue(EnqueueOpts{SHA: "abc123", Branch: "main"}))
	qrows, _ := l.QueueRows()
	if qrows[0].Reviewer != "manual" {
		t.Errorf("Reviewer = %s, want manual", qrows[0].Reviewer)
	}
}

func TestTier(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Record(RecordOpts{SHA: "abc123", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1", Tier: "R2"}))
	tier, err := l.Tier("abc123")
	if err != nil {
		t.Fatalf("Tier: %v", err)
	}
	if tier != "R2" {
		t.Errorf("Tier = %s, want R2", tier)
	}
}

func TestTierEmptyWhenNotSet(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Record(RecordOpts{SHA: "abc123", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
	tier, err := l.Tier("abc123")
	if err != nil {
		t.Fatalf("Tier: %v", err)
	}
	if tier != "" {
		t.Errorf("Tier = %s, want empty", tier)
	}
}

func TestNormalizeSHAPreservesFullSHA(t *testing.T) {
	l := newTestLedger(t)
	short := "deadbeef"
	got := l.NormalizeSHA(short)
	if got == "" {
		t.Error("got empty string from NormalizeSHA")
	}
}

func TestPassSHAs(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Record(RecordOpts{SHA: "aaaa", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
	must2(l.Verdict(VerdictOpts{SHA: "aaaa", Reviewer: "reviewer-1", Verdict: VerdictPASS, ReviewerFamily: "google"}))
	mustErr(l.Record(RecordOpts{SHA: "bbbb", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-2"}))
	must2(l.Verdict(VerdictOpts{SHA: "bbbb", Reviewer: "reviewer-2", Verdict: VerdictFAIL}))

	passSHAs, err := l.PassSHAs()
	if err != nil {
		t.Fatalf("PassSHAs: %v", err)
	}
	if len(passSHAs) != 1 {
		t.Fatalf("got %d pass SHAs, want 1", len(passSHAs))
	}
	if passSHAs[0] != "aaaa" {
		t.Errorf("pass SHA = %s, want aaaa", passSHAs[0])
	}
}

func TestVetoSHAs(t *testing.T) {
	l := newTestLedger(t)
	mustErr(l.Record(RecordOpts{SHA: "aaaa", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
	must2(l.Verdict(VerdictOpts{SHA: "aaaa", Reviewer: "reviewer-1", Verdict: VerdictFAIL}))
	mustErr(l.Record(RecordOpts{SHA: "bbbb", Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-2"}))
	must2(l.Verdict(VerdictOpts{SHA: "bbbb", Reviewer: "reviewer-2", Verdict: VerdictPASS, ReviewerFamily: "google"}))

	vtoSHAs, err := l.VetoSHAs()
	if err != nil {
		t.Fatalf("VetoSHAs: %v", err)
	}
	if len(vtoSHAs) != 1 {
		t.Fatalf("got %d veto SHAs, want 1", len(vtoSHAs))
	}
	if vtoSHAs[0] != "aaaa" {
		t.Errorf("veto SHA = %s, want aaaa", vtoSHAs[0])
	}
}

func TestFamilyAllowlist(t *testing.T) {
	expected := []string{
		"anthropic", "openai", "google", "xai", "zhipu",
		"moonshot", "alibaba", "deepseek", "open-weight",
		"antigravity", "proxy",
	}
	for _, f := range expected {
		if !FamilyAllowlist[f] {
			t.Errorf("FamilyAllowlist missing %s", f)
		}
	}
	if FamilyAllowlist["unicorn"] {
		t.Error("FamilyAllowlist contains unicorn")
	}
}

func TestRejectUnknownReviewerFamily(t *testing.T) {
	l := newTestLedger(t)
	_, err := l.Verdict(VerdictOpts{
		SHA: "abc123", Reviewer: "reviewer-1",
		Verdict:        VerdictPASS,
		ReviewerFamily: "unicorn",
	})
	if err == nil {
		t.Fatal("expected error for unknown reviewer family")
	}
}

func TestRejectInvalidVerdict(t *testing.T) {
	l := newTestLedger(t)
	_, err := l.Verdict(VerdictOpts{
		SHA: "abc123", Reviewer: "reviewer-1",
		Verdict: "LGTM",
	})
	if err == nil {
		t.Fatal("expected error for invalid verdict")
	}
}

func TestVerdictIdempotent(t *testing.T) {
	l := newTestLedger(t)
	enq1, err := l.Verdict(VerdictOpts{
		SHA: "abc123", Reviewer: "reviewer-1",
		Verdict: VerdictPASS, ReviewerFamily: "google",
	})
	if err != nil {
		t.Fatalf("first Verdict: %v", err)
	}
	if !enq1 {
		t.Error("expected enqueued=true")
	}
	rows, _ := l.AllRows()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after first verdict, got %d", len(rows))
	}

	enq2, err := l.Verdict(VerdictOpts{
		SHA: "abc123", Reviewer: "reviewer-1",
		Verdict: VerdictPASS, ReviewerFamily: "google",
	})
	if err != nil {
		t.Fatalf("second Verdict: %v", err)
	}
	if enq2 {
		t.Error("duplicate verdict must report enqueued=false")
	}
	rows, _ = l.AllRows()
	if len(rows) != 1 {
		t.Fatalf("expected still 1 row after duplicate verdict, got %d", len(rows))
	}
}

func TestRetryPASSExplicitlySupersedesOnlyNamedReviewer(t *testing.T) {
	l := newTestLedger(t)
	const sha = "retry-candidate"
	for _, reviewer := range []string{"reviewer-a", "reviewer-b", "reviewer-c", "reviewer-d"} {
		if err := l.Record(RecordOpts{SHA: sha, Branch: "herd/fac-493", Reviewer: reviewer, BuilderFamily: "anthropic", ReviewerFamily: "openai"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-a", Verdict: VerdictFAIL, ReviewerFamily: "openai"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-b", Verdict: VerdictPASS, ReviewerFamily: "openai", RetryOf: "reviewer-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-c", Verdict: VerdictFAIL, ReviewerFamily: "openai"}); err != nil {
		t.Fatal(err)
	}
	eligible, err := l.Eligible(sha, "")
	if err == nil || eligible || !strings.Contains(err.Error(), "review veto") {
		t.Fatalf("unrelated conflicting FAIL was not preserved: eligible=%v err=%v", eligible, err)
	}
	if _, err := l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer-d", Verdict: VerdictPASS, ReviewerFamily: "openai", RetryOf: "reviewer-c"}); err != nil {
		t.Fatal(err)
	}
	eligible, err = l.Eligible(sha, "")
	if err != nil || !eligible {
		t.Fatalf("explicit retries should clear named vetoes: eligible=%v err=%v", eligible, err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	seenAudit := false
	for _, row := range rows {
		if row.Event == string(EventSupersession) && row.SHA == sha && row.RetryOf == "reviewer-a" {
			seenAudit = true
		}
	}
	if !seenAudit {
		t.Fatal("retry PASS did not append an explicit supersession audit row")
	}
}

func TestQuarantineMalformed(t *testing.T) {
	l := newTestLedger(t)
	qDir := t.TempDir()
	qPath := filepath.Join(qDir, "malformed.jsonl")
	SetQuarantinePath(qPath)

	// Write a malformed line directly.
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("{borken}\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}

	rows, err := l.AllRows()
	if err != nil {
		t.Fatalf("AllRows: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows (malformed skipped), got %d", len(rows))
	}

	qData, err := os.ReadFile(qPath)
	if err != nil {
		t.Fatalf("quarantine file not created: %v", err)
	}
	if !strings.Contains(string(qData), "borken") {
		t.Errorf("quarantine file = %q, want contains borken", string(qData))
	}
}

func TestReadRowsFailClosed(t *testing.T) {
	_, err := readRows("/nonexistent/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("expected nil for non-existent path, got %v", err)
	}
	_, err = readRows("/tmp")
	if err == nil {
		t.Fatal("expected error for directory path, got nil")
	}
}

func TestAllRowsEmptyLedger(t *testing.T) {
	l := newTestLedger(t)
	rows, err := l.AllRows()
	if err != nil {
		t.Fatalf("AllRows: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0", len(rows))
	}
}

func TestAppendStability(t *testing.T) {
	l := newTestLedger(t)
	for i := 0; i < 5; i++ {
		sha := strings.Repeat(string(rune('a'+i)), 12)
		mustErr(l.Record(RecordOpts{SHA: sha, Branch: "main", BuilderFamily: "anthropic", Reviewer: "r" + string(rune('0'+i))}))
	}
	rows, _ := l.AllRows()
	if len(rows) != 5 {
		t.Fatalf("got %d rows, want 5", len(rows))
	}
}

func TestEligibleNoRecordAtAll(t *testing.T) {
	l := newTestLedger(t)
	eligible, err := l.Eligible("deadbeef", "")
	if err == nil {
		t.Fatal("expected error for sha with no records")
	}
	if eligible {
		t.Error("sha with no records should not be eligible")
	}
}

func TestEligibleRefusalReasons(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *Ledger, string)
		want  string
	}{
		{
			name: "superseded",
			setup: func(t *testing.T, l *Ledger, sha string) {
				mustErr(l.Record(RecordOpts{SHA: sha, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer"}))
				mustErr(l.Supersession(DecisionOpts{SHA: "replacement", PreviousSHA: sha, Reason: "fixed"}))
			},
			want: "superseded",
		},
		{
			name: "family mismatch",
			setup: func(t *testing.T, l *Ledger, sha string) {
				mustErr(l.Record(RecordOpts{SHA: sha, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer"}))
				must2(l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer", Verdict: VerdictPASS, ReviewerFamily: "anthropic"}))
			},
			want: "family mismatch",
		},
		{
			name: "consumed",
			setup: func(t *testing.T, l *Ledger, sha string) {
				mustErr(l.Record(RecordOpts{SHA: sha, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer"}))
				must2(l.Verdict(VerdictOpts{SHA: sha, Reviewer: "reviewer", Verdict: VerdictPASS, ReviewerFamily: "google"}))
				mustErr(l.Consumed(sha, "merge-sha"))
			},
			want: "consumed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLedger(t)
			sha := "reason-" + strings.ReplaceAll(tc.name, " ", "-")
			tc.setup(t, l, sha)
			eligible, err := l.Eligible(sha, "anthropic")
			if eligible || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Eligible = %v, %v; want refusal containing %q", eligible, err, tc.want)
			}
		})
	}
}

func TestEligibleReportsQueueReadError(t *testing.T) {
	l := newTestLedger(t)
	if err := os.Remove(l.QueuePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(l.QueuePath, 0o755); err != nil {
		t.Fatal(err)
	}
	eligible, err := l.Eligible("queue-error", "")
	if eligible || err == nil || !strings.Contains(err.Error(), "queue read error") {
		t.Fatalf("Eligible = %v, %v; want queue read error", eligible, err)
	}
}

func TestDecisionEventsAreExplicitAndPreserveReason(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Refutation(DecisionOpts{SHA: "newsha", PreviousSHA: "oldsha", Reviewer: "reviewer", Reason: "finding disproven", FindingsRef: "findings.md"}); err != nil {
		t.Fatalf("Refutation: %v", err)
	}
	if err := l.Supersession(DecisionOpts{SHA: "replacement", PreviousSHA: "oldsha", Reviewer: "reviewer", Reason: "replacement repairs the finding", CandidateSHA: "replacement"}); err != nil {
		t.Fatalf("Supersession: %v", err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatalf("AllRows: %v", err)
	}
	if len(rows) != 2 || rows[0].Event != string(EventRefutation) || rows[1].Event != string(EventSupersession) {
		t.Fatalf("unexpected decision rows: %+v", rows)
	}
	if rows[0].Reason == "" || rows[0].Task != "oldsha" || rows[1].Reason == "" || rows[1].Task != "oldsha" {
		t.Fatalf("decision relationship not retained: %+v", rows)
	}
}
