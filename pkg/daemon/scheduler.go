package daemon

import (
	"context"
	"fmt"
	"time"
)

// PulseSchedulerOptions controls the durable coordinator cadence. A zero
// interval uses the production default; MaxTicks is intended for bounded
// verification and zero means continue until the context is cancelled.
type PulseSchedulerOptions struct {
	Interval time.Duration
	MaxTicks int
}

const defaultPulseSchedulerInterval = time.Minute

// RunPulseScheduler runs one pulse sweep immediately and repeats it at a
// bounded cadence. Sweep failures are returned after the scheduler has made
// its configured attempts, while later ticks still run; a transient provider
// or quota refusal must not silently terminate the self-driving cadence.
func RunPulseScheduler(ctx context.Context, opts PulseSchedulerOptions, sweep func(context.Context) error) error {
	if ctx == nil {
		return fmt.Errorf("pulse scheduler: context is required")
	}
	if sweep == nil {
		return fmt.Errorf("pulse scheduler: sweep is required")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultPulseSchedulerInterval
	}

	var firstErr error
	for tick := 0; opts.MaxTicks == 0 || tick < opts.MaxTicks; tick++ {
		select {
		case <-ctx.Done():
			if firstErr != nil {
				return firstErr
			}
			return ctx.Err()
		default:
		}
		if err := sweep(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("pulse scheduler: tick %d: %w", tick+1, err)
		}
		if opts.MaxTicks > 0 && tick+1 >= opts.MaxTicks {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if firstErr != nil {
				return firstErr
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return firstErr
}
