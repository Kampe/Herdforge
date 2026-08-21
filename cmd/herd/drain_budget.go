package main

import (
	"os"
	"strings"
	"time"
)

// Review-scan budget defaults. The scan visits every unmerged worktree, so a
// fixed ceiling is wrong by construction on a large board: measured live,
// harvest-scan took 41s for 64 worktrees and review-scan then had 79s of a
// shared 2m for those same 64, timing out on every pass.
const (
	defaultDrainReviewPerItem = 3 * time.Second
	minDrainReviewTimeout     = 30 * time.Second
	maxDrainReviewTimeout     = 15 * time.Minute
)

// drainReviewPerItem is the per-worktree allowance.
func drainReviewPerItem() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("HERD_DRAIN_REVIEW_PER_ITEM")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultDrainReviewPerItem
}

// drainReviewTimeout derives review-scan's budget from the exact number of
// worktrees harvest-scan just measured.
//
// An explicit HERD_DRAIN_REVIEW_TIMEOUT always wins, so an operator can pin it.
// Otherwise the budget scales with the item count between a floor (a tiny board
// still gets room for slow I/O) and a ceiling (the scan must stay bounded --
// removing the ceiling would let a stalled provider hang the command, which is
// strictly worse than a timeout with a partial).
func drainReviewTimeout(items int) time.Duration {
	if raw := strings.TrimSpace(os.Getenv("HERD_DRAIN_REVIEW_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	if items < 0 {
		items = 0
	}
	budget := time.Duration(items) * drainReviewPerItem()
	if budget < minDrainReviewTimeout {
		return minDrainReviewTimeout
	}
	if budget > maxDrainReviewTimeout {
		return maxDrainReviewTimeout
	}
	return budget
}

// perTipSeconds is the observed per-tip cost, used to report why a budget was
// insufficient instead of leaving an operator to guess.
func perTipSeconds(elapsed time.Duration, scanned int) float64 {
	if scanned <= 0 {
		return 0
	}
	return elapsed.Seconds() / float64(scanned)
}

// suggestPerItem turns an observed per-tip cost into a concrete override, with
// headroom. Reporting a measured number beats asking an operator to guess, and
// beats me guessing a default from the outside.
func suggestPerItem(elapsed time.Duration, scanned int) time.Duration {
	per := perTipSeconds(elapsed, scanned)
	if per <= 0 {
		return defaultDrainReviewPerItem
	}
	// 50% headroom, rounded up to the next second.
	suggested := time.Duration((per*1.5)+0.999) * time.Second
	if suggested < defaultDrainReviewPerItem {
		return defaultDrainReviewPerItem
	}
	return suggested
}
