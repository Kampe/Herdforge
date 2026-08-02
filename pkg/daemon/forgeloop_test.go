package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// fakeDriver records the actions the loop drives and lets a test script the
// board's lane state and completion signals per tick.
type fakeDriver struct {
	lanes     LaneState
	completed map[string]bool
	verified  map[string]bool
	actions   []string
	onApprove func(ref string)
	onReview  func(ref string)
}

func (f *fakeDriver) LaneState(context.Context) LaneState { return f.lanes }
func (f *fakeDriver) Signals(context.Context) (map[string]bool, map[string]bool) {
	return f.completed, f.verified
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
	return nil
}
func (f *fakeDriver) Renudge(_ context.Context, t *provider.Task) error {
	f.actions = append(f.actions, "renudge:"+t.Ref)
	return nil
}
func (f *fakeDriver) Log(string) {}

func TestForgeLoop_DrivesActionsPerStep(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent},
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
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "in-progress", Priority: provider.PriorityHigh},
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
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "done", Priority: provider.PriorityHigh},
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
