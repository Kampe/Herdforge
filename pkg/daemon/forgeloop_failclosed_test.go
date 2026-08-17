package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-138: the coordinator must never mistake an unreadable fleet, a failed
// transition, or an orphaned in-progress card for a drained board.

func fastLoop(maxTicks int) ForgeLoopOptions {
	return ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: maxTicks}
}

func todoTask(ref, id string) *provider.Task {
	return &provider.Task{ID: id, Ref: ref, Status: "to-do", Priority: provider.PriorityUrgent,
		Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"" + ref + "\",\"task_id\":\"" + id + "\",\"edges\":[]}\n```\n"}
}

// An unreadable lane state is UNKNOWN capacity. Dispatching here is exactly
// the "Herdr failure == zero busy lanes" defect: a full fleet looks idle.
func TestForgeLoop_UnknownLaneStateBlocksDispatch(t *testing.T) {
	e := forgeEngine(t, todoTask("FAC-1", "1"))
	d := &fakeDriver{laneErr: errors.New("herdr agent list: connection refused"), lanes: LaneState{Busy: 0, Max: 3}}

	err := e.ForgeLoop(context.Background(), d, fastLoop(1))
	if err == nil {
		t.Fatal("unreadable lane state exited 0")
	}
	if !strings.Contains(err.Error(), "fleet_state_unknown") {
		t.Fatalf("want a fleet_state_unknown transition, got %v", err)
	}
	if len(d.actions) != 0 {
		t.Fatalf("acted on an unreadable fleet: %v", d.actions)
	}
}

// Same for completion signals: an empty signal set from a failed read would
// silently strand every completed build.
func TestForgeLoop_UnknownSignalsBlocksActions(t *testing.T) {
	e := forgeEngine(t, todoTask("FAC-1", "1"))
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 3}, signalErr: errors.New("herdr agent list: timeout")}

	err := e.ForgeLoop(context.Background(), d, fastLoop(1))
	if err == nil {
		t.Fatal("unreadable completion signals exited 0")
	}
	if !strings.Contains(err.Error(), "fleet_state_unknown") {
		t.Fatalf("want a fleet_state_unknown transition, got %v", err)
	}
	if len(d.actions) != 0 {
		t.Fatalf("acted without completion signals: %v", d.actions)
	}
}

// flakyLaneDriver fails its first `fails` lane reads, then reports normally.
type flakyLaneDriver struct {
	fakeDriver
	fails int
	calls int
}

func (f *flakyLaneDriver) LaneState(ctx context.Context) (LaneState, error) {
	f.calls++
	if f.calls <= f.fails {
		return LaneState{}, errors.New("herdr agent list: connection refused")
	}
	return f.fakeDriver.LaneState(ctx)
}

// A recovered read must not poison the exit status: tick 1 fails, tick 2 reads
// fine and dispatches, and the run ends clean.
func TestForgeLoop_RecoveredFleetReadClearsFailure(t *testing.T) {
	e := forgeEngine(t, todoTask("FAC-1", "1"))
	d := &flakyLaneDriver{
		fakeDriver: fakeDriver{lanes: LaneState{Busy: 0, Max: 3}, completed: map[string]bool{}, verified: map[string]bool{}},
		fails:      1,
	}

	if err := e.ForgeLoop(context.Background(), d, fastLoop(2)); err != nil {
		t.Fatalf("recovered fleet read must exit 0: %v", err)
	}
	if len(d.actions) != 1 || d.actions[0] != "dispatch:FAC-1" {
		t.Fatalf("loop did not resume dispatching after recovery: %v", d.actions)
	}
}

// An action that fails is a FAILED transition, not a log line: it has to reach
// the exit status.
func TestForgeLoop_ActionFailureAffectsExitStatus(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "2", Ref: "FAC-2", Status: "in-review", Priority: provider.PriorityHigh,
			Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-2\",\"task_id\":\"2\",\"edges\":[]}\n```\n"},
	)
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{},
		approveErr: func(string) error { return errors.New("no commit on origin/main names it") }}

	err := e.ForgeLoop(context.Background(), d, fastLoop(1))
	if err == nil {
		t.Fatal("a failed approve exited 0")
	}
	if !strings.Contains(err.Error(), "approve FAC-2 failed") {
		t.Fatalf("exit error must name the failed transition, got %v", err)
	}
}

// ...but a transition that later succeeds is resolved, not held against the
// run. Approve fails on the first attempt and succeeds on the second.
func TestForgeLoop_SucceededRetryClearsFailure(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "2", Ref: "FAC-2", Status: "in-review", Priority: provider.PriorityHigh,
			Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-2\",\"task_id\":\"2\",\"edges\":[]}\n```\n"},
	)
	attempts := 0
	d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{},
		approveErr: func(string) error {
			attempts++
			if attempts == 1 {
				return errors.New("transient board write")
			}
			return nil
		}}

	if err := e.ForgeLoop(context.Background(), d, fastLoop(2)); err != nil {
		t.Fatalf("a retried-and-succeeded approve must exit 0: %v actions=%v logs=%v", err, d.actions, d.logged)
	}
	if attempts != 2 {
		t.Fatalf("want 2 approve attempts, got %d", attempts)
	}
}

// FAC-354: a receiptless legacy approval is a durable blocked state, not a
// reason to invoke the same failing approval every forge tick. A fresh loop
// (simulated restart) must retain the tombstone, while changed board evidence
// must make the approval eligible again.
func TestForgeLoop_LegacyApproveRefusalSuppressesAcrossRestartUntilStateChanges(t *testing.T) {
	task := &provider.Task{ID: "354", Ref: "FAC-354", Status: "in-review", Priority: provider.PriorityHigh,
		Description: "legacy candidate state A"}
	e := forgeEngine(t, task)
	path := filepath.Join(t.TempDir(), "approve-suppressions.json")
	attempts := 0
	newDriver := func() *fakeDriver {
		return &fakeDriver{lanes: LaneState{Busy: 0, Max: 1}, completed: map[string]bool{}, verified: map[string]bool{},
			approveErr: func(string) error {
				attempts++
				return hsync.ErrNoEvidence
			}}
	}
	first := newDriver()
	firstErr := e.ForgeLoop(context.Background(), first, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 3, ApproveSuppressionPath: path})
	if firstErr == nil || !strings.Contains(firstErr.Error(), "approve FAC-354 failed") {
		t.Fatalf("first receiptless refusal must remain blocked, got %v", firstErr)
	}
	if attempts != 1 {
		t.Fatalf("repeated legacy refusal was not suppressed in one run: attempts=%d logs=%v", attempts, first.logged)
	}
	if !strings.Contains(strings.Join(first.logged, "\n"), "BLOCKED-legacy") {
		t.Fatalf("suppression was not visible in loop output: %v", first.logged)
	}

	second := newDriver()
	secondErr := e.ForgeLoop(context.Background(), second, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1, ApproveSuppressionPath: path})
	if secondErr == nil {
		t.Fatal("persisted receiptless refusal must remain blocked after restart")
	}
	if attempts != 1 {
		t.Fatalf("persisted legacy refusal was retried after restart: attempts=%d", attempts)
	}
	retry := newDriver()
	_ = e.ForgeLoop(context.Background(), retry, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1, ApproveSuppressionPath: path, ApproveRetryRefs: map[string]bool{"FAC-354": true}})
	if attempts != 2 {
		t.Fatalf("explicit operator retry did not bypass suppression: attempts=%d", attempts)
	}

	task.Description = "legacy candidate state B"
	third := newDriver()
	_ = e.ForgeLoop(context.Background(), third, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1, ApproveSuppressionPath: path})
	if attempts != 3 {
		t.Fatalf("changed candidate/receipt state did not re-arm approval: attempts=%d", attempts)
	}
}

// The orphan: a card sits in-progress with no builder behind it, so it emits
// no completion signal at all. Idle + no busy lane looked exactly like a
// drained board and the loop exited, abandoning the card.
func TestForgeLoop_OrphanInProgressBlocksCleanExit(t *testing.T) {
	// A standing coordinator (StopEmpty=false) must report the same reason
	// rather than sitting silently idle on top of the orphan.
	for _, stopEmpty := range []bool{true, false} {
		e := forgeEngine(t,
			&provider.Task{ID: "9", Ref: "FAC-9", Status: "in-progress", Priority: provider.PriorityHigh,
				Description: "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"FAC-9\",\"task_id\":\"9\",\"edges\":[]}\n```\n"},
		)
		// No completion signal for FAC-9 and no busy lane — the orphan shape.
		d := &fakeDriver{lanes: LaneState{Busy: 0, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{}}

		err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 2, StopEmpty: stopEmpty})
		if err == nil {
			t.Fatalf("stop-empty=%v: loop exited clean while FAC-9 was orphaned in-progress", stopEmpty)
		}
		if !strings.Contains(err.Error(), "orphan_in_progress") || !strings.Contains(err.Error(), "FAC-9") {
			t.Fatalf("stop-empty=%v: want a blocked-with-reason naming FAC-9, got %v", stopEmpty, err)
		}
		if !strings.Contains(strings.Join(d.logged, "\n"), "orphan_in_progress") {
			t.Fatalf("stop-empty=%v: orphan block was not observable in the log: %v", stopEmpty, d.logged)
		}
	}
}
