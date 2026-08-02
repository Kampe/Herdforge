package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

type errorProvider struct {
	listTasksFn func(ctx context.Context, projectID string, status string) ([]*provider.Task, error)
}

func (e *errorProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*provider.Task, error) {
	return e.listTasksFn(ctx, projectID, status)
}

func (e *errorProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	return nil, errors.New("not implemented")
}

func (e *errorProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	return errors.New("not implemented")
}

func (e *errorProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	return errors.New("not implemented")
}

func (e *errorProvider) AddComment(ctx context.Context, taskID string, body string) error {
	return errors.New("not implemented")
}

func TestReconcileBoard_ListTasksError(t *testing.T) {
	ep := &errorProvider{
		listTasksFn: func(ctx context.Context, projectID string, status string) ([]*provider.Task, error) {
			return nil, errors.New("list tasks failed")
		},
	}
	syncer := NewBoardSyncer(ep)

	_, err := syncer.ReconcileBoard(context.Background(), "proj-1")
	if err == nil {
		t.Fatal("expected error when ListTasks fails")
	}
}

func TestReconcileBoard_EmptyTasks(t *testing.T) {
	mp := provider.NewMemoryProvider()
	syncer := NewBoardSyncer(mp)

	report, err := syncer.ReconcileBoard(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if report.TotalProcessed != 0 {
		t.Errorf("expected 0 processed tasks, got %d", report.TotalProcessed)
	}
	if report.UpdatedStatus != 0 {
		t.Errorf("expected 0 status updates, got %d", report.UpdatedStatus)
	}
}

func TestReconcileBoard_NoInProgress(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "1", Ref: "FAC-17", Title: "Done Task", Status: "done", Priority: provider.PriorityMedium, ProjectID: "proj-1"})
	mp.AddTask(&provider.Task{ID: "2", Ref: "FAC-18", Title: "Todo Task", Status: "to-do", Priority: provider.PriorityLow, ProjectID: "proj-1"})

	syncer := NewBoardSyncer(mp)

	report, err := syncer.ReconcileBoard(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if report.TotalProcessed != 2 {
		t.Errorf("expected 2 processed tasks, got %d", report.TotalProcessed)
	}
	if report.UpdatedStatus != 0 {
		t.Errorf("expected 0 status updates (no in-progress tasks), got %d", report.UpdatedStatus)
	}
}

func TestReconcileBoard_MultipleInProgress(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "1", Ref: "FAC-19", Title: "In Progress 1", Status: "in-progress", Priority: provider.PriorityHigh, ProjectID: "proj-1"})
	mp.AddTask(&provider.Task{ID: "2", Ref: "FAC-20", Title: "In Progress 2", Status: "in-progress", Priority: provider.PriorityMedium, ProjectID: "proj-1"})
	mp.AddTask(&provider.Task{ID: "3", Ref: "FAC-21", Title: "Todo", Status: "to-do", Priority: provider.PriorityLow, ProjectID: "proj-1"})

	syncer := NewBoardSyncer(mp)

	report, err := syncer.ReconcileBoard(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if report.TotalProcessed != 3 {
		t.Errorf("expected 3 processed tasks, got %d", report.TotalProcessed)
	}
	if report.UpdatedStatus != 2 {
		t.Errorf("expected 2 status updates (in-progress tasks), got %d", report.UpdatedStatus)
	}
}

func TestReconcileBoard_DifferentProject(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "1", Ref: "FAC-22", Title: "Other Project", Status: "in-progress", Priority: provider.PriorityHigh, ProjectID: "other-proj"})

	syncer := NewBoardSyncer(mp)

	report, err := syncer.ReconcileBoard(context.Background(), "proj-1")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if report.TotalProcessed != 0 {
		t.Errorf("expected 0 tasks from different project, got %d", report.TotalProcessed)
	}
}

func TestNewBoardSyncer(t *testing.T) {
	mp := provider.NewMemoryProvider()
	s := NewBoardSyncer(mp)
	if s.Provider != mp {
		t.Error("expected Provider to be set")
	}
}
