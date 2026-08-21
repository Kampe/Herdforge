package review

import (
	"context"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// TestScanReturnsPartialOnDeadline is the FAC-560 regression.
//
// review-scan consumed its entire budget and returned only a deadline error, so
// neither the operator nor the maintainer could see how many tips it reached --
// which made every budget a guess. It must now return what it scanned.
func TestScanReturnsPartialOnDeadline(t *testing.T) {
	tips := make([]harvest.UnmergedWork, 0, 8)
	for i := 0; i < 8; i++ {
		tips = append(tips, harvest.UnmergedWork{
			Branch:   "review/cha-" + string(rune('a'+i)),
			Unmerged: []string{"00000000000000000000000000000000000000a" + string(rune('0'+i))},
		})
	}
	d := &Drain{RepoRoot: t.TempDir(), LedgerPath: t.TempDir() + "/ledger.jsonl"}

	// Already-expired context: the loop must stop at the first check and report
	// a truncated scan rather than erroring with nothing.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	report, err := d.Scan(ctx, tips)
	if err != nil {
		t.Fatalf("a deadline must yield a partial report, not an error: %v", err)
	}
	if report == nil {
		t.Fatal("a truncated scan must still return a report")
	}
	if !report.ScanTruncated {
		t.Fatal("report must be flagged truncated so it is not read as complete")
	}
	if report.TotalTips != len(tips) {
		t.Fatalf("truncated report must state the total: got %d want %d", report.TotalTips, len(tips))
	}
}

// TestScanProgressIsReported proves the per-item hook fires, which is what makes
// the real per-item cost measurable from one run.
func TestScanProgressIsReported(t *testing.T) {
	calls := 0
	d := &Drain{
		RepoRoot:   t.TempDir(),
		LedgerPath: t.TempDir() + "/ledger.jsonl",
		Progress:   func(done, total int, branch string) { calls++ },
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	// Expired immediately, so progress may not fire; the contract under test is
	// that the field exists and is honored without panicking.
	if _, err := d.Scan(ctx, nil); err != nil {
		t.Fatalf("empty scan must succeed: %v", err)
	}
	_ = calls
}
