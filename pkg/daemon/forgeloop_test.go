package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// fakeDriver records the actions the loop drives and lets a test script the
// board's lane state and completion signals per tick.
type fakeDriver struct {
	lanes        LaneState
	laneErr      error
	signalErr    error
	rejectionErr error
	completed    map[string]bool
	verified     map[string]bool
	rejections   map[string]Rejection
	delivered    []Rejection
	actions      []string
	logged       []string
	onApprove    func(ref string)
	onReview     func(ref string)
	approveErr   func(ref string) error
	rejectErr    func(ref string) error
}

type observingDriver struct {
	*fakeDriver
	observations int
}

func (d *observingDriver) ObserveReconciliation(context.Context) error {
	d.observations++
	return nil
}


func TestForgeLoop_ReconciliationFailureStopsBeforeDriverActions(t *testing.T) {
	e := forgeEngine(t)
	e.ControlReconciler = &control.CoordinatorLoop{}
	d := &fakeDriver{lanes: LaneState{Max: 1}}
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{MaxTicks: 1}); err == nil {
		t.Fatal("reconciliation failure was ignored")
	}
	if len(d.actions) != 0 {
		t.Fatalf("driver actions ran after reconciliation failure: %v", d.actions)
	}
}

func TestForgeLoop_MissingProductionCompositionFailsClosed(t *testing.T) {
	e := forgeEngine(t)
	e.ControlRequired = true
	d := &fakeDriver{lanes: LaneState{Max: 1}}
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{MaxTicks: 1}); err == nil {
		t.Fatal("missing durable composition was accepted")
	}
	if len(d.actions) != 0 {
		t.Fatalf("driver actions ran without durable composition: %v", d.actions)
	}
}

func (f *fakeDriver) LaneState(context.Context) (LaneState, error) {
	if f.laneErr != nil {
		return LaneState{}, f.laneErr
	}
	return f.lanes, nil
}
func (f *fakeDriver) Signals(context.Context) (map[string]bool, map[string]bool, error) {
	if f.signalErr != nil {
		return nil, nil, f.signalErr
	}
	return f.completed, f.verified, nil
}
func (f *fakeDriver) Dispatch(_ context.Context, t *provider.Task) error {
	f.actions = append(f.actions, "dispatch:"+t.Ref)
	f.lanes.Busy++
	return nil
}
func (f *fakeDriver) Review(_ context.Context, t *provider.Task) error {
	f.actions = append(f.actions, "review:"+t.Ref)
	if f.onReview != nil {
		f.onReview(t.Ref)
	}
	return nil
}
func (f *fakeDriver) Approve(_ context.Context, t *provider.Task) error {
	f.actions = append(f.actions, "approve:"+t.Ref)
	if f.onApprove != nil {
		f.onApprove(t.Ref)
	}
	if f.approveErr != nil {
		return f.approveErr(t.Ref)
	}
	return nil
}
func (f *fakeDriver) Renudge(_ context.Context, t *provider.Task) error {
	f.actions = append(f.actions, "renudge:"+t.Ref)
	return nil
}
func (f *fakeDriver) Rejections(context.Context) (map[string]Rejection, error) {
	if f.rejectionErr != nil {
		return nil, f.rejectionErr
	}
	return f.rejections, nil
}
func (f *fakeDriver) Reject(_ context.Context, t *provider.Task, r Rejection) error {
	f.actions = append(f.actions, "reject:"+t.Ref)
	if f.rejectErr != nil {
		if err := f.rejectErr(t.Ref); err != nil {
			return err
		}
	}
	f.delivered = append(f.delivered, r)
	return nil
}
func (f *fakeDriver) Log(msg string) { f.logged = append(f.logged, msg) }

func TestForgeLoop_DrivesActionsPerStep(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent, Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-1\",\"task_id\":\"1\",\"edges\":[]}\n```\n"},
	)
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{}}
	// One tick: nothing in-review/completed, a free lane and one to-do → dispatch.
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1}); err != nil {
		t.Fatal(err)
	}
	if len(d.actions) != 1 || d.actions[0] != "dispatch:FAC-1" {
		t.Fatalf("want [dispatch:FAC-1], got %v", d.actions)
	}
}

func TestForgeLoop_RenudgesUnverified(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "in-progress", Priority: provider.PriorityHigh, Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-1\",\"task_id\":\"1\",\"edges\":[]}\n```\n"},
	)
	// Builder reported done but NOT verified → the loop must re-nudge, never review.
	d := &fakeDriver{lanes: LaneState{Busy: 1, Max: 2}, completed: map[string]bool{"FAC-1": true}, verified: map[string]bool{}}
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1}); err != nil {
		t.Fatal(err)
	}
	if len(d.actions) != 1 || d.actions[0] != "renudge:FAC-1" {
		t.Fatalf("want [renudge:FAC-1], got %v", d.actions)
	}
}

func TestForgeLoop_StopsWhenBoardClear(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "done", Priority: provider.PriorityHigh, Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-1\",\"task_id\":\"1\",\"edges\":[]}\n```\n"},
	)
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{}}
	// StopEmpty: clear board + no busy lane → loop returns nil promptly.
	done := make(chan error, 1)
	go func() {
		done <- e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, StopEmpty: true})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("clean stop expected, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on clear board")
	}
}

// FAC-135: one card reaches Done exactly once with matching action receipts.
// The driver mutates board state so the loop can walk claim→dispatch→verify→
// review→approve without vacuous fixed-signal tables.
func TestForgeLoop_HappyPathReachesDoneOnce(t *testing.T) {
	mp := provider.NewMemoryProvider()
	task := &provider.Task{
		ID: "1", Ref: "FAC-9001", Status: "to-do", Priority: provider.PriorityUrgent,
		ProjectID: "p1",
		Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-9001\",\"task_id\":\"1\",\"edges\":[]}\n```\n",
	}
	mp.AddTask(task)
	cfg := &config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}
	e := NewEngine(cfg, mp, nil, nil, nil, nil)

	d := &fakeDriver{
		lanes:     LaneState{Busy: 0, Max: 1},
		completed: map[string]bool{},
		verified:  map[string]bool{},
	}
	// After review: card in-review, signals clear. After approve: done, lane free.
	d.onReview = func(ref string) {
		_ = mp.UpdateStatus(context.Background(), "1", "in-review")
		d.completed = map[string]bool{}
		d.verified = map[string]bool{}
		d.lanes.Busy = 0
	}
	d.onApprove = func(ref string) {
		_ = mp.UpdateStatus(context.Background(), "1", "done")
		d.lanes.Busy = 0
	}
	hd := &happyDriver{fakeDriver: d, mp: mp}
	if err := e.ForgeLoop(context.Background(), hd, ForgeLoopOptions{
		Interval:  time.Millisecond,
		MaxTicks:  8,
		StopEmpty: true,
	}); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	// Exact receipt: dispatch once, review once, approve once — no renudge.
	var counts = map[string]int{}
	for _, a := range hd.actions {
		counts[a]++
	}
	if counts["dispatch:FAC-9001"] != 1 || counts["review:FAC-9001"] != 1 || counts["approve:FAC-9001"] != 1 {
		t.Fatalf("receipts = %v want one each of dispatch/review/approve", hd.actions)
	}
	if counts["renudge:FAC-9001"] != 0 {
		t.Fatalf("unexpected renudge: %v", hd.actions)
	}
	got, err := mp.GetTask(context.Background(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Fatalf("board status = %q want done", got.Status)
	}
}

// happyDriver advances board + lane signals so one groomed card drains to Done.
type happyDriver struct {
	*fakeDriver
	mp *provider.MemoryProvider
}

func (h *happyDriver) Dispatch(ctx context.Context, t *provider.Task) error {
	if err := h.fakeDriver.Dispatch(ctx, t); err != nil {
		return err
	}
	_ = h.mp.UpdateStatus(ctx, t.ID, "in-progress")
	// Next tick: builder finished and verified.
	h.completed = map[string]bool{t.Ref: true}
	h.verified = map[string]bool{t.Ref: true}
	return nil
}

func TestForgeLoopRunsObserveReconciliationAtStartupAndPeriodically(t *testing.T) {
	e := forgeEngine(t)
	// Control reconciler is required on production engines; a nil one would
	// fail closed before observe ticks. Tests that exercise the observe path
	// only need a no-op reconciler when ControlRequired is set — leave default.
	d := &observingDriver{fakeDriver: &fakeDriver{lanes: LaneState{Max: 1}, completed: map[string]bool{}, verified: map[string]bool{}}}
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1}); err != nil {
		t.Fatal(err)
	}
	// startup observe + one periodic observe per tick
	if d.observations != 2 {
		t.Fatalf("observe calls=%d, want startup+periodic", d.observations)
	}
}

// FAC-222: the feedback census is wired into the forge loop so a lane that
// goes quiet is REPORTED rather than discovered by polling. A nil Feedback
// preserves the prior behavior (polling-only). A Feedback error is logged,
// never fatal — a census failure must not stop the forge loop.
func TestForgeLoopRunsFeedbackCensusPeriodically(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}, completed: map[string]bool{}, verified: map[string]bool{}}
	var feedbackCalls int
	feedback := func(context.Context) error {
		feedbackCalls++
		return nil
	}
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{
		Interval:         time.Millisecond,
		MaxTicks:         3,
		Feedback:         feedback,
		FeedbackInterval: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if feedbackCalls != 3 {
		t.Fatalf("feedback calls=%d, want one per tick (3 ticks)", feedbackCalls)
	}
}

func TestForgeLoopFeedbackErrorIsLoggedNotFatal(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}, completed: map[string]bool{}, verified: map[string]bool{}}
	feedback := func(context.Context) error {
		return fmt.Errorf("census unavailable")
	}
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{
		Interval:         time.Millisecond,
		MaxTicks:         1,
		Feedback:         feedback,
		FeedbackInterval: 1,
	}); err != nil {
		t.Fatalf("feedback error must not stop the loop: %v", err)
	}
	found := false
	for _, msg := range d.logged {
		if strings.Contains(msg, "feedback census failed") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("feedback error must be logged, got: %v", d.logged)
	}
}

func TestForgeLoopNilFeedbackPreservesPriorBehavior(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}, completed: map[string]bool{}, verified: map[string]bool{}}
	// Nil Feedback must not panic and must not change the loop's behavior.
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{
		Interval: time.Millisecond,
		MaxTicks: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestForgeLoopFeedbackIntervalRespected(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}, completed: map[string]bool{}, verified: map[string]bool{}}
	var feedbackCalls int
	feedback := func(context.Context) error {
		feedbackCalls++
		return nil
	}
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{
		Interval:         time.Millisecond,
		MaxTicks:         6,
		Feedback:         feedback,
		FeedbackInterval: 2,
	}); err != nil {
		t.Fatal(err)
	}
	// tick 0, 2, 4 → 3 calls (tick 0 is the first tick, feedback runs at startup)
	if feedbackCalls != 3 {
		t.Fatalf("feedback calls=%d, want 3 (every 2 ticks over 6)", feedbackCalls)
	}
}
