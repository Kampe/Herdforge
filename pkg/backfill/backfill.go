// Package backfill turns the durable lifecycle log into IMMEDIATE capacity
// backfill: when a build completes, a review lands, or a gate frees, the
// coordinator re-evaluates within a settle window instead of waiting out the
// next periodic sweep. The periodic sweep is still there, demoted to what it
// should always have been — a safety net for events that were never delivered
// and for drift the fleet caused outside the event log (a tab closed by hand,
// a lane freed by a crash).
//
// Three properties carry that, and each exists because its absence cost real
// fleet time:
//
//   - SETTLE, per repository/task. A finishing agent emits a burst of
//     transitions; evaluating each one launches a thundering herd of board
//     reads. One quiet window per key collapses the burst into one evaluation
//     that still carries the burst's HIGHEST sequence.
//   - RECONCILE. A dropped callback must cost latency, never work. The sweep
//     re-reads from the last applied sequence, so an event nobody announced is
//     picked up on the next interval.
//   - SINGLE-ACTIVE. Two coordinators evaluating the same log double-dispatch
//     the same task. Ownership is a lease; a watcher that does not hold it
//     observes and plans nothing.
//
// The watcher itself performs no side effects. It produces a Plan and hands
// it to an Executor, which must be idempotent: delivery is at-least-once by
// construction (a crash between Execute and the sequence commit replays the
// plan), and the durable outbox is what makes a replayed plan free.
//
// ponytail: this package is the seam FAC-138 consumes. It deliberately does
// not wire itself into daemon.ForgeLoop — the loop's driver composition is
// that card's scope, and a watcher that reaches into it here would be two
// tickets in one diff.
package backfill

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrUnknownState is returned when the capacity gate cannot report the truth
// (provider timeout, Herdr unreachable, Git state indeterminate). It is
// deliberately NOT the same as "no capacity": an unknown gate blocks the
// affected evaluation and leaves the sequence cursor untouched, so the next
// pass retries the same events rather than recording that there was nothing
// to do.
var ErrUnknownState = errors.New("backfill: capacity state is unknown")

// Defaults. Settle is short because harvest lag is the cost being paid;
// ReconcileInterval is long because it is a safety net, not the mechanism.
const (
	DefaultSettle            = 2 * time.Second
	DefaultReconcileInterval = 60 * time.Second
	DefaultBatch             = 256
)

// Evaluation reasons, reported on Plan and Result.
const (
	ReasonIdle      = "idle"
	ReasonSettle    = "settle"
	ReasonReconcile = "reconcile"
	ReasonNotActive = "not-active"
)

// Event is one durable lifecycle transition, reduced to what scheduling
// needs. Sequence is the source's monotone cursor.
//
// Callbacks stay lean on purpose: a full task packet is NOT delivered inline.
// The Executor re-reads the packet for the refs it is given, so a watcher
// restart never depends on payloads it happened to have in memory.
type Event struct {
	Sequence int64
	Repo     string
	TaskRef  string
	State    string
}

func (e Event) key() string { return e.Repo + "\x00" + e.TaskRef }

// EventSource is the durable log being tailed. Since returns events with
// Sequence > after, ascending, at most limit of them.
type EventSource interface {
	Since(ctx context.Context, after int64, limit int) ([]Event, error)
}

// Capacity is the backpressure snapshot the gate reports. Both fields are
// counts of REAL lanes/queue depth; a negative value is treated as unknown
// rather than clamped, because a clamped bogus reading launches work.
type Capacity struct {
	FreeBuilderLanes int
	PendingReviews   int
}

// Gate reports current capacity. Returning an error means UNKNOWN — see
// ErrUnknownState. It must never report zero to express "I could not tell".
type Gate interface {
	Capacity(ctx context.Context) (Capacity, error)
}

// Plan is one settled scheduling decision.
type Plan struct {
	Reason string
	// HighestSequence is the highest source sequence this plan covers. It is
	// the burst's newest event, never an older one that happened to be read
	// first.
	HighestSequence int64
	// Refs are the task refs whose evidence must be advanced before more
	// builder work is claimed, deduplicated and sorted.
	Refs []string
	// DrainReviews is how much review pressure the executor must absorb.
	DrainReviews int
	// LaunchBuilders is the builder headroom left AFTER reserving a lane for
	// every pending review. Review pressure is therefore drained before any
	// excess builder is launched.
	LaunchBuilders int
}

func (p Plan) empty() bool {
	return len(p.Refs) == 0 && p.DrainReviews == 0 && p.LaunchBuilders == 0
}

// Executor performs the plan. It MUST be idempotent (see package doc).
type Executor interface {
	Execute(ctx context.Context, plan Plan) error
}

// Lease is the single-active guard. Acquire reports whether owner holds
// ownership right now; a watcher that does not hold it plans nothing.
type Lease interface {
	Acquire(ctx context.Context, owner string, now time.Time) (bool, error)
	Release(ctx context.Context, owner string) error
}

// Stats is the observable surface: how far the watcher has applied, how far
// behind it was at the last pass, and whether it is currently blocked on an
// unknown gate.
type Stats struct {
	LastSequence  int64
	LagEvents     int
	Active        bool
	Unknown       bool
	Evaluations   int64
	LastEvaluated time.Time
	LastError     string
}

// Result is what one Step did.
type Result struct {
	Reason   string
	Executed bool
	Plan     Plan
}

// Watcher is settle-driven with a periodic reconciliation floor. The zero
// value plus Source/Gate/Exec/Lease/Owner is usable; defaults are applied on
// first use.
type Watcher struct {
	Source EventSource
	Gate   Gate
	Exec   Executor
	Lease  Lease
	Owner  string

	// Settle is the per-key quiet window a burst must clear.
	Settle time.Duration
	// ReconcileInterval is the safety sweep floor. The sweep ignores the
	// settle window, so a key that never goes quiet cannot starve.
	ReconcileInterval time.Duration
	// Batch bounds one Since read.
	Batch int
	// Now is overridable for tests; defaults to time.Now.
	Now func() time.Time
	// Log, when set, receives one line per failed Step from Run.
	Log func(string)

	mu            sync.Mutex
	ready         bool
	hints         map[string]time.Time
	lastSeq       int64
	lastReconcile time.Time
	stats         Stats
	wake          chan struct{}
}

func (w *Watcher) init() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ready {
		return
	}
	if w.Settle <= 0 {
		w.Settle = DefaultSettle
	}
	if w.ReconcileInterval <= 0 {
		w.ReconcileInterval = DefaultReconcileInterval
	}
	if w.Batch <= 0 {
		w.Batch = DefaultBatch
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	w.hints = map[string]time.Time{}
	w.wake = make(chan struct{}, 1)
	w.ready = true
}

func (w *Watcher) now() time.Time {
	w.mu.Lock()
	f := w.Now
	w.mu.Unlock()
	return f()
}

// Resume restores the cursor a previous watcher had applied, so a restart
// does not replay work the executor already performed.
func (w *Watcher) Resume(sequence int64) {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	if sequence > w.lastSeq {
		w.lastSeq = sequence
		w.stats.LastSequence = sequence
	}
}

// Stats returns the current observable state.
func (w *Watcher) Stats() Stats {
	w.init()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

// Notify records that a key produced durable events. It is a HINT, never the
// data: the watcher still reads the log itself, so a lost, duplicated, or
// out-of-order notification costs latency at worst.
func (w *Watcher) Notify(repo, taskRef string) {
	w.init()
	w.mu.Lock()
	w.hints[Event{Repo: repo, TaskRef: taskRef}.key()] = w.Now()
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Step runs one pass. It is the whole scheduler: everything Run does is call
// this on a timer and on wake-ups, which is what makes the behaviour testable
// against a fake clock.
func (w *Watcher) Step(ctx context.Context) (Result, error) {
	w.init()
	if w.Source == nil || w.Gate == nil || w.Exec == nil || w.Lease == nil {
		return Result{Reason: ReasonIdle}, fmt.Errorf("backfill: watcher requires source, gate, executor and lease")
	}
	now := w.now()

	w.mu.Lock()
	// A zero lastReconcile means "never swept": reconcile on the first Step
	// so a restart re-derives truth before trusting any hint.
	reconcileDue := w.lastReconcile.IsZero() || now.Sub(w.lastReconcile) >= w.ReconcileInterval
	// settled: keys that went quiet and are ready to evaluate.
	// noisy: keys still inside their window, whose events are held back.
	// A key with NO hint is neither — nobody claims it is mid-burst, so its
	// events (a dropped callback, a reconciliation find) are never deferred.
	settled := map[string]time.Time{}
	noisy := map[string]bool{}
	for key, at := range w.hints {
		if now.Sub(at) >= w.Settle {
			settled[key] = at
			continue
		}
		noisy[key] = true
	}
	cursor, batch := w.lastSeq, w.Batch
	w.mu.Unlock()

	reason := ReasonIdle
	switch {
	case reconcileDue:
		reason = ReasonReconcile
	case len(settled) > 0:
		reason = ReasonSettle
	default:
		return Result{Reason: ReasonIdle}, nil
	}

	active, err := w.Lease.Acquire(ctx, w.Owner, now)
	if err != nil {
		w.fail(fmt.Sprintf("lease: %v", err))
		return Result{Reason: reason}, fmt.Errorf("backfill: acquire ownership: %w", err)
	}
	if !active {
		w.mu.Lock()
		w.stats.Active = false
		if reconcileDue {
			// Consume the timer so a passive watcher does not spin, but keep
			// the hints: on takeover it should act on them immediately.
			w.lastReconcile = now
		}
		w.mu.Unlock()
		return Result{Reason: ReasonNotActive}, nil
	}

	events, err := w.Source.Since(ctx, cursor, batch)
	if err != nil {
		w.fail(fmt.Sprintf("read events: %v", err))
		return Result{Reason: reason}, fmt.Errorf("backfill: read events since %d: %w", cursor, err)
	}

	// On the settle path a key that is still noisy is held back. Its events
	// are not dropped: the cursor stops short of the first deferred sequence,
	// so the next pass re-reads them.
	var included []Event
	var firstDeferred int64
	for _, ev := range events {
		if reason == ReasonSettle && noisy[ev.key()] {
			if firstDeferred == 0 {
				firstDeferred = ev.Sequence
			}
			continue
		}
		included = append(included, ev)
	}

	capacity, err := w.Gate.Capacity(ctx)
	if err != nil {
		w.blockUnknown(fmt.Sprintf("capacity gate: %v", err), len(events))
		return Result{Reason: reason}, fmt.Errorf("%w: %v", ErrUnknownState, err)
	}
	if capacity.FreeBuilderLanes < 0 || capacity.PendingReviews < 0 {
		msg := fmt.Sprintf("capacity gate reported an impossible snapshot: %+v", capacity)
		w.blockUnknown(msg, len(events))
		return Result{Reason: reason}, fmt.Errorf("%w: %s", ErrUnknownState, msg)
	}

	plan := Plan{
		Reason:          reason,
		HighestSequence: highestSequence(included),
		Refs:            refsOf(included),
		DrainReviews:    capacity.PendingReviews,
		LaunchBuilders:  capacity.FreeBuilderLanes - capacity.PendingReviews,
	}
	if plan.LaunchBuilders < 0 {
		plan.LaunchBuilders = 0
	}

	executed := false
	if !plan.empty() {
		if err := w.Exec.Execute(ctx, plan); err != nil {
			// No cursor movement: an unexecuted plan must come back.
			w.fail(fmt.Sprintf("execute: %v", err))
			return Result{Reason: reason, Plan: plan}, fmt.Errorf("backfill: execute plan: %w", err)
		}
		executed = true
	}

	w.commit(now, reason, plan, settled, len(events), len(included), firstDeferred, executed)
	return Result{Reason: reason, Executed: executed, Plan: plan}, nil
}

func (w *Watcher) commit(now time.Time, reason string, plan Plan, settled map[string]time.Time, fetched, applied int, firstDeferred int64, executed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	advance := plan.HighestSequence
	if firstDeferred > 0 && firstDeferred-1 < advance {
		advance = firstDeferred - 1
	}
	if advance > w.lastSeq {
		w.lastSeq = advance
	}
	for key, at := range settled {
		// Only retire the exact hint this pass acted on. A Notify that
		// arrived while the plan was executing must survive, or its events
		// would wait for the reconciliation floor.
		if cur, ok := w.hints[key]; ok && cur.Equal(at) {
			delete(w.hints, key)
		}
	}
	if reason == ReasonReconcile {
		w.lastReconcile = now
	}
	w.stats.Active = true
	w.stats.Unknown = false
	w.stats.LastError = ""
	w.stats.LastSequence = w.lastSeq
	w.stats.LagEvents = fetched - applied
	if executed {
		w.stats.Evaluations++
		w.stats.LastEvaluated = now
	}
}

func (w *Watcher) fail(msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.LastError = msg
}

// blockUnknown records that the pass was blocked by indeterminate state. It
// leaves lastSeq and lastReconcile alone on purpose: the events stay
// unapplied and the next Step retries them immediately instead of waiting a
// full interval, and nothing anywhere records "no work".
func (w *Watcher) blockUnknown(msg string, pending int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Unknown = true
	w.stats.LastError = msg
	w.stats.LagEvents = pending
}

// Run drives Step until ctx is cancelled, then releases ownership. It returns
// ctx.Err() — a cancelled Run is a graceful shutdown, not a failure.
func (w *Watcher) Run(ctx context.Context) error {
	w.init()
	poll := w.Settle / 2
	if poll <= 0 {
		poll = time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		if _, err := w.Step(ctx); err != nil && ctx.Err() == nil {
			w.logf("backfill: %v", err)
		}
		select {
		case <-ctx.Done():
			// ctx is already done, so release on a fresh bounded context —
			// otherwise shutdown could not hand ownership over and the next
			// coordinator would wait out the lease's stale timeout.
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := w.Lease.Release(rctx, w.Owner)
			cancel()
			if err != nil {
				w.logf("backfill: release ownership: %v", err)
			}
			w.mu.Lock()
			w.stats.Active = false
			w.mu.Unlock()
			return ctx.Err()
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

func (w *Watcher) logf(format string, args ...any) {
	w.mu.Lock()
	log := w.Log
	w.mu.Unlock()
	if log != nil {
		log(fmt.Sprintf(format, args...))
	}
}

func highestSequence(events []Event) int64 {
	var high int64
	for _, ev := range events {
		if ev.Sequence > high {
			high = ev.Sequence
		}
	}
	return high
}

func refsOf(events []Event) []string {
	seen := map[string]bool{}
	var refs []string
	for _, ev := range events {
		if ev.TaskRef == "" || seen[ev.TaskRef] {
			continue
		}
		seen[ev.TaskRef] = true
		refs = append(refs, ev.TaskRef)
	}
	sort.Strings(refs)
	return refs
}
