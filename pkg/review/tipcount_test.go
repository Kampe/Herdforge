package review

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// TestCountTipsCountsSHAsNotWorktrees is the FAC-561 root-cause regression.
//
// review-scan iterates one tip per unmerged SHA. The budget was computed from
// the WORKTREE count, so a board whose 54 in-scope worktrees expanded to 400
// tips got 2m42s for work needing roughly eight times that. The scan could
// never finish regardless of per-item tuning.
func TestCountTipsCountsSHAsNotWorktrees(t *testing.T) {
	work := []harvest.UnmergedWork{
		{Branch: "a", Unmerged: []string{"s1", "s2", "s3"}},
		{Branch: "b", Unmerged: []string{"s4"}},
	}
	if got := CountTips(work); got != 4 {
		t.Fatalf("want 4 tips across 2 worktrees, got %d", got)
	}
	if got := len(work); got == CountTips(work) {
		t.Fatal("fixture must distinguish worktree count from tip count")
	}
}

// Tips are deduplicated by SHA, matching the scan loop.
func TestCountTipsDeduplicatesSHAs(t *testing.T) {
	work := []harvest.UnmergedWork{
		{Branch: "a", Unmerged: []string{"same", "same"}},
		{Branch: "b", Unmerged: []string{"same", ""}},
	}
	if got := CountTips(work); got != 1 {
		t.Fatalf("want 1 unique tip, got %d", got)
	}
}

// TestAgentScratchExcludedByDefault pins the declared polarity. The generic
// fail-open kept an agent scratch branch in scope on a board with no readable
// receipts, which the consumer reported: worktree-agent-* must be excluded BY
// DEFAULT unless a receipt names it.
func TestAgentScratchExcludedByDefault(t *testing.T) {
	work := []harvest.UnmergedWork{{Branch: "worktree-agent-a220b4ebf0cd79450"}}

	kept, skipped := ScopeDrainCandidates(work, nil)
	if len(kept) != 0 || len(skipped) != 1 || skipped[0].Reason != SkipAgentScratch {
		t.Fatalf("agent scratch must be excluded with no oracle: kept=%d skipped=%v", len(kept), skipped)
	}

	// A receipt naming it brings it back in scope.
	kept, _ = ScopeDrainCandidates(work, func(harvest.UnmergedWork) bool { return true })
	if len(kept) != 1 {
		t.Fatal("a receipted agent branch must be kept")
	}
}

// Control lanes keep the fail-open behavior: without an oracle they cannot be
// proven non-candidates, and hiding a real candidate is worse than a slow scan.
func TestControlLanesStillFailOpenWithoutOracle(t *testing.T) {
	kept, _ := ScopeDrainCandidates([]harvest.UnmergedWork{
		{Branch: "standing/nft-data-engineer"},
		{Branch: "herd/cha-1804"},
	}, nil)
	if len(kept) != 2 {
		t.Fatalf("control lanes must stay in scope without a receipt oracle, kept=%d", len(kept))
	}
}
