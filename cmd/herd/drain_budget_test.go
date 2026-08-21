package main

import (
	"testing"
	"time"
)

// TestReviewBudgetScalesWithWorktreeCount is the FAC-555 follow-up regression.
//
// Measured live: harvest-scan took 41s for 64 worktrees, leaving review-scan
// 79s of a shared fixed 2m for those same 64. It timed out every pass no matter
// how good the partial was, because the work is O(items) and the bound was not.
func TestReviewBudgetScalesWithWorktreeCount(t *testing.T) {
	t.Setenv("HERD_DRAIN_REVIEW_TIMEOUT", "")
	t.Setenv("HERD_DRAIN_REVIEW_PER_ITEM", "")

	// The reported board shape must get materially more than the 79s it had.
	got := drainReviewTimeout(64)
	if got <= 79*time.Second {
		t.Fatalf("64 worktrees must get more than the 79s that failed live, got %s", got)
	}
	// Monotonic in item count.
	if drainReviewTimeout(128) <= got {
		t.Fatalf("budget must grow with item count: 128 gave %s vs 64 %s", drainReviewTimeout(128), got)
	}
}

// A tiny or empty board still gets room for slow I/O rather than a near-zero
// budget that fails instantly.
func TestReviewBudgetHasAFloor(t *testing.T) {
	t.Setenv("HERD_DRAIN_REVIEW_TIMEOUT", "")
	t.Setenv("HERD_DRAIN_REVIEW_PER_ITEM", "")
	for _, n := range []int{0, 1, -5} {
		if got := drainReviewTimeout(n); got < minDrainReviewTimeout {
			t.Fatalf("items=%d must get at least the floor %s, got %s", n, minDrainReviewTimeout, got)
		}
	}
}

// The scan must stay bounded. Removing the ceiling would let a stalled provider
// hang the command, which is strictly worse than a timeout with a partial.
func TestReviewBudgetHasACeiling(t *testing.T) {
	t.Setenv("HERD_DRAIN_REVIEW_TIMEOUT", "")
	t.Setenv("HERD_DRAIN_REVIEW_PER_ITEM", "")
	if got := drainReviewTimeout(100000); got > maxDrainReviewTimeout {
		t.Fatalf("budget must stay bounded, got %s", got)
	}
}

// An explicit operator pin always wins over the derived value.
func TestReviewBudgetHonorsExplicitOverrides(t *testing.T) {
	t.Setenv("HERD_DRAIN_REVIEW_TIMEOUT", "7m")
	if got := drainReviewTimeout(1); got != 7*time.Minute {
		t.Fatalf("explicit timeout must win, got %s", got)
	}
	t.Setenv("HERD_DRAIN_REVIEW_TIMEOUT", "")
	t.Setenv("HERD_DRAIN_REVIEW_PER_ITEM", "10s")
	if got := drainReviewTimeout(10); got != 100*time.Second {
		t.Fatalf("per-item override must apply, got %s", got)
	}
	// A malformed value must fall back to the default, not to zero.
	t.Setenv("HERD_DRAIN_REVIEW_PER_ITEM", "not-a-duration")
	if got := drainReviewPerItem(); got != defaultDrainReviewPerItem {
		t.Fatalf("malformed override must fall back to the default, got %s", got)
	}
}
