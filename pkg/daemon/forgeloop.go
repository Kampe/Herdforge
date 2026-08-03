package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/server"
)

// cmpOr returns the first non-empty string.
func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

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
	// ControlAddr starts the production control plane (live disk metrics +
	// authorized reclamation) for the loop's lifetime. Empty falls back to
	// HERD_CONTROL_ADDR; both empty disables it.
	ControlAddr string
}

// ForgeLoop runs the async orchestration cycle: each tick it reads lane state
// and completion signals, asks ForgeStep for the next action, and executes it
// through the driver. It keeps lanes saturated and drains the board.
//
// FAC-150: when the task provider times out, health becomes
// BLOCKED(provider_timeout). The next tick transitions to recovering and
// re-probes without claiming work until a successful bound call returns ok.
func (e *Engine) ForgeLoop(ctx context.Context, d ForgeDriver, opts ForgeLoopOptions) (retErr error) {
	interval := opts.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	// Production control plane (FAC-153): live disk metrics + authorized
	// exact-target reclamation for the lifetime of the loop. EXPLICITLY
	// configured means fail closed: the loop must not silently run without
	// the promised health/control plane, and a teardown failure is a real
	// error, not a discarded one.
	var cs *server.ControlServer
	if addr := cmpOr(opts.ControlAddr, os.Getenv(EnvControlAddr)); addr != "" {
		var err error
		cs, err = e.StartControlPlane(ctx, addr, d.Log)
		if err != nil {
			return fmt.Errorf("forge: configured control plane failed to start (failing closed): %w", err)
		}
		d.Log("forge: control server on " + cs.BoundAddr())
		defer func() {
			if stopErr := cs.Stop(context.Background()); stopErr != nil {
				d.Log("forge: control server stop failed: " + stopErr.Error())
				if retErr == nil {
					retErr = fmt.Errorf("control server stop: %w", stopErr)
				}
			}
		}()
	}

	// admitAction is the common disk admission/reservation gate for every
	// disk-growing side effect the loop drives (FAC-153). The reservation
	// is held for the duration of the action.
	admitAction := func(op string) (func(), bool) {
		release, err := preflight.AdmitDiskMutation(op, e.diskPaths()...)
		if err != nil {
			d.Log("forge: " + e.DiskStatus() + " — skip " + op + " (disk pressure)")
			return nil, false
		}
		return release, true
	}

	for tick := 0; opts.MaxTicks == 0 || tick < opts.MaxTicks; tick++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// A configured mandatory control plane that died at runtime blocks
		// the loop immediately — mutations must not continue while the
		// promised health/control surface is gone (FAC-153).
		if cs != nil {
			if serveErr := cs.ServeErr(); serveErr != nil {
				d.Log("forge: BLOCKED(control_plane_dead) — cancelling loop")
				return fmt.Errorf("forge: BLOCKED(control_plane_dead): configured control plane failed at runtime (failing closed): %w", serveErr)
			}
		}

		// timeout → BLOCKED → recovering (probe) → ok on success.
		if e.health.isBlocked() {
			d.Log("forge: " + e.ProviderStatus() + " — not claiming work")
			e.health.beginRecovery()
			d.Log("forge: " + e.ProviderStatus() + " — probing board")
		}

		lanes := d.LaneState(ctx)
		completed, verified := d.Signals(ctx)

		action, err := e.ForgeStep(ctx, lanes, completed, verified)
		if err != nil {
			d.Log(fmt.Sprintf("forge: step error (%s): %v", e.ProviderStatus(), err))
			sleep(ctx, interval)
			continue
		}

		switch action.Kind {
		case ActionApprove:
			release, ok := admitAction("approve")
			if !ok {
				break
			}
			d.Log("forge: approve " + action.Ref)
			if err := d.Approve(ctx, action.Task); err != nil {
				d.Log(fmt.Sprintf("forge: approve %s failed: %v", action.Ref, err))
			}
			release()
		case ActionReview:
			release, ok := admitAction("review")
			if !ok {
				break
			}
			d.Log("forge: review " + action.Ref)
			if err := d.Review(ctx, action.Task); err != nil {
				d.Log(fmt.Sprintf("forge: review %s failed: %v", action.Ref, err))
			}
			release()
		case ActionRenudge:
			release, ok := admitAction("renudge")
			if !ok {
				break
			}
			d.Log("forge: re-nudge " + action.Ref + " (reported done but failed verify)")
			if err := d.Renudge(ctx, action.Task); err != nil {
				d.Log(fmt.Sprintf("forge: renudge %s failed: %v", action.Ref, err))
			}
			release()
		case ActionDispatch:
			if e.health.isBlocked() {
				d.Log("forge: " + e.ProviderStatus() + " — skip dispatch")
				break
			}
			// Fresh graduated disk probe each tick (FAC-153): refuse under
			// hard pressure (also drives BLOCKED → recovering → ok as
			// headroom returns); in the soft band, serialize — dispatch only
			// when no lane is busy, so fan-out degrades before work stops.
			adv := preflight.AdviseDiskPressure("dispatch", e.diskPaths()...)
			if adv.Verdict == preflight.AdviceRefuse {
				d.Log("forge: " + e.DiskStatus() + " — skip dispatch (disk pressure)")
				break
			}
			if adv.Verdict == preflight.AdviceSerialize && lanes.Busy > 0 {
				d.Log(fmt.Sprintf("forge: disk soft pressure — serializing dispatch (%d lane(s) busy)", lanes.Busy))
				break
			}
			release, ok := admitAction("dispatch")
			if !ok {
				break
			}
			d.Log("forge: dispatch " + action.Ref)
			if err := d.Dispatch(ctx, action.Task); err != nil {
				d.Log(fmt.Sprintf("forge: dispatch %s failed: %v", action.Ref, err))
			}
			release()
		case ActionIdle:
			if st := e.ProviderStatus(); strings.HasPrefix(st, "BLOCKED") || st == "recovering" {
				d.Log("forge: idle under " + st)
			}
			if opts.StopEmpty && lanes.Busy == 0 && e.ProviderHealth().State == ProviderOK {
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
