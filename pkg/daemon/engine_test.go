package daemon

import (
	"context"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestEngine_SelectNextTask_DeterministicSort(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "1", Ref: "FAC-10", Title: "Medium 1", Priority: provider.PriorityMedium, Status: "to-do", ProjectID: "proj-1", Labels: []string{"herd-smith"}})
	mp.AddTask(&provider.Task{ID: "2", Ref: "FAC-2", Title: "Urgent 1", Priority: provider.PriorityUrgent, Status: "to-do", ProjectID: "proj-1", Labels: []string{"herd-smith"}})
	mp.AddTask(&provider.Task{ID: "3", Ref: "FAC-1", Title: "High 1", Priority: provider.PriorityHigh, Status: "to-do", ProjectID: "proj-1", Labels: []string{"herd-smith"}})

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{ProjectID: "proj-1"},
	}
	engine := NewEngine(cfg, mp, nil, nil, nil, nil)

	task, err := engine.SelectNextTask(context.Background(), "herd-smith")
	if err != nil {
		t.Fatalf("expected task selection, got err: %v", err)
	}

	if task == nil {
		t.Fatalf("expected non-nil task selected")
	}

	if task.Ref != "FAC-2" {
		t.Errorf("expected urgent task FAC-2 selected first, got %s", task.Ref)
	}
}
