package daemon

import (
	"context"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func forgeEngine(t *testing.T, tasks ...*provider.Task) *Engine {
	t.Helper()
	mp := provider.NewMemoryProvider()
	for _, tk := range tasks {
		tk.ProjectID = "p1"
		mp.AddTask(tk)
	}
	cfg := &config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}
	return NewEngine(cfg, mp, nil, nil, nil, nil)
}

func TestForgeStep_ApprovesInReviewFirst(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent},
		&provider.Task{ID: "2", Ref: "FAC-2", Status: "in-review", Priority: provider.PriorityLow},
	)
	a, err := e.ForgeStep(context.Background(), LaneState{Busy: 0, Max: 3}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	// In-review wins even against an urgent to-do: finish before starting.
	if a.Kind != ActionApprove || a.Ref != "FAC-2" {
		t.Fatalf("want approve FAC-2, got %s %s", a.Kind, a.Ref)
	}
}

func TestForgeStep_ReviewsCompletedBuild(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "in-progress", Priority: provider.PriorityHigh},
		&provider.Task{ID: "2", Ref: "FAC-2", Status: "in-progress", Priority: provider.PriorityHigh},
	)
	// Only FAC-2's builder reported complete.
	a, err := e.ForgeStep(context.Background(), LaneState{Busy: 2, Max: 3}, map[string]bool{"FAC-2": true})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ActionReview || a.Ref != "FAC-2" {
		t.Fatalf("want review FAC-2, got %s %s", a.Kind, a.Ref)
	}
}

func TestForgeStep_DispatchesWhenLaneFree(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-9", Status: "to-do", Priority: provider.PriorityMedium},
		&provider.Task{ID: "2", Ref: "FAC-3", Status: "to-do", Priority: provider.PriorityUrgent},
	)
	a, err := e.ForgeStep(context.Background(), LaneState{Busy: 1, Max: 3}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	// Urgent FAC-3 dispatched before medium FAC-9.
	if a.Kind != ActionDispatch || a.Ref != "FAC-3" {
		t.Fatalf("want dispatch FAC-3, got %s %s", a.Kind, a.Ref)
	}
}

func TestForgeStep_NoDispatchWhenLanesFull(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent},
	)
	a, err := e.ForgeStep(context.Background(), LaneState{Busy: 3, Max: 3}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ActionIdle {
		t.Fatalf("lanes full: want idle, got %s %s", a.Kind, a.Ref)
	}
}

func TestForgeStep_IdleWhenBoardClear(t *testing.T) {
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "done", Priority: provider.PriorityHigh},
	)
	a, err := e.ForgeStep(context.Background(), LaneState{Busy: 0, Max: 3}, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != ActionIdle {
		t.Fatalf("clear board: want idle, got %s", a.Kind)
	}
}
