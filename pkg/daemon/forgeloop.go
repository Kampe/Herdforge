package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
//
// FAC-138: LaneState and Signals return an error because an UNKNOWN fleet is
// not an empty fleet. When the herdr read fails, "zero busy lanes" and "no
// completion signals" are indistinguishable from a drained board, so the loop
// used to backfill lanes it could not see. Both now fail the tick closed.
type ForgeDriver interface {
	// LaneState reports builder-lane occupancy so the loop backfills only
	// when a lane is free. An error means capacity is UNKNOWN, never free.
	LaneState(ctx context.Context) (LaneState, error)
	// Signals returns the refs whose builder reported done (completed) and
	// the subset that PASSED herd verify (verified) — drained from the
	// callback bus + verify runs. An error means the signal set is unknown.
	Signals(ctx context.Context) (completed, verified map[string]bool, err error)
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
//
// FAC-150: when the task provider times out, health becomes
// BLOCKED(provider_timeout). The next tick transitions to recovering and
// re-probes without claiming work until a successful bound call returns ok.
//
// FAC-138: an action failure is a FAILED transition, not a log line. Each
// failure is held against its ref until the same action succeeds, and any
// still-unresolved failure is returned as the loop's error so the exit status
// reflects it. The loop also refuses to report a clean stop while a card sits
// in-progress with no builder behind it — that orphan used to look exactly
// like a drained board.
func (e *Engine) ForgeLoop(ctx context.Context, d ForgeDriver, opts ForgeLoopOptions) error {
	interval := opts.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}

	// key ("approve:FAC-1", "lanes", "orphan") -> last reason. Cleared when
	// the same transition succeeds, so a recovered blip does not poison exit.
	failures := map[string]string{}
	fail := func(key, reason string) {
		failures[key] = reason
		d.Log("forge: " + reason)
	}
	// act runs one driver action and records/clears its failure state.
	act := func(kind, ref string, run func() error) {
		key := kind + ":" + ref
		if err := run(); err != nil {
			fail(key, fmt.Sprintf("%s %s failed: %v", kind, ref, err))
			return
		}
		delete(failures, key)
	}

	for tick := 0; opts.MaxTicks == 0 || tick < opts.MaxTicks; tick++ {
		select {
		case <-ctx.Done():
			if err := unresolvedFailures(failures); err != nil {
				return err
			}
			return ctx.Err()
		default:
		}
		if e.ControlRequired && e.ControlReconciler == nil {
			return fmt.Errorf("forge: durable control reconciler is required before board or lane actions")
		}
		if e.ControlReconciler != nil {
			if err := e.ControlReconciler.RunOnce(ctx); err != nil {
				d.Log(fmt.Sprintf("forge: control reconciliation failed: %v", err))
				return fmt.Errorf("forge: control reconciliation failed before lane/board actions: %w", err)
			}
		}

		// timeout → BLOCKED → recovering (probe) → ok on success.
		if e.health.isBlocked() {
			d.Log("forge: " + e.ProviderStatus() + " — not claiming work")
			e.health.beginRecovery()
			d.Log("forge: " + e.ProviderStatus() + " — probing board")
		}

		lanes, err := d.LaneState(ctx)
		if err != nil {
			fail("lanes", fmt.Sprintf("BLOCKED(fleet_state_unknown): lane capacity unreadable, refusing to backfill: %v", err))
			sleep(ctx, interval)
			continue
		}
		delete(failures, "lanes")
		completed, verified, err := d.Signals(ctx)
		if err != nil {
			fail("signals", fmt.Sprintf("BLOCKED(fleet_state_unknown): completion signals unreadable, refusing to act: %v", err))
			sleep(ctx, interval)
			continue
		}
		delete(failures, "signals")

		action, err := e.ForgeStep(ctx, lanes, completed, verified)
		if err != nil {
			d.Log(fmt.Sprintf("forge: step error (%s): %v", e.ProviderStatus(), err))
			sleep(ctx, interval)
			continue
		}

		switch action.Kind {
		case ActionApprove:
			d.Log("forge: approve " + action.Ref)
			act("approve", action.Ref, func() error { return d.Approve(ctx, action.Task) })
		case ActionReview:
			d.Log("forge: review " + action.Ref)
			act("review", action.Ref, func() error { return d.Review(ctx, action.Task) })
		case ActionRenudge:
			d.Log("forge: re-nudge " + action.Ref + " (reported done but failed verify)")
			act("renudge", action.Ref, func() error { return d.Renudge(ctx, action.Task) })
		case ActionDispatch:
			if e.health.isBlocked() {
				d.Log("forge: " + e.ProviderStatus() + " — skip dispatch")
				break
			}
			d.Log("forge: dispatch " + action.Ref)
			act("dispatch", action.Ref, func() error { return d.Dispatch(ctx, action.Task) })
		case ActionIdle:
			if st := e.ProviderStatus(); strings.HasPrefix(st, "BLOCKED") || st == "recovering" {
				d.Log("forge: idle under " + st)
			}
			if lanes.Busy != 0 || e.ProviderHealth().State != ProviderOK {
				break
			}
			// Idle + no busy lane still is not a drained board: a card left
			// in-progress with no builder behind it produces no completion
			// signal at all. The coordinator is then blocked-with-reason, not
			// idle — and a --stop-empty run that exited here abandoned it.
			orphans, err := e.inProgressRefs(ctx)
			switch {
			case err != nil:
				fail("orphan-scan", fmt.Sprintf("BLOCKED(board_unreadable): cannot confirm a drained board: %v", err))
			case len(orphans) > 0:
				fail("orphan", "BLOCKED(orphan_in_progress): "+strings.Join(orphans, ", ")+" in-progress with no builder")
			default:
				delete(failures, "orphan")
				delete(failures, "orphan-scan")
				if !opts.StopEmpty {
					break
				}
				if err := unresolvedFailures(failures); err != nil {
					d.Log("forge: board clear but transitions failed — exiting non-zero")
					return err
				}
				d.Log("forge: board clear and no lane busy — loop complete")
				return nil
			}
		}
		sleep(ctx, interval)
	}
	return unresolvedFailures(failures)
}

// inProgressRefs lists the refs the board still holds in-progress. Used as the
// orphan guard before the loop may claim a drained board.
func (e *Engine) inProgressRefs(ctx context.Context) ([]string, error) {
	tasks, err := e.listTasksBound(ctx, e.Config.TaskProvider.ProjectID, "in-progress")
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(tasks))
	for _, t := range tasks {
		refs = append(refs, t.Ref)
	}
	sort.Strings(refs)
	return refs, nil
}

// unresolvedFailures projects the still-failing transitions into the loop's
// error, so `herd forge --loop` cannot exit 0 on a run that never landed its
// actions. Returns nil when every recorded failure was later cleared.
func unresolvedFailures(failures map[string]string) error {
	if len(failures) == 0 {
		return nil
	}
	keys := make([]string, 0, len(failures))
	for k := range failures {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	reasons := make([]string, 0, len(keys))
	for _, k := range keys {
		reasons = append(reasons, failures[k])
	}
	return fmt.Errorf("forge: %d unresolved transition(s): %s", len(keys), strings.Join(reasons, "; "))
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
