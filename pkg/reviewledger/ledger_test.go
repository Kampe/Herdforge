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
			sha: failSHA, wantEligible: false, wantErr: true,
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
			name: "consumed sha is not eligible",
			setup: func(t *testing.T, l *Ledger) {
				mustErr(l.Record(RecordOpts{SHA: passSHA, Branch: "main", BuilderFamily: "anthropic", Reviewer: "reviewer-1"}))
				must2(l.Verdict(VerdictOpts{SHA: passSHA, Reviewer: "reviewer-1", Verdict: VerdictPASS, ReviewerFamily: "google"}))
				mustErr(l.Consumed(passSHA, "deadbeef"))
			},
			sha: passSHA, wantEligible: false,
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
	if err != nil {
		t.Fatalf("Eligible: %v", err)
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
