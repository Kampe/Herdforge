package lifecycle

import (
	"errors"
	"fmt"
	"time"
)

// RecoveryAction records one reconciliation sweep decision.
type RecoveryAction struct {
	TaskRef  string
	From     State
	To       State
	Replayed bool
}

const defaultStaleAfter = 5 * time.Minute

// Reconciler sweeps lifecycle_task_state for tasks that have made no
// forward progress in StaleAfter and folds them into an explicit
// Recovering (first sweep) or Blocked (still stale on a later sweep)
// state via Machine.Transition — never leaving a task silently stuck in a
// state its last durable event no longer backs.
//
// ponytail: this only marks a task Recovering/Blocked; it does not itself
// resume the external side effect (worktree, Herdr session, verification
// run). Resuming from Recovering is FAC-120/121/122's job — each owns the
// boundary it can actually re-check (lease liveness, dispatch receipt,
// verification receipt) and calls Machine.Transition back out of
// Recovering once it has re-established truth. Add an active resume hook
// here if a boundary needs the reconciler itself to retry, not just flag.
type Reconciler struct {
	machine *Machine
	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
	// StaleAfter is how long a task may sit without a new event before the
	// reconciler acts on it.
	StaleAfter time.Duration
	// Actor is recorded on every event the reconciler produces.
	Actor string
}

// NewReconciler builds a Reconciler over machine with sane defaults.
func NewReconciler(machine *Machine) *Reconciler {
	return &Reconciler{
		machine:    machine,
		Now:        time.Now,
		StaleAfter: defaultStaleAfter,
		Actor:      "lifecycle-reconciler",
	}
}

// Reconcile runs one sweep. Call it on startup and on a periodic timer;
// it is safe to call repeatedly — each decision is idempotency-keyed on
// the task's current sequence number, so re-running a sweep that hasn't
// seen forward progress replays the same durable event instead of
// piling up duplicates.
func (r *Reconciler) Reconcile() ([]RecoveryAction, error) {
	states, err := r.machine.EventStore().AllTaskStates()
	if err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}

	now := r.Now()
	var actions []RecoveryAction
	var errs []error

	for _, ts := range states {
		if IsTerminal(ts.State) {
			continue
		}
		if now.Sub(ts.UpdatedAt) <= r.StaleAfter {
			continue
		}

		target := StateRecovering
		if ts.State == StateRecovering {
			target = StateBlocked
		}
		if !ValidTransition(ts.State, target) {
			continue
		}

		key := recoveryIdempotencyKey(ts.TaskRef, ts.Seq, target)
		res, err := r.machine.Transition(TransitionRequest{
			TaskRef:         ts.TaskRef,
			Repo:            ts.Repo,
			To:              target,
			Actor:           r.Actor,
			IdempotencyKey:  key,
			LeaseGeneration: ts.LeaseGeneration,
			Branch:          ts.Branch,
			CandidateSHA:    ts.CandidateSHA,
			EvidenceDigest:  fmt.Sprintf("reconciler: no new event for task %s since seq %d", ts.TaskRef, ts.Seq),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("reconcile %s: %w", ts.TaskRef, err))
			continue
		}
		actions = append(actions, RecoveryAction{
			TaskRef: ts.TaskRef, From: ts.State, To: target, Replayed: res.Replayed,
		})
	}

	if len(errs) > 0 {
		return actions, errors.Join(errs...)
	}
	return actions, nil
}

func recoveryIdempotencyKey(taskRef string, seq int64, to State) string {
	return fmt.Sprintf("reconcile:%s:%d:%s", taskRef, seq, to)
}
