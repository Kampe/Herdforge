package sync

import (
	"context"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func TestBoardSyncer_ReconcileBoard(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "1", Ref: "FAC-16", Title: "Sync Task", Status: "in-progress", Priority: provider.PriorityHigh, ProjectID: "proj-1"})

	syncer := NewBoardSyncer(mp)

	report, err := syncer.ReconcileBoard(context.Background(), "proj-1")
	if err != nil || report == nil {
		t.Fatalf("expected clean board sync execution, got err: %v", err)
	}

	if report.TotalProcessed != 1 {
		t.Errorf("expected 1 task processed, got %d", report.TotalProcessed)
	}
}
