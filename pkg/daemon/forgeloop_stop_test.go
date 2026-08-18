package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/budget"
)

func TestForgeLoop_BudgetStopsBeforeNextTick(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}}
	bm := budget.NewBudgetManager(1)
	bm.TotalCostUSD = 1
	err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, Budget: bm, MaxTicks: 2})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("want budget stop, got %v", err)
	}
	if len(d.actions) != 0 {
		t.Fatalf("budget stop must not start a transition: %v", d.actions)
	}
	if !strings.Contains(strings.Join(d.logged, "\n"), "budget exhausted") {
		t.Fatalf("budget stop was not reported: %v", d.logged)
	}
}

func TestForgeLoop_RepeatedBlockerStopsAtThreshold(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}}
	calls := 0
	err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 4, BlockerThreshold: 3, Blockers: func(context.Context) (map[string]string, error) {
		calls++
		return map[string]string{"FAC-414": "open_blocker"}, nil
	}})
	if !errors.Is(err, ErrRepeatedBlocker) {
		t.Fatalf("want repeated-blocker stop, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("blocker callback calls=%d, want 3", calls)
	}
	if len(d.actions) != 0 {
		t.Fatalf("repeated blocker must stop before a transition: %v", d.actions)
	}
	if !strings.Contains(err.Error(), "FAC-414") || !strings.Contains(err.Error(), "open_blocker") {
		t.Fatalf("stop must report ref and code: %v", err)
	}
}

func TestForgeLoop_RepeatedBlockerResetsWhenCodeChanges(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}}
	codes := []string{"open_blocker", "drift", "open_blocker", "open_blocker", "open_blocker"}
	calls := 0
	err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 5, BlockerThreshold: 3, Blockers: func(context.Context) (map[string]string, error) {
		code := codes[calls]
		calls++
		return map[string]string{"FAC-414": code}, nil
	}})
	if !errors.Is(err, ErrRepeatedBlocker) || !strings.Contains(err.Error(), "open_blocker") {
		t.Fatalf("want stop on third unchanged open_blocker, got %v", err)
	}
	if calls != 5 {
		t.Fatalf("blocker callback calls=%d, want 5", calls)
	}
}
