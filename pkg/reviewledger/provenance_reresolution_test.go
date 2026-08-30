package reviewledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
)

func historicalUnrecordedLedger(t *testing.T) (*Ledger, string, LedgerRow) {
	t.Helper()
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	if err := l.Record(RecordOpts{
		SHA: sha, Reviewer: "reviewer-a", ReviewerFamily: "xai", Tier: "R3",
		BuilderFamily: FamilyUnrecorded, Gate: GateProvenanceUnrecorded,
	}); err != nil {
		t.Fatal(err)
	}
	unrelated := LedgerRow{Event: string(EventRecord), SHA: strings.Repeat("b", 40), Reviewer: "unrelated", BuilderFamily: "anthropic", Tier: "R1"}
	if err := l.appendRow(l.Path, &unrelated); err != nil {
		t.Fatal(err)
	}
	return l, sha, unrelated
}

func writeLaunchReceipts(t *testing.T, path string, receipts ...launch.Receipt) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for _, receipt := range receipts {
		line, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		body.Write(line)
		body.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resolutionOpts(sha, receiptPath string, commitTime time.Time) LaunchProvenanceResolutionOpts {
	return LaunchProvenanceResolutionOpts{
		SHA: sha, Reviewer: "reviewer-a", ReceiptPath: receiptPath,
		CommitTime: commitTime, Reaches: func(string) bool { return true }, Apply: true,
	}
}

func TestReResolveLaunchProvenanceAppendsPreservesAndRetriesIdempotently(t *testing.T) {
	l, sha, unrelated := historicalUnrecordedLedger(t)
	commitTime := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	receipts := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	writeLaunchReceipts(t, receipts, launch.Receipt{
		CreatedAt: commitTime.Add(-time.Minute), Accepted: true,
		Branch: "herd/fac-630", BuilderFamily: "openai",
	})

	first, err := l.ReResolveLaunchProvenance(resolutionOpts(sha, receipts, commitTime))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Appended || !first.ReadbackVerified || first.Idempotent || first.PreviousFamily != FamilyUnrecorded || first.ResolvedFamily != "openai" {
		t.Fatalf("first resolution = %+v", first)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	var candidateRows []LedgerRow
	var gotUnrelated *LedgerRow
	for i := range rows {
		if rows[i].SHA == sha && rows[i].Event == string(EventRecord) {
			candidateRows = append(candidateRows, rows[i])
		}
		if rows[i].SHA == unrelated.SHA && rows[i].Reviewer == unrelated.Reviewer {
			row := rows[i]
			gotUnrelated = &row
		}
	}
	if len(candidateRows) != 2 {
		t.Fatalf("candidate record rows=%d, want original plus one resolution", len(candidateRows))
	}
	if candidateRows[0].BuilderFamily != FamilyUnrecorded || candidateRows[0].Gate != GateProvenanceUnrecorded {
		t.Fatalf("historical row was rewritten: %+v", candidateRows[0])
	}
	if candidateRows[1].BuilderFamily != "openai" || candidateRows[1].Gate != GateReceiptResolvedProvenance {
		t.Fatalf("resolved row = %+v", candidateRows[1])
	}
	if gotUnrelated == nil || *gotUnrelated != unrelated {
		t.Fatalf("unrelated row changed: got=%+v want=%+v", gotUnrelated, unrelated)
	}

	second, err := l.ReResolveLaunchProvenance(resolutionOpts(sha, receipts, commitTime))
	if err != nil {
		t.Fatal(err)
	}
	if second.Appended || !second.Idempotent || !second.ReadbackVerified {
		t.Fatalf("idempotent retry = %+v", second)
	}
	after, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(rows) {
		t.Fatalf("idempotent retry grew ledger from %d to %d rows", len(rows), len(after))
	}
}

func TestReResolveLaunchProvenanceRefusesReceiptFailuresWithoutMutation(t *testing.T) {
	commitTime := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		receipts []launch.Receipt
		want     string
	}{
		{name: "no receipt", want: "receipt_resolution=none"},
		{
			name: "ambiguous receipt",
			receipts: []launch.Receipt{
				{CreatedAt: commitTime.Add(-time.Minute), Accepted: true, Branch: "branch-a", BuilderFamily: "anthropic"},
				{CreatedAt: commitTime.Add(-time.Minute), Accepted: true, Branch: "branch-b", BuilderFamily: "openai"},
			},
			want: "receipt_resolution=ambiguous",
		},
		{
			name: "unallowlisted receipt family",
			receipts: []launch.Receipt{
				{CreatedAt: commitTime.Add(-time.Minute), Accepted: true, Branch: "branch-a", BuilderFamily: "unknown"},
			},
			want: `builder_family="unknown" allowlisted=false`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, sha, _ := historicalUnrecordedLedger(t)
			path := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
			writeLaunchReceipts(t, path, tc.receipts...)
			before, err := l.AllRows()
			if err != nil {
				t.Fatal(err)
			}
			result, err := l.ReResolveLaunchProvenance(resolutionOpts(sha, path, commitTime))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("result=%+v err=%v, want refusal containing %q", result, err, tc.want)
			}
			after, readErr := l.AllRows()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(after) != len(before) {
				t.Fatalf("refusal changed ledger rows %d -> %d", len(before), len(after))
			}
		})
	}
}

func TestReResolveLaunchProvenanceRefusesBusyMutationLock(t *testing.T) {
	l, sha, _ := historicalUnrecordedLedger(t)
	commitTime := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	receipts := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	writeLaunchReceipts(t, receipts, launch.Receipt{
		CreatedAt: commitTime.Add(-time.Minute), Accepted: true,
		Branch: "herd/fac-630", BuilderFamily: "openai",
	})

	holder, err := os.OpenFile(l.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN) //nolint:errcheck

	peer, err := NewReviewLedger(l.RepoRoot, l.Path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	result, err := peer.ReResolveLaunchProvenance(resolutionOpts(sha, receipts, commitTime))
	if err == nil || !strings.Contains(err.Error(), "admission_condition=concurrency") || !strings.Contains(err.Error(), "mutation_lock_acquired=false") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	after, readErr := l.AllRows()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != len(before) {
		t.Fatalf("busy-lock refusal changed ledger rows %d -> %d", len(before), len(after))
	}
}

func TestReResolveLaunchProvenanceConcurrentAttemptsAppendAtMostOnce(t *testing.T) {
	l, sha, _ := historicalUnrecordedLedger(t)
	commitTime := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	receipts := filepath.Join(t.TempDir(), "launch-receipts.jsonl")
	writeLaunchReceipts(t, receipts, launch.Receipt{
		CreatedAt: commitTime.Add(-time.Minute), Accepted: true,
		Branch: "herd/fac-630", BuilderFamily: "openai",
	})
	peer, err := NewReviewLedger(l.RepoRoot, l.Path)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan LaunchProvenanceResolution, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, ledger := range []*Ledger{l, peer} {
		wg.Add(1)
		go func(ledger *Ledger) {
			defer wg.Done()
			<-start
			result, resolveErr := ledger.ReResolveLaunchProvenance(resolutionOpts(sha, receipts, commitTime))
			results <- result
			errs <- resolveErr
		}(ledger)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for resolveErr := range errs {
		if resolveErr != nil && !strings.Contains(resolveErr.Error(), "admission_condition=concurrency") {
			t.Fatalf("unexpected concurrent refusal: %v", resolveErr)
		}
	}
	appended := 0
	for result := range results {
		if result.Appended {
			appended++
			if !result.ReadbackVerified {
				t.Fatalf("append winner lacked exact readback: %+v", result)
			}
		}
	}
	if appended != 1 {
		t.Fatalf("concurrent appended winners=%d, want exactly one", appended)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	records := 0
	for _, row := range rows {
		if row.Event == string(EventRecord) && row.SHA == sha && row.Reviewer == "reviewer-a" {
			records++
		}
	}
	if records != 2 {
		t.Fatalf("candidate record rows=%d, want original plus exactly one resolution", records)
	}
}
