package main

import (
	"fmt"
	"io"
	"time"

	"github.com/Kampe/Herdforge/pkg/drainreceipt"
	"github.com/Kampe/Herdforge/pkg/review"
)

// attachDrainReceiptProgress wires d.Progress so the FAC-605 receipt advances
// from inside Drain.Scan's per-tip loop, at a time-bounded cadence.
//
// every <= 0 selects drainreceipt.DefaultProgressInterval (2s). Tests pass a
// tiny interval so multi-tip progress is observable without sleeping.
func attachDrainReceiptProgress(d *review.Drain, root string, quiet bool, errOut io.Writer, reviewStart time.Time, every time.Duration) {
	lastTick := reviewStart
	receiptCadence := drainreceipt.NewProgressCadence(every)
	d.Progress = func(done, total int, branch, sha string) {
		now := time.Now()
		if receiptCadence.ShouldWrite(now, done) {
			_ = drainreceipt.Progress(root, "review-scan", sha, done, total, 0)
			receiptCadence.MarkWritten(now)
		}
		if quiet {
			return
		}
		if done < 3 || done%5 == 0 {
			fmt.Fprintf(errOut, "herd-drain: review-scan %d/%d elapsed=%s last_item=%s branch=%s\n",
				done, total, now.Sub(reviewStart).Round(time.Second),
				now.Sub(lastTick).Round(time.Millisecond), branch)
			lastTick = now
		}
	}
}
