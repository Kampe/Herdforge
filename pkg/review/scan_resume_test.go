package review

import (
	"context"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// FAC-605: ResumeAfter must skip tips at or before the cursor so a subsequent
// drain continues rather than restarting the tip set.
func TestScanResumesAfterCursor(t *testing.T) {
	tips := []harvest.UnmergedWork{
		{Branch: "a", Unmerged: []string{"sha-a"}},
		{Branch: "b", Unmerged: []string{"sha-b"}},
		{Branch: "c", Unmerged: []string{"sha-c"}},
		{Branch: "d", Unmerged: []string{"sha-d"}},
	}
	seen := []string{}
	d := &Drain{
		RepoRoot:    t.TempDir(),
		LedgerPath:  t.TempDir() + "/ledger.jsonl",
		ResumeAfter: "sha-b",
		Progress: func(done, total int, branch, sha string) {
			seen = append(seen, branch)
		},
	}
	// Expired context: progress fires only for the first tip we would probe.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	report, err := d.Scan(ctx, tips)
	if err == nil {
		t.Fatal("expired context must fail closed")
	}
	if report == nil {
		t.Fatal("partial report required")
	}
	// Start index after sha-b is 2 (sha-c). Deadline hits before probing, so
	// ScannedTips is the resume start index and Progress may not fire.
	if report.ScannedTips != 2 {
		t.Fatalf("ScannedTips=%d want 2 (resume start)", report.ScannedTips)
	}
	if report.TotalTips != 4 {
		t.Fatalf("TotalTips=%d", report.TotalTips)
	}
}
