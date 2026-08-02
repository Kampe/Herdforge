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

	_, err := syncer.ReconcileBoard(context.Background(), "proj-1", t.TempDir())
	if err == nil {
		t.Fatal("expected error when ListTasks fails")
	}
}

// memoryDir returns a non-git temp dir: every git fact degrades to the
// empty/false value (a hermetic substitute for a silent offline git).
func memoryDir(t *testing.T) string {
	return t.TempDir()
}

func TestReconcileBoard_EmptyTasks(t *testing.T) {
	mp := provider.NewMemoryProvider()
	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "proj-1", memoryDir(t))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if drift.Drift != 0 {
		t.Errorf("expected 0 drift, got %d", drift.Drift)
	}
	if len(drift.Findings) != 0 {
		t.Errorf("expected no findings, got %d", len(drift.Findings))
	}
}

func TestReconcileBoard_DifferentProject(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "1", Ref: "FAC-22", Title: "Other Project", Status: "in-progress", Priority: provider.PriorityHigh, ProjectID: "other-proj"})

	syncer := NewBoardSyncer(mp)

	drift, err := syncer.ReconcileBoard(context.Background(), "proj-1", memoryDir(t))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if drift.Drift != 0 || len(drift.Findings) != 0 {
		t.Errorf("expected empty board for other project, got drift=%d findings=%d", drift.Drift, len(drift.Findings))
	}
}

func TestReconcileBoard_FilterStandingEpic(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "1", Ref: "FAC-77", Title: "standing epic to-do list", Status: "in-progress", ProjectID: "proj-1"})

	syncer := NewBoardSyncer(mp)
	drift, err := syncer.ReconcileBoard(context.Background(), "proj-1", memoryDir(t))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if drift.Drift != 0 || len(drift.Findings) != 0 {
		t.Errorf("standing epic must be skipped, got drift=%d findings=%d", drift.Drift, len(drift.Findings))
	}
}

func TestNewBoardSyncer(t *testing.T) {
	mp := provider.NewMemoryProvider()
	s := NewBoardSyncer(mp)
	if s.Provider != mp {
		t.Error("expected Provider to be set")
	}
}