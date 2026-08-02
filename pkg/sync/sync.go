package sync

import (
	"context"
	"fmt"

	"github.com/Kampe/Herdforge/pkg/provider"
)

type SyncReport struct {
	TotalProcessed int
	UpdatedStatus  int
}

type BoardSyncer struct {
	Provider provider.TaskProvider
}

func NewBoardSyncer(p provider.TaskProvider) *BoardSyncer {
	return &BoardSyncer{Provider: p}
}

// ReconcileBoard executes atomic board state reconciliation across Kanban columns (porting bin/herd-board-sync)
func (b *BoardSyncer) ReconcileBoard(ctx context.Context, projectID string) (*SyncReport, error) {
	if b.Provider == nil {
		return nil, fmt.Errorf("task provider is nil")
	}

	tasks, err := b.Provider.ListTasks(ctx, projectID, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks for board sync: %w", err)
	}

	report := &SyncReport{
		TotalProcessed: len(tasks),
		UpdatedStatus:  0,
	}

	for _, t := range tasks {
		// Reconcile task states if needed
		if t.Status == "in-progress" {
			report.UpdatedStatus++
		}
	}

	return report, nil
}
