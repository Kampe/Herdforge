package drainreceipt

import "time"

// DefaultProgressInterval bounds how often an in-flight drain may fsync the
// receipt during the hot tip loop (FAC-605 residual).
//
// Always-writing once per tip turns an O(N) merge-tree scan into O(N) durable
// writes — a new performance defect on the path that already vanished mid-board
// (1,742 tips). Never-writing loses the cursor on SIGKILL. Time-based cadence
// is preferred over tip-count cadence because tip costs are not uniform: cheap
// tips would under-write progress and expensive tips would over-write disk.
//
// 2s: at most ~30 fsyncs/minute; a kill loses at most ~2s of tip progress,
// which is one tip on a slow probe and a few tips on fast ones — still usable.
const DefaultProgressInterval = 2 * time.Second

// ProgressCadence decides when a hot-loop Progress write should hit disk.
type ProgressCadence struct {
	Every time.Duration
	last  time.Time
}

// NewProgressCadence returns a cadence with the given interval (or the default
// when every <= 0).
func NewProgressCadence(every time.Duration) *ProgressCadence {
	if every <= 0 {
		every = DefaultProgressInterval
	}
	return &ProgressCadence{Every: every}
}

// ShouldWrite reports whether this tip should persist the receipt. The first
// tip always writes so a kill before the interval still leaves a cursor.
// Subsequent writes require Every to have elapsed since the last write.
func (c *ProgressCadence) ShouldWrite(now time.Time, done int) bool {
	if c == nil {
		return true
	}
	if done == 0 || c.last.IsZero() {
		return true
	}
	return now.Sub(c.last) >= c.Every
}

// MarkWritten records that a Progress write landed at now.
func (c *ProgressCadence) MarkWritten(now time.Time) {
	if c == nil {
		return
	}
	c.last = now
}
