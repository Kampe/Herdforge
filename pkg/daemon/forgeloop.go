package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-128: the RUNNING forge loop. ForgeStep decides the next action; this
// driver executes it and repeats, turning the built primitives into an
// operating forge. The ForgeDriver interface is injected so the loop logic is
// testable without live herdr/git — cmd/herd supplies the real driver
// (dispatch via herd, verify via herd verify, approve via git + herd approve,
// renudge via herd shoot).

// ForgeDriver is the side-effecting layer the loop calls. Every method is a
// concrete forge operation; the loop itself stays pure control-flow.
type ForgeDriver interface {
	// LaneState reports builder-lane occupancy so the loop backfills only
	// when a lane is free.
	LaneState(ctx context.Context) LaneState
	// Signals returns the refs whose builder reported done (completed) and
	// the subset that PASSED herd verify (verified) — drained from the
	// callback bus + verify runs.
	Signals(ctx context.Context) (completed, verified map[string]bool)
	// Dispatch claims a to-do card and launches a builder on it.
	Dispatch(ctx context.Context, t *provider.Task) error
	// Review hands a verified build to the reviewer lane (card -> in-review).
	Review(ctx context.Context, t *provider.Task) error
	// Approve merges + approves an in-review card (card -> done) and closes
	// its tab.
	Approve(ctx context.Context, t *provider.Task) error
	// Renudge re-drives a builder that reported done but failed verify.
	Renudge(ctx context.Context, t *provider.Task) error
	// Log surfaces a one-line action to the operator.
	Log(msg string)
}

// ForgeLoopOptions tunes the loop.
type ForgeLoopOptions struct {
	Interval  time.Duration // pause between ticks (default 5s)
	MaxTicks  int           // 0 = run until ctx cancelled or board drained
	StopEmpty bool          // stop once the board is clear and no lane is busy
}

// ForgeLoop runs the async orchestration cycle: each tick it reads lane state
// and completion signals, asks ForgeStep for the next action, and executes it
// through the driver. It keeps lanes saturated and drains the board.
func (e *Engine) ForgeLoop(ctx context.Context, d ForgeDriver, opts ForgeLoopOptions) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	for tick := 0; opts.MaxTicks == 0 || tick < opts.MaxTicks; tick++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		lanes := d.LaneState(ctx)
		completed, verified := d.Signals(ctx)

		action, err := e.ForgeStep(ctx, lanes, completed, verified)
		if err != nil {
			d.Log(fmt.Sprintf("forge: step error: %v", err))
			sleep(ctx, interval)
			continue
		}

		switch action.Kind {
		case ActionApprove:
			d.Log("forge: approve " + action.Ref)
			if err := d.Approve(ctx, action.Task); err != nil {
				d.Log(fmt.Sprintf("forge: approve %s failed: %v", action.Ref, err))
			}
		case ActionReview:
			d.Log("forge: review " + action.Ref)
			if err := d.Review(ctx, action.Task); err != nil {
				d.Log(fmt.Sprintf("forge: review %s failed: %v", action.Ref, err))
			}
		case ActionRenudge:
			d.Log("forge: re-nudge " + action.Ref + " (reported done but failed verify)")
			if err := d.Renudge(ctx, action.Task); err != nil {
				d.Log(fmt.Sprintf("forge: renudge %s failed: %v", action.Ref, err))
			}
		case ActionDispatch:
			d.Log("forge: dispatch " + action.Ref)
			if err := d.Dispatch(ctx, action.Task); err != nil {
				d.Log(fmt.Sprintf("forge: dispatch %s failed: %v", action.Ref, err))
			}
		case ActionIdle:
			if opts.StopEmpty && lanes.Busy == 0 {
				d.Log("forge: board clear and no lane busy — loop complete")
				return nil
			}
		}
		sleep(ctx, interval)
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
