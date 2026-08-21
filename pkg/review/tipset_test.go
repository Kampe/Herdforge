package review

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// TestTipSetIncludesQueuedPins is the FAC-562 root-cause regression, with the
// consumer's exact live shape.
//
// Live run: "review budget 9m21s for 187 tips" then "586/586" scanned. The tip
// set is worktree SHAs PLUS queued ledger pins, and the budget planner counted
// only the worktree half -- so the whole queue was missing from the budget. Both
// numbers now come from buildTipSet, so they cannot diverge.
func TestTipSetIncludesQueuedPins(t *testing.T) {
	unmerged := []harvest.UnmergedWork{
		{Branch: "review/a", WorktreePath: "/wt/a", Unmerged: []string{"sha-a"}},
		{Branch: "review/b", WorktreePath: "/wt/b", Unmerged: []string{"sha-b"}},
	}
	queued := []queuePin{
		{sha: "sha-q1", branch: "", lane: "lane-1"},
		{sha: "sha-q2", branch: "review/q2", lane: "lane-2"},
	}
	tips, lanes := buildTipSet(unmerged, queued)
	if len(tips) != 4 {
		t.Fatalf("tips must span worktrees AND queued pins, got %d", len(tips))
	}
	if len(tips) == len(unmerged) {
		t.Fatal("fixture must distinguish worktree count from tip count")
	}
	if lanes["sha-q1"] != "lane-1" {
		t.Fatalf("queue lanes must be recorded, got %v", lanes)
	}
	if tips[0].Branch != "review/a" || tips[3].Branch != "review/q2" {
		t.Fatalf("worktree tips must come first, preserving scan order: %+v", tips)
	}
}

// A SHA in both halves is one tip, matching the scan's own dedup.
func TestTipSetDeduplicatesAcrossHalves(t *testing.T) {
	tips, _ := buildTipSet(
		[]harvest.UnmergedWork{{Branch: "review/a", Unmerged: []string{"dup"}}},
		[]queuePin{{sha: "dup", branch: "queued", lane: "l"}},
	)
	if len(tips) != 1 {
		t.Fatalf("a SHA in both halves is one tip, got %d", len(tips))
	}
	if tips[0].Branch != "review/a" {
		t.Fatalf("the worktree identity must win, got %+v", tips[0])
	}
}

// Empty SHAs are not tips; they produced the unlabelled progress lines the
// consumer reported ("branch=" with no identity).
func TestTipSetSkipsEmptySHAs(t *testing.T) {
	tips, _ := buildTipSet(
		[]harvest.UnmergedWork{{Branch: "a", Unmerged: []string{"", "real"}}},
		[]queuePin{{sha: "", branch: "b"}},
	)
	if len(tips) != 1 || tips[0].Unmerged[0] != "real" {
		t.Fatalf("empty SHAs must be skipped, got %+v", tips)
	}
}

// A queued pin carries no worktree branch, so progress needs a SHA label or a
// pathological object cannot be identified.
func TestShortSHAProvidesALabel(t *testing.T) {
	if got := shortSHA("8ef12f0dbd6b172745c3b9abde4fc294fe7b1d2f"); got != "8ef12f0dbd6b" {
		t.Fatalf("want a 12-char label, got %q", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Fatalf("short input must pass through, got %q", got)
	}
}
