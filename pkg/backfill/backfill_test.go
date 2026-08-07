package backfill

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// --- fakes -----------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeSource struct {
	mu     sync.Mutex
	events []Event
	err    error
	reads  int
}

func (s *fakeSource) append(evs ...Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evs...)
}

func (s *fakeSource) Since(_ context.Context, after int64, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	var out []Event
	for _, ev := range s.events {
		if ev.Sequence > after {
			out = append(out, ev)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type fakeGate struct {
	mu  sync.Mutex
	cap Capacity
	err error
}

func (g *fakeGate) Capacity(context.Context) (Capacity, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cap, g.err
}

func (g *fakeGate) set(c Capacity, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cap, g.err = c, err
}

type fakeExec struct {
	mu    sync.Mutex
	plans []Plan
	err   error
	fired chan struct{}
}

func (e *fakeExec) Execute(_ context.Context, p Plan) error {
	e.mu.Lock()
	if e.err != nil {
		err := e.err
		e.mu.Unlock()
		return err
	}
	e.plans = append(e.plans, p)
	fired := e.fired
	e.mu.Unlock()
	if fired != nil {
		select {
		case fired <- struct{}{}:
		default:
		}
	}
	return nil
}

func (e *fakeExec) snapshot() []Plan {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Plan(nil), e.plans...)
}

// fakeLease is a shared single-active lease: the first owner to acquire keeps
// it until it releases.
type fakeLease struct {
	mu      sync.Mutex
	holder  string
	err     error
	release int
}

func (l *fakeLease) Acquire(_ context.Context, owner string, _ time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return false, l.err
	}
	if l.holder == "" {
		l.holder = owner
	}
	return l.holder == owner, nil
}

func (l *fakeLease) Release(_ context.Context, owner string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.release++
	if l.holder == owner {
		l.holder = ""
	}
	return nil
}

func (l *fakeLease) heldBy() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder
}

type harness struct {
	w      *Watcher
	clock  *fakeClock
	source *fakeSource
	gate   *fakeGate
	exec   *fakeExec
	lease  *fakeLease
}

func newHarness(t *testing.T, capacity Capacity) *harness {
	t.Helper()
	h := &harness{
		clock:  newClock(),
		source: &fakeSource{},
		gate:   &fakeGate{cap: capacity},
		exec:   &fakeExec{},
		lease:  &fakeLease{},
	}
	h.w = &Watcher{
		Source:            h.source,
		Gate:              h.gate,
		Exec:              h.exec,
		Lease:             h.lease,
		Owner:             "coordinator-a",
		Settle:            2 * time.Second,
		ReconcileInterval: time.Hour,
		Now:               h.clock.Now,
	}
	return h
}

// startup drains the first Step, whose only job is the restart reconciliation
// sweep. Tests that care about event-driven behaviour run after it.
func (h *harness) startup(t *testing.T) {
	t.Helper()
	if _, err := h.w.Step(context.Background()); err != nil {
		t.Fatalf("startup step: %v", err)
	}
}

func (h *harness) step(t *testing.T) Result {
	t.Helper()
	res, err := h.w.Step(context.Background())
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	return res
}

// --- tests -----------------------------------------------------------------

// Acceptance: a completion event backfills compatible capacity without
// waiting for the periodic interval.
func TestStep_SettleBackfillsWithoutWaitingForReconcileInterval(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 2})
	h.startup(t)
	before := len(h.exec.snapshot())

	h.source.append(Event{Sequence: 1, Repo: "herdforge", TaskRef: "FAC-1", State: "verified"})
	h.w.Notify("herdforge", "FAC-1")

	// Not yet quiet: nothing may fire.
	if res := h.step(t); res.Executed {
		t.Fatalf("evaluated inside the settle window: %+v", res.Plan)
	}

	h.clock.advance(2 * time.Second)
	res := h.step(t)
	if !res.Executed || res.Reason != ReasonSettle {
		t.Fatalf("expected a settle-driven evaluation, got %+v", res)
	}
	if res.Plan.HighestSequence != 1 || len(res.Plan.Refs) != 1 || res.Plan.Refs[0] != "FAC-1" {
		t.Fatalf("plan does not carry the completion: %+v", res.Plan)
	}
	if res.Plan.LaunchBuilders != 2 {
		t.Fatalf("free capacity was not backfilled: %+v", res.Plan)
	}
	if got := len(h.exec.snapshot()); got != before+1 {
		t.Fatalf("expected exactly one new execution, got %d (was %d)", got, before)
	}
	// The reconcile interval is an hour; the clock moved 2s.
	if h.clock.Now().Sub(newClock().t) >= h.w.ReconcileInterval {
		t.Fatal("test advanced past the reconcile interval — it proves nothing about settle")
	}
}

// Acceptance: event bursts result in ONE settled evaluation without losing
// the highest sequence.
func TestStep_BurstCollapsesToOneEvaluationKeepingHighestSequence(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 1})
	h.startup(t)
	before := len(h.exec.snapshot())

	for seq := int64(1); seq <= 3; seq++ {
		h.source.append(Event{Sequence: seq, Repo: "herdforge", TaskRef: "FAC-7", State: "progress"})
		h.w.Notify("herdforge", "FAC-7")
		h.clock.advance(500 * time.Millisecond) // still inside the 2s window
		if res := h.step(t); res.Executed {
			t.Fatalf("burst event %d fired an evaluation: %+v", seq, res.Plan)
		}
	}

	h.clock.advance(2 * time.Second)
	res := h.step(t)
	if !res.Executed {
		t.Fatal("burst never settled")
	}
	if res.Plan.HighestSequence != 3 {
		t.Fatalf("settled evaluation lost the newest event: %+v", res.Plan)
	}
	if got := len(h.exec.snapshot()); got != before+1 {
		t.Fatalf("burst produced %d evaluations, want exactly 1", got-before)
	}
	if h.w.Stats().LastSequence != 3 {
		t.Fatalf("cursor did not advance to the burst head: %+v", h.w.Stats())
	}
}

// Acceptance: a deliberately dropped callback is recovered by reconciliation.
func TestStep_ReconcileRecoversDroppedCallback(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 1})
	h.startup(t)
	before := len(h.exec.snapshot())

	// Durable event, no Notify — the callback was dropped on the floor.
	h.source.append(Event{Sequence: 9, Repo: "herdforge", TaskRef: "FAC-42", State: "verified"})

	h.clock.advance(10 * time.Second)
	if res := h.step(t); res.Executed {
		t.Fatalf("un-announced event fired before reconciliation: %+v", res.Plan)
	}

	h.clock.advance(time.Hour)
	res := h.step(t)
	if !res.Executed || res.Reason != ReasonReconcile {
		t.Fatalf("reconciliation did not recover the dropped callback: %+v", res)
	}
	if res.Plan.HighestSequence != 9 || len(res.Plan.Refs) != 1 || res.Plan.Refs[0] != "FAC-42" {
		t.Fatalf("recovered plan is wrong: %+v", res.Plan)
	}
	if got := len(h.exec.snapshot()); got != before+1 {
		t.Fatalf("expected one recovery evaluation, got %d", got-before)
	}
}

// Acceptance: two watchers cannot double-dispatch the same task.
func TestStep_SecondWatcherCannotDoubleDispatch(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 2})
	passenger := &fakeExec{}
	other := &Watcher{
		Source:            h.source,
		Gate:              h.gate,
		Exec:              passenger,
		Lease:             h.lease, // SAME lease
		Owner:             "coordinator-b",
		Settle:            2 * time.Second,
		ReconcileInterval: time.Hour,
		Now:               h.clock.Now,
	}

	h.source.append(Event{Sequence: 1, Repo: "herdforge", TaskRef: "FAC-5", State: "verified"})
	h.startup(t) // coordinator-a takes ownership

	res, err := other.Step(context.Background())
	if err != nil {
		t.Fatalf("passive step: %v", err)
	}
	if res.Reason != ReasonNotActive || res.Executed {
		t.Fatalf("non-owner evaluated: %+v", res)
	}
	if plans := passenger.snapshot(); len(plans) != 0 {
		t.Fatalf("non-owner dispatched %d plans", len(plans))
	}
	if other.Stats().Active {
		t.Fatal("non-owner reports itself active")
	}
	if h.lease.heldBy() != "coordinator-a" {
		t.Fatalf("ownership moved to %q", h.lease.heldBy())
	}

	// Ownership handover: once A releases, B evaluates the same log.
	if err := h.lease.Release(context.Background(), "coordinator-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	h.clock.advance(time.Hour)
	res, err = other.Step(context.Background())
	if err != nil {
		t.Fatalf("takeover step: %v", err)
	}
	if !res.Executed || res.Plan.HighestSequence != 1 {
		t.Fatalf("watcher B did not take over: %+v", res)
	}
}

// Acceptance: unknown provider/Herdr/Git state blocks affected transitions
// but does not fabricate zero work.
func TestStep_UnknownCapacityBlocksAndKeepsWorkPending(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 3})
	h.startup(t)
	before := len(h.exec.snapshot())

	h.source.append(Event{Sequence: 4, Repo: "herdforge", TaskRef: "FAC-11", State: "verified"})
	h.w.Notify("herdforge", "FAC-11")
	h.clock.advance(2 * time.Second)

	h.gate.set(Capacity{}, errors.New("herdr status timed out"))
	_, err := h.w.Step(context.Background())
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("unknown gate did not block: %v", err)
	}
	if got := len(h.exec.snapshot()); got != before {
		t.Fatalf("blocked pass still executed %d plans", got-before)
	}
	st := h.w.Stats()
	if !st.Unknown {
		t.Fatal("unknown state is not observable")
	}
	if st.LastSequence != 0 {
		t.Fatalf("cursor advanced past unevaluated work: %+v", st)
	}
	if st.LagEvents != 1 {
		t.Fatalf("blocked work is not reported as pending: %+v", st)
	}

	// The work is still there when the gate recovers — nothing recorded
	// "there was nothing to do".
	h.gate.set(Capacity{FreeBuilderLanes: 3}, nil)
	res := h.step(t)
	if !res.Executed || res.Plan.HighestSequence != 4 {
		t.Fatalf("work was lost while the gate was unknown: %+v", res)
	}
	if h.w.Stats().Unknown {
		t.Fatal("unknown flag survived a healthy pass")
	}
}

func TestStep_ImpossibleCapacitySnapshotIsUnknownNotZero(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: -1})
	_, err := h.w.Step(context.Background())
	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("negative capacity was accepted: %v", err)
	}
	if plans := h.exec.snapshot(); len(plans) != 0 {
		t.Fatalf("executed on an impossible snapshot: %+v", plans)
	}
}

// Acceptance: review pressure is drained before excess builders are launched.
func TestStep_ReviewPressureDrainsBeforeBuilders(t *testing.T) {
	for _, tc := range []struct {
		name         string
		capacity     Capacity
		wantDrain    int
		wantBuilders int
	}{
		{"pressure consumes the excess lanes", Capacity{FreeBuilderLanes: 3, PendingReviews: 2}, 2, 1},
		{"pressure exceeds free lanes", Capacity{FreeBuilderLanes: 2, PendingReviews: 3}, 3, 0},
		{"pressure equals free lanes", Capacity{FreeBuilderLanes: 2, PendingReviews: 2}, 2, 0},
		{"drained", Capacity{FreeBuilderLanes: 3, PendingReviews: 0}, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.capacity)
			res := h.step(t)
			if !res.Executed {
				t.Fatalf("no evaluation: %+v", res)
			}
			if res.Plan.DrainReviews != tc.wantDrain {
				t.Errorf("DrainReviews = %d, want %d", res.Plan.DrainReviews, tc.wantDrain)
			}
			if res.Plan.LaunchBuilders != tc.wantBuilders {
				t.Errorf("LaunchBuilders = %d, want %d", res.Plan.LaunchBuilders, tc.wantBuilders)
			}
		})
	}
}

// Acceptance (restart): a watcher resumed at a checkpoint replays nothing and
// still picks up everything newer.
func TestStep_RestartResumesFromCheckpoint(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 1})
	h.source.append(
		Event{Sequence: 1, Repo: "herdforge", TaskRef: "FAC-1", State: "verified"},
		Event{Sequence: 2, Repo: "herdforge", TaskRef: "FAC-2", State: "verified"},
	)
	h.startup(t)
	checkpoint := h.w.Stats().LastSequence
	if checkpoint != 2 {
		t.Fatalf("pre-restart cursor = %d, want 2", checkpoint)
	}

	restarted := &Watcher{
		Source: h.source, Gate: h.gate, Exec: h.exec, Lease: h.lease,
		Owner: "coordinator-a", Settle: 2 * time.Second,
		ReconcileInterval: time.Hour, Now: h.clock.Now,
	}
	restarted.Resume(checkpoint)

	res, err := restarted.Step(context.Background()) // startup sweep
	if err != nil {
		t.Fatalf("restart step: %v", err)
	}
	if len(res.Plan.Refs) != 0 {
		t.Fatalf("restart replayed applied work: %+v", res.Plan)
	}

	h.source.append(Event{Sequence: 3, Repo: "herdforge", TaskRef: "FAC-3", State: "verified"})
	restarted.Notify("herdforge", "FAC-3")
	h.clock.advance(2 * time.Second)
	res, err = restarted.Step(context.Background())
	if err != nil {
		t.Fatalf("post-restart step: %v", err)
	}
	if !res.Executed || len(res.Plan.Refs) != 1 || res.Plan.Refs[0] != "FAC-3" {
		t.Fatalf("restart missed post-checkpoint work: %+v", res.Plan)
	}
}

// A key that is still noisy is held back, and its events are NOT skipped by
// the cursor when a quieter key is evaluated ahead of it.
func TestStep_DeferredNoisyKeyIsNotSkippedByTheCursor(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 2})
	h.startup(t)

	h.source.append(Event{Sequence: 1, Repo: "herdforge", TaskRef: "FAC-noisy", State: "progress"})
	h.source.append(Event{Sequence: 2, Repo: "herdforge", TaskRef: "FAC-quiet", State: "verified"})
	h.w.Notify("herdforge", "FAC-quiet")
	h.clock.advance(2 * time.Second)
	h.w.Notify("herdforge", "FAC-noisy") // still chattering

	res := h.step(t)
	if !res.Executed {
		t.Fatal("quiet key was not evaluated")
	}
	if len(res.Plan.Refs) != 1 || res.Plan.Refs[0] != "FAC-quiet" {
		t.Fatalf("noisy key leaked into the settled plan: %+v", res.Plan)
	}
	if got := h.w.Stats().LastSequence; got != 0 {
		t.Fatalf("cursor jumped past the deferred event: %d", got)
	}

	h.clock.advance(2 * time.Second)
	res = h.step(t)
	if !res.Executed {
		t.Fatal("noisy key never settled")
	}
	found := false
	for _, ref := range res.Plan.Refs {
		if ref == "FAC-noisy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("deferred event was lost: %+v", res.Plan)
	}
	if h.w.Stats().LastSequence != 2 {
		t.Fatalf("cursor did not catch up: %+v", h.w.Stats())
	}
}

func TestStep_ExecutorFailureKeepsWorkPending(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 1})
	h.source.append(Event{Sequence: 1, Repo: "herdforge", TaskRef: "FAC-8", State: "verified"})
	h.exec.err = errors.New("dispatch refused")

	if _, err := h.w.Step(context.Background()); err == nil {
		t.Fatal("executor failure was swallowed")
	}
	if h.w.Stats().LastSequence != 0 {
		t.Fatalf("cursor advanced past a failed execution: %+v", h.w.Stats())
	}

	h.exec.mu.Lock()
	h.exec.err = nil
	h.exec.mu.Unlock()
	h.clock.advance(time.Hour)
	res := h.step(t)
	if !res.Executed || res.Plan.HighestSequence != 1 {
		t.Fatalf("failed plan was not retried: %+v", res)
	}
}

func TestStep_LeaseErrorIsNotSilentIdleness(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 1})
	h.lease.err = errors.New("lease store unavailable")
	res, err := h.w.Step(context.Background())
	if err == nil {
		t.Fatal("lease failure reported as a normal pass")
	}
	if res.Executed {
		t.Fatal("evaluated without proven ownership")
	}
}

func TestStep_MissingCollaboratorsFailClosed(t *testing.T) {
	w := &Watcher{Owner: "x"}
	if _, err := w.Step(context.Background()); err == nil {
		t.Fatal("a watcher with no source/gate/executor/lease ran anyway")
	}
}

// Acceptance (cancel): Run stops promptly on cancellation and hands ownership
// back so the next coordinator does not wait out a stale lease.
func TestRun_CancelReleasesOwnership(t *testing.T) {
	h := newHarness(t, Capacity{FreeBuilderLanes: 1})
	h.w.Settle = 10 * time.Millisecond
	h.w.ReconcileInterval = 20 * time.Millisecond
	h.w.Now = time.Now // real clock: this test is about the Run loop, not debounce
	h.exec.fired = make(chan struct{}, 1)
	h.source.append(Event{Sequence: 1, Repo: "herdforge", TaskRef: "FAC-3", State: "verified"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()

	select {
	case <-h.exec.fired:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run never evaluated")
	}
	if h.lease.heldBy() != "coordinator-a" {
		t.Fatalf("Run did not take ownership, holder=%q", h.lease.heldBy())
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
	if h.lease.heldBy() != "" {
		t.Fatalf("ownership not released on shutdown, holder=%q", h.lease.heldBy())
	}
	if h.w.Stats().Active {
		t.Fatal("stopped watcher still reports itself active")
	}
}

func TestNotify_WakesTheRunLoop(t *testing.T) {
	h := newHarness(t, Capacity{}) // no free lanes: only a real event can produce a plan
	h.w.Settle = 10 * time.Millisecond
	h.w.ReconcileInterval = time.Hour
	h.w.Now = time.Now
	h.exec.fired = make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()

	h.source.append(Event{Sequence: 1, Repo: "herdforge", TaskRef: "FAC-6", State: "verified"})
	h.w.Notify("herdforge", "FAC-6")

	select {
	case <-h.exec.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify did not drive an evaluation")
	}
	plans := h.exec.snapshot()
	last := plans[len(plans)-1]
	if last.HighestSequence != 1 {
		t.Fatalf("woken evaluation did not carry the event: %+v", last)
	}
	cancel()
	<-done
}
