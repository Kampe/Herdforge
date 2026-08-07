package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/boardfreeze"
)

// isolateFreeze gives each test its own durable state dir so freezing the
// board in one test can never leak into another (mirrors pkg/posture's
// isolate helper).
func isolateFreeze(t *testing.T) {
	t.Helper()
	t.Setenv("HERD_STATE_DIR", t.TempDir())
}

func freezeOn(t *testing.T, actor, reason string) boardfreeze.State {
	t.Helper()
	st, err := boardfreeze.SetState(true, actor, reason, "", nil, time.Now())
	if err != nil {
		t.Fatalf("freeze on: %v", err)
	}
	return st
}

func newFrozenBoundClient(t *testing.T) (*BoundClient, *MemoryProvider) {
	t.Helper()
	mp := NewMemoryProvider()
	mp.AddTask(&Task{ID: "t1", Ref: "FAC-1", Status: "open"})
	bc := NewBoundClient(mp, DefaultDeadlines())
	return bc, mp
}

func TestBoundClient_RefusesMutationsWhileFrozen(t *testing.T) {
	isolateFreeze(t)
	bc, mp := newFrozenBoundClient(t)
	ctx := context.Background()

	if err := bc.UpdateStatus(ctx, "t1", "done"); err != nil {
		t.Fatalf("unfrozen UpdateStatus must succeed: %v", err)
	}
	if got, _ := mp.GetTask(ctx, "t1"); got.Status != "done" {
		t.Fatalf("baseline mutation did not land: %+v", got)
	}

	freezeOn(t, "operator", "incident-142")

	cases := []struct {
		name string
		call func() error
	}{
		{"ClaimTask", func() error { return bc.ClaimTask(ctx, "t1", "worker") }},
		{"UpdateStatus", func() error { return bc.UpdateStatus(ctx, "t1", "in-review") }},
		{"AddComment", func() error { return bc.AddComment(ctx, "t1", "hello") }},
		{"CreateTaskLabel", func() error { _, err := bc.CreateTaskLabel(ctx, "t1", "urgent"); return err }},
		{"AttachTaskLabel", func() error { return bc.AttachTaskLabel(ctx, "t1", "label-1") }},
		{"DetachTaskLabel", func() error { return bc.DetachTaskLabel(ctx, "label-1") }},
		{"DeleteTaskLabel", func() error { return bc.DeleteTaskLabel(ctx, "label-1") }},
		{"CreateRelation", func() error { _, err := bc.CreateRelation(ctx, "t1", "t2", RelationBlocks); return err }},
		{"DeleteRelation", func() error { return bc.DeleteRelation(ctx, "rel-1", "t1", "t2") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s must refuse while frozen", tc.name)
			}
			if !errors.Is(err, ErrBoardFrozen) {
				t.Fatalf("%s: want ErrBoardFrozen, got %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), "actor=\"operator\"") || !strings.Contains(err.Error(), "reason=\"incident-142\"") {
				t.Fatalf("%s: refusal must carry actor/reason: %v", tc.name, err)
			}
		})
	}

	// The frozen status write must never have landed.
	if got, _ := mp.GetTask(ctx, "t1"); got.Status != "done" {
		t.Fatalf("status changed despite freeze: %+v", got)
	}
}

func TestBoundClient_ReadsBypassFreeze(t *testing.T) {
	isolateFreeze(t)
	bc, _ := newFrozenBoundClient(t)
	ctx := context.Background()
	freezeOn(t, "operator", "incident-142")

	if _, err := bc.GetTask(ctx, "t1"); err != nil {
		t.Fatalf("GetTask must remain available while frozen: %v", err)
	}
	if _, err := bc.ListTasks(ctx, "", ""); err != nil {
		t.Fatalf("ListTasks must remain available while frozen: %v", err)
	}
	if _, err := bc.ListTaskLabels(ctx, "t1"); err != nil {
		t.Fatalf("ListTaskLabels must remain available while frozen: %v", err)
	}
	if _, err := bc.ListRelations(ctx, "t1"); err != nil {
		t.Fatalf("ListRelations must remain available while frozen: %v", err)
	}
	if _, err := bc.ListProjectRelations(ctx, "p"); err != nil {
		t.Fatalf("ListProjectRelations must remain available while frozen: %v", err)
	}
}

func TestBoundClient_MutationsResumeAfterOff(t *testing.T) {
	isolateFreeze(t)
	bc, _ := newFrozenBoundClient(t)
	ctx := context.Background()
	freezeOn(t, "operator", "incident-142")

	if err := bc.UpdateStatus(ctx, "t1", "done"); !errors.Is(err, ErrBoardFrozen) {
		t.Fatalf("expected frozen refusal, got %v", err)
	}
	if _, err := boardfreeze.SetState(false, "operator", "resolved", "", nil, time.Now()); err != nil {
		t.Fatalf("freeze off: %v", err)
	}
	if err := bc.UpdateStatus(ctx, "t1", "done"); err != nil {
		t.Fatalf("mutation must resume once the gate is off: %v", err)
	}
}

// A caller that retries a refused mutation must never be judged against a
// stale freeze decision: each attempt re-reads live state, so a newer
// generation (even one that keeps the gate on, e.g. an updated reason) is
// always what the retry sees.
func TestBoundClient_RetryPicksUpNewerGeneration(t *testing.T) {
	isolateFreeze(t)
	bc, _ := newFrozenBoundClient(t)
	ctx := context.Background()

	first := freezeOn(t, "operator", "incident-142")
	err1 := bc.UpdateStatus(ctx, "t1", "done")
	if !errors.Is(err1, ErrBoardFrozen) {
		t.Fatalf("first attempt must be refused: %v", err1)
	}
	if !strings.Contains(err1.Error(), "generation=1") {
		t.Fatalf("first refusal should carry generation 1: %v", err1)
	}

	second := freezeOn(t, "operator", "incident-142-followup")
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation must be monotonic: first=%d second=%d", first.Generation, second.Generation)
	}
	err2 := bc.UpdateStatus(ctx, "t1", "done")
	if !errors.Is(err2, ErrBoardFrozen) {
		t.Fatalf("retry must still be refused: %v", err2)
	}
	if !strings.Contains(err2.Error(), "generation=2") {
		t.Fatalf("retry must be judged against the newer generation: %v", err2)
	}
}

func TestBoundClient_FailsClosedWhenFreezeStateUnreadable(t *testing.T) {
	isolateFreeze(t)
	// Corrupt the durable gate file before anything ever opens it as SQLite.
	path := filepath.Join(os.Getenv("HERD_STATE_DIR"), "board-freeze.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}

	bc, mp := newFrozenBoundClient(t)
	ctx := context.Background()
	if err := bc.UpdateStatus(ctx, "t1", "done"); err == nil {
		t.Fatal("unreadable freeze state must fail closed (refuse), not silently proceed")
	}
	if got, _ := mp.GetTask(ctx, "t1"); got.Status == "done" {
		t.Fatalf("mutation must not land when freeze state is unreadable: %+v", got)
	}
}
