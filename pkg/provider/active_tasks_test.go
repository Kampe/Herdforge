package provider

import (
	"context"
	"errors"
	"testing"
)

func seedTask(t *testing.T, mp *MemoryProvider, id, ref, status string) {
	t.Helper()
	if _, err := mp.CreateTask(context.Background(), &Task{
		ID: id, Ref: ref, Status: status, ProjectID: "proj", Title: ref,
	}); err != nil {
		t.Fatalf("seed %s: %v", ref, err)
	}
}

// TestListActiveTasksExcludesTerminalColumns is the contract that made the fix
// worthwhile: terminal columns are never read, so their size cannot cost the
// caller anything.
func TestListActiveTasksExcludesTerminalColumns(t *testing.T) {
	mp := NewMemoryProvider()
	seedTask(t, mp, "1", "FAC-1", StatusToDo)
	seedTask(t, mp, "2", "FAC-2", StatusInProgress)
	seedTask(t, mp, "3", "FAC-3", StatusInReview)
	seedTask(t, mp, "4", "FAC-4", StatusPlanned)
	seedTask(t, mp, "5", "FAC-5", StatusDone)
	seedTask(t, mp, "6", "FAC-6", StatusArchived)

	got, err := ListActiveTasks(context.Background(), mp, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 active tasks, got %d", len(got))
	}
	for _, task := range got {
		if !IsActiveStatus(task.Status) {
			t.Fatalf("terminal card leaked into the active set: %s (%s)", task.Ref, task.Status)
		}
	}
	// Deterministic order regardless of which column answered first.
	for i := 1; i < len(got); i++ {
		if CompareRefs(got[i-1].Ref, got[i].Ref) > 0 {
			t.Fatalf("results not sorted by ref: %s before %s", got[i-1].Ref, got[i].Ref)
		}
	}
}

// TestListActiveTasksFailsClosed proves a column error is never reported as a
// smaller board. A partial active set would let a dependency fence pass on
// edges it never saw.
func TestListActiveTasksFailsClosed(t *testing.T) {
	boom := errors.New("column unavailable")
	tp := &failingColumnProvider{MemoryProvider: NewMemoryProvider(), failOn: StatusInReview, err: boom}
	seedTask(t, tp.MemoryProvider, "1", "FAC-1", StatusToDo)

	got, err := ListActiveTasks(context.Background(), tp, "proj")
	if !errors.Is(err, boom) {
		t.Fatalf("want the column error, got %v", err)
	}
	if got != nil {
		t.Fatalf("a failed fan-out must not return a partial set, got %d tasks", len(got))
	}
}

// TestListActiveTasksDeduplicatesMovedCard covers a card observed in two
// columns because it moved mid-fan-out.
func TestListActiveTasksDeduplicatesMovedCard(t *testing.T) {
	tp := &duplicatingProvider{MemoryProvider: NewMemoryProvider()}
	seedTask(t, tp.MemoryProvider, "1", "FAC-1", StatusToDo)

	got, err := ListActiveTasks(context.Background(), tp, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a card seen in two columns must appear once, got %d", len(got))
	}
}

type failingColumnProvider struct {
	*MemoryProvider
	failOn string
	err    error
}

func (p *failingColumnProvider) ListTasks(ctx context.Context, projectID, status string) ([]*Task, error) {
	if status == p.failOn {
		return nil, p.err
	}
	return p.MemoryProvider.ListTasks(ctx, projectID, status)
}

type duplicatingProvider struct {
	*MemoryProvider
}

// ListTasks returns the same card for every active column, as if it moved
// while the fan-out was in flight.
func (p *duplicatingProvider) ListTasks(ctx context.Context, projectID, _ string) ([]*Task, error) {
	return p.MemoryProvider.ListTasks(ctx, projectID, StatusToDo)
}
