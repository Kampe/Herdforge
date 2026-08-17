package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
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
	// Rejections returns, keyed by ticket ref, every reviewer FAIL that no
	// later PASS has superseded. FAC-140: an error means the review state is
	// UNKNOWN, which must never read as "no card was rejected" — that
	// silently re-arms the merge gate on a FAILed candidate.
	Rejections(ctx context.Context) (map[string]Rejection, error)
	// Reject delivers the reviewer's numbered rejection to the card's
	// authoring worker and proves the worker consumed it. Delivery that
	// cannot be proven is an error, never a silent success.
	Reject(ctx context.Context, t *provider.Task, r Rejection) error
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

// ReconciliationObserver is an observe-only safety sweep. Implementations
// assemble complete source evidence and may persist structured observations,
// but must not close tabs; FAC-180 owns atomic compare-and-close.
type ReconciliationObserver interface {
	ObserveReconciliation(ctx context.Context) error
}

// ForgeLoopOptions tunes the loop.
type ForgeLoopOptions struct {
	Interval  time.Duration // pause between ticks (default 5s)
	MaxTicks  int           // 0 = run until ctx cancelled or board drained
	StopEmpty bool          // stop once the board is clear and no lane is busy

	// Feedback is the periodic fleet-wide census runner (FAC-222). When set,
	// the loop calls it every FeedbackInterval ticks so a lane that goes quiet
	// is REPORTED rather than discovered by polling. A nil Feedback preserves
	// the prior behavior (polling-only) — it is the backstop, not the primary
	// signal. A Feedback error is logged, never fatal: a census failure must
	// not stop the forge loop the way a transition failure does.
	Feedback         func(ctx context.Context) error
	FeedbackInterval int // call Feedback every N ticks (default 0 = every tick when Feedback is set)

	// ApproveSuppressionPath persists receiptless legacy approve tombstones.
	// Empty keeps the ledger process-local, which is useful for embedders and
	// tests; production forge-loop supplies a repo-relative path.
	ApproveSuppressionPath string
	// ApproveRetryRefs is an explicit operator request to clear suppression for
	// a ref and try approval once more.
	ApproveRetryRefs map[string]bool
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
	approveSuppressions, err := loadApproveSuppressionLedger(opts.ApproveSuppressionPath)
	if err != nil {
		return fmt.Errorf("forge: approve suppression state unavailable: %w", err)
	}
	// routed holds ref -> the FAILed SHA whose rejection this loop already
	// proved delivered. It is the (ref, SHA) idempotency key: the reviewer's
	// FAIL stays in the ledger until a fresh candidate earns a fresh PASS, so
	// without it every tick would re-prompt the worker with the same
	// rejection.
	// ponytail: in-memory, per coordinator run. A restart re-delivers a
	// still-outstanding rejection exactly once, which is recovery, not spam;
	// make it durable only if restarts become frequent enough to notice.
	routed := map[string]string{}
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
	observe := func() {
		if r, ok := d.(ReconciliationObserver); ok {
			if err := r.ObserveReconciliation(ctx); err != nil {
				d.Log(fmt.Sprintf("forge: reconciliation observe BLOCKED: %v", err))
			}
		}
	}
	// Startup recovery is observe-only and fail-visible.
	observe()

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
		// Periodic reconciliation is the safety net for lost callbacks.
		observe()

		// FAC-222: periodic fleet-wide feedback census. A lane that goes quiet
		// is REPORTED rather than discovered by polling. A census failure is
		// logged, never fatal — it must not stop the forge loop.
		if opts.Feedback != nil {
			feedbackInterval := opts.FeedbackInterval
			if feedbackInterval <= 0 {
				feedbackInterval = 1
			}
			if tick%feedbackInterval == 0 {
				if ferr := opts.Feedback(ctx); ferr != nil {
					d.Log(fmt.Sprintf("forge: feedback census failed: %v", ferr))
				}
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
		rejections, err := d.Rejections(ctx)
		if err != nil {
			fail("rejections", fmt.Sprintf("BLOCKED(review_state_unknown): review verdicts unreadable, refusing to approve or route: %v", err))
			sleep(ctx, interval)
			continue
		}
		delete(failures, "rejections")

		action, err := e.ForgeStep(ctx, lanes, completed, verified, rejections)
		if err != nil {
			d.Log(fmt.Sprintf("forge: step error (%s): %v", e.ProviderStatus(), err))
			sleep(ctx, interval)
			continue
		}
		// Approval is deliberately the highest-priority board transition, but
		// it must not be allowed to strand a newer worker completion. Admit one
		// verified (or re-nudge one unverified) completion before attempting an
		// approval, so a receiptless legacy refusal cannot end the tick before
		// callback processing and review admission have happened.
		if action.Kind == ActionApprove {
			callbackAction, callbackErr := e.completionAction(ctx, completed, verified)
			if callbackErr != nil {
				fail("completion", fmt.Sprintf("completion admission failed: %v", callbackErr))
			} else if callbackAction != nil {
				switch callbackAction.Kind {
				case ActionReview:
					d.Log("forge: review " + callbackAction.Ref + " (before approve)")
					act("review", callbackAction.Ref, func() error { return d.Review(ctx, callbackAction.Task) })
				case ActionRenudge:
					d.Log("forge: re-nudge " + callbackAction.Ref + " (reported done but failed verify; before approve)")
					act("renudge", callbackAction.Ref, func() error { return d.Renudge(ctx, callbackAction.Task) })
				}
			}
		}

		switch action.Kind {
		case ActionApprove:
			d.Log("forge: approve " + action.Ref)
			candidateState, receiptState := approveSuppressionState(action.Task)
			legacyKey := approveSuppressionKey(action.Ref, hsync.ErrNoEvidence.Error(), candidateState, receiptState)
			explicitRetry := opts.ApproveRetryRefs[action.Ref]
			if explicitRetry {
				if err := approveSuppressions.removeRef(action.Ref); err != nil {
					reason := fmt.Sprintf("approve %s retry state reset failed: %v", action.Ref, err)
					failures["approve:"+action.Ref] = reason
					d.Log("forge: " + reason)
					break
				}
				d.Log("forge: approve " + action.Ref + " explicit operator retry")
			}
			if !explicitRetry && approveSuppressions.has(legacyKey) {
				d.Log(fmt.Sprintf("forge: approve %s suppressed BLOCKED-legacy (receiptless refusal; state unchanged)", action.Ref))
				if _, alreadyBlocked := failures["approve:"+action.Ref]; !alreadyBlocked {
					failures["approve:"+action.Ref] = fmt.Sprintf("approve %s BLOCKED-legacy suppressed (receiptless refusal; state unchanged)", action.Ref)
				}
				break
			}
			approveErr := d.Approve(ctx, action.Task)
			if approveErr == nil {
				if err := approveSuppressions.removeRef(action.Ref); err != nil {
					reason := fmt.Sprintf("approve %s suppression cleanup failed: %v", action.Ref, err)
					failures["approve:"+action.Ref] = reason
					d.Log("forge: " + reason)
				} else {
					delete(failures, "approve:"+action.Ref)
				}
				// Post-merge board/session/worktree truth is observed before the
				// next capacity decision; no cleanup mutation occurs here.
				observe()
				break
			}
			if errors.Is(approveErr, hsync.ErrNoEvidence) {
				if err := approveSuppressions.record(approveSuppression{
					Key: legacyKey, Ref: action.Ref, Reason: hsync.ErrNoEvidence.Error(), CandidateState: candidateState,
					ReceiptState: receiptState, BlockedAt: time.Now().UTC().Format(time.RFC3339Nano),
				}); err != nil {
					approveErr = fmt.Errorf("legacy refusal could not be persisted: %w", err)
				} else {
					d.Log(fmt.Sprintf("forge: approve %s recorded BLOCKED-legacy (receiptless refusal)", action.Ref))
				}
			}
			reason := fmt.Sprintf("approve %s failed: %v", action.Ref, approveErr)
			failures["approve:"+action.Ref] = reason
			d.Log("forge: " + reason)
			// Post-merge board/session/worktree truth is observed before the
			// next capacity decision; no cleanup mutation occurs here.
			observe()
		case ActionReview:
			d.Log("forge: review " + action.Ref)
			act("review", action.Ref, func() error { return d.Review(ctx, action.Task) })
		case ActionRenudge:
			d.Log("forge: re-nudge " + action.Ref + " (reported done but failed verify)")
			act("renudge", action.Ref, func() error { return d.Renudge(ctx, action.Task) })
		case ActionReject:
			r := *action.Rejection
			if routed[action.Ref] == r.SHA {
				// Already delivered for this exact candidate. Nothing more is
				// owed until the worker publishes a fresh SHA and a fresh
				// review lands on it.
				break
			}
			d.Log("forge: route review FAIL for " + action.Ref + " @ " + shortSHA(r.SHA) + " back to its worker")
			act("reject", action.Ref, func() error {
				if err := d.Reject(ctx, action.Task, r); err != nil {
					return err
				}
				// Marked only on PROVEN delivery: an unproven prompt must be
				// retried next tick, not recorded as routed.
				routed[action.Ref] = r.SHA
				return nil
			})
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

// completionAction selects the highest-priority completed in-progress card
// without considering approval, dispatch, or rejection state. ForgeStep uses
// the same selector for its normal precedence, while ForgeLoop also invokes it
// ahead of approval so a legacy approval refusal cannot suppress a fresh
// completion in the current tick.
func (e *Engine) completionAction(ctx context.Context, completed, verified map[string]bool) (*ForgeAction, error) {
	if len(completed) == 0 {
		return nil, nil
	}
	inProgress, err := e.listTasksBound(ctx, e.Config.TaskProvider.ProjectID, "in-progress")
	if err != nil {
		return nil, formatProviderStepError("list in-progress", err)
	}
	var ready, failed []*provider.Task
	for _, t := range inProgress {
		if !completed[t.Ref] {
			continue
		}
		if verified[t.Ref] {
			ready = append(ready, t)
		} else {
			failed = append(failed, t)
		}
	}
	if t := firstByPriority(ready); t != nil {
		return &ForgeAction{Kind: ActionReview, Ref: t.Ref, Task: t}, nil
	}
	if t := firstByPriority(failed); t != nil {
		return &ForgeAction{Kind: ActionRenudge, Ref: t.Ref, Task: t}, nil
	}
	return nil, nil
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

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
