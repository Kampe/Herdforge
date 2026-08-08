package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// mockLaneSource is a test LaneSource that returns a fixed set of lanes.
type mockLaneSource struct {
	lanes []LaneRef
	err   error
}

func (m mockLaneSource) ListLanes() ([]LaneRef, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.lanes, nil
}

func TestRefFromAgentName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"task-fac-225", "fac-225"},
		{"task-fac-018", "fac-18"},
		{"TASK-FAC-225", "fac-225"},
		{" task-fac-225 ", "fac-225"},
		{"task-cha-99", "cha-99"},
		{"worker", ""},
		{"task-225", ""},
		{"task-fac-", ""},
		{"", ""},
		{"task-fac-225-extra", ""},
	}
	for _, tc := range cases {
		got := RefFromAgentName(tc.name)
		if got != tc.want {
			t.Errorf("RefFromAgentName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReconcileBoard_TodoWithLiveLane_BoardLag(t *testing.T) {
	// A to-do card with no git branch but a live lane named task-fac-77.
	// Board-sync must detect BOARD_LAG: the lane proves work is in flight.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "new feature", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)
	syncer.Lanes = mockLaneSource{lanes: []LaneRef{
		{Name: "task-fac-77", Ref: "FAC-77"},
	}}

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 1 {
		t.Fatalf("expected 1 drift (BOARD_LAG from live lane), got %d (findings: %+v)", drift.Drift, drift.Findings)
	}
	f := drift.Findings[0]
	if f.Kind != "BOARD_LAG" {
		t.Fatalf("expected BOARD_LAG, got %s", f.Kind)
	}
	if f.Ref != "FAC-77" {
		t.Fatalf("expected ref FAC-77, got %s", f.Ref)
	}
}

func TestReconcileBoard_InProgressWithLiveLane_Honest(t *testing.T) {
	// An in-progress card with a live lane but no git branch: board is honest.
	// The lane proves work is in flight, so the card is not stale.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "active work", Status: "in-progress", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)
	syncer.Lanes = mockLaneSource{lanes: []LaneRef{
		{Name: "task-fac-77", Ref: "FAC-77"},
	}}

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift (lane proves active), got %d (findings: %+v)", drift.Drift, drift.Findings)
	}
}

func TestReconcileBoard_LaneDoesNotMatchDifferentRef(t *testing.T) {
	// A lane for FAC-77 must NOT make FAC-99 active. The non-digit boundary
	// in the regex must prevent fac-9 from matching fac-99.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-9", Title: "other work", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)
	syncer.Lanes = mockLaneSource{lanes: []LaneRef{
		{Name: "task-fac-99", Ref: "FAC-99"},
	}}

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift (lane FAC-99 must not match card FAC-9), got %d (findings: %+v)", drift.Drift, drift.Findings)
	}
}

func TestReconcileBoard_LaneSourceError_DegradesGracefully(t *testing.T) {
	// When the lane source errors, board-sync degrades to git-only — no
	// crash, no false positive. A to-do card with no branch and no lanes
	// must still be honest.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "backlog", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)
	syncer.Lanes = mockLaneSource{err: errors.New("herdr not available")}

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift on lane source error (degrade to git-only), got %d", drift.Drift)
	}
}

func TestReconcileBoard_NilLaneSource_Unchanged(t *testing.T) {
	// When no lane source is configured, behavior is identical to before:
	// only git facts determine activity.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "backlog", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)
	// Lanes is nil

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 0 {
		t.Fatalf("expected 0 drift with nil lane source, got %d", drift.Drift)
	}
}

func TestReconcileBoard_LaneAndBranchBothActive(t *testing.T) {
	// A to-do card with BOTH a git branch and a live lane: one BOARD_LAG
	// finding (not two). The lane is redundant but not harmful.
	repo := newBsyncRepo(t)
	wt := t.TempDir()
	repo.addWorktree(t, wt, "fac-77-feature", "feat: work on FAC-77")
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "new feature", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)
	syncer.Lanes = mockLaneSource{lanes: []LaneRef{
		{Name: "task-fac-77", Ref: "FAC-77"},
	}}

	drift, err := syncer.ReconcileBoard(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("ReconcileBoard: %v", err)
	}
	if drift.Drift != 1 {
		t.Fatalf("expected 1 drift (not doubled), got %d", drift.Drift)
	}
}

func TestFixBoardLag_AdvancesTodoCards(t *testing.T) {
	// A to-do card with a live lane: FixBoardLag advances it to in-progress.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "new feature", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)
	syncer.Lanes = mockLaneSource{lanes: []LaneRef{
		{Name: "task-fac-77", Ref: "FAC-77"},
	}}

	result, err := syncer.FixBoardLag(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("FixBoardLag: %v", err)
	}
	if len(result.Advanced) != 1 {
		t.Fatalf("expected 1 advanced card, got %d", len(result.Advanced))
	}
	if result.Advanced[0].Ref != "FAC-77" {
		t.Fatalf("expected FAC-77, got %s", result.Advanced[0].Ref)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected 0 errors, got %d", len(result.Errors))
	}
	// Verify the card was actually moved.
	task, err := mp.GetTask(context.Background(), "id-1")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != "in-progress" {
		t.Fatalf("expected card status in-progress, got %s", task.Status)
	}
}

func TestFixBoardLag_DoesNotTouchInProgress(t *testing.T) {
	// An in-progress card with a live lane: FixBoardLag must not touch it
	// (it's not BOARD_LAG).
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "active", Status: "in-progress", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)
	syncer.Lanes = mockLaneSource{lanes: []LaneRef{
		{Name: "task-fac-77", Ref: "FAC-77"},
	}}

	result, err := syncer.FixBoardLag(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("FixBoardLag: %v", err)
	}
	if len(result.Advanced) != 0 {
		t.Fatalf("expected 0 advanced (card already in-progress), got %d", len(result.Advanced))
	}
}

func TestFixBoardLag_NoLanes_NoChange(t *testing.T) {
	// A to-do card with no lane and no branch: FixBoardLag does nothing.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "backlog", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(mp)

	result, err := syncer.FixBoardLag(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("FixBoardLag: %v", err)
	}
	if len(result.Advanced) != 0 {
		t.Fatalf("expected 0 advanced, got %d", len(result.Advanced))
	}
}

func TestFixBoardLag_UpdateStatusError_Recorded(t *testing.T) {
	// When UpdateStatus fails, the error is recorded but FixBoardLag continues.
	repo := newBsyncRepo(t)
	mp := newBoard(&provider.Task{
		ID: "id-1", Ref: "FAC-77", Title: "new feature", Status: "to-do", ProjectID: "p1",
	})
	syncer := NewBoardSyncer(&errorUpdateProvider{mp: mp})
	syncer.Lanes = mockLaneSource{lanes: []LaneRef{
		{Name: "task-fac-77", Ref: "FAC-77"},
	}}

	result, err := syncer.FixBoardLag(context.Background(), "p1", repo.dir)
	if err != nil {
		t.Fatalf("FixBoardLag: %v", err)
	}
	if len(result.Advanced) != 0 {
		t.Fatalf("expected 0 advanced on UpdateStatus error, got %d", len(result.Advanced))
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 error, got %d", len(result.Errors))
	}
	if !strings.Contains(result.Errors[0].Error(), "advance") {
		t.Fatalf("error should mention 'advance': %v", result.Errors[0])
	}
}

// errorUpdateProvider wraps a MemoryProvider but makes UpdateStatus fail.
type errorUpdateProvider struct {
	mp *provider.MemoryProvider
}

func (e *errorUpdateProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	return e.mp.ListTasks(ctx, projectID, status)
}
func (e *errorUpdateProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	return e.mp.GetTask(ctx, id)
}
func (e *errorUpdateProvider) ClaimTask(ctx context.Context, taskID, role string) error {
	return e.mp.ClaimTask(ctx, taskID, role)
}
func (e *errorUpdateProvider) UpdateStatus(ctx context.Context, taskID, status string) error {
	return fmt.Errorf("provider unavailable")
}
func (e *errorUpdateProvider) AddComment(ctx context.Context, taskID, body string) error {
	return e.mp.AddComment(ctx, taskID, body)
}
