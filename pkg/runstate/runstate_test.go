package runstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

func fixture(t *testing.T) (*Store, *provider.MemoryProvider, RunState) {
	t.Helper()
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{ID: "id-1", Ref: "FAC-1", Status: provider.StatusToDo, ProjectID: "p", UpdatedAt: time.Unix(1, 0)})
	mp.AddTask(&provider.Task{ID: "id-2", Ref: "FAC-2", Status: provider.StatusDone, ProjectID: "p", UpdatedAt: time.Unix(2, 0)})
	run, err := FromTasks("run-1", "ship", "main", "graph-a", Policy{Lane: "worker", Model: "codex"}, 2, 1, []*provider.Task{
		{ID: "id-1", Ref: "FAC-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(1, 0)},
		{ID: "id-2", Ref: "FAC-2", Status: provider.StatusDone, UpdatedAt: time.Unix(2, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.TempDir() + "/runs.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, mp, run
}

func TestCheckpointLoadResumeAndTerminalDispatchGate(t *testing.T) {
	s, mp, run := fixture(t)
	saved, err := s.Checkpoint(context.Background(), run, 0)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 {
		t.Fatalf("revision=%d", saved.Revision)
	}
	loaded, err := s.Resume(context.Background(), "run-1", Authority{Tasks: mp, Graph: func(context.Context) (string, error) { return "graph-a", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Dispatchable("FAC-1"); err != nil {
		t.Fatalf("nonterminal dispatch blocked: %v", err)
	}
	if err := loaded.Dispatchable("FAC-2"); !errors.Is(err, ErrTerminal) {
		t.Fatalf("terminal redispatch err=%v", err)
	}
}

func TestResumeRejectsProviderRevisionAndGraphDrift(t *testing.T) {
	s, mp, run := fixture(t)
	if _, err := s.Checkpoint(context.Background(), run, 0); err != nil {
		t.Fatal(err)
	}
	if err := mp.UpdateStatus(context.Background(), "id-1", provider.StatusInProgress); err != nil {
		t.Fatal(err)
	}
	_, err := s.Resume(context.Background(), "run-1", Authority{Tasks: mp, Graph: func(context.Context) (string, error) { return "graph-a", nil }})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("revision drift err=%v", err)
	}
	// A second independent run proves graph drift, rather than letting the
	// prior provider mutation make this assertion vacuous.
	s2, fresh, freshRun := fixture(t)
	freshRun.ID = "run-graph"
	if _, err := s2.Checkpoint(context.Background(), freshRun, 0); err != nil {
		t.Fatal(err)
	}
	_, err = s2.Resume(context.Background(), "run-graph", Authority{Tasks: fresh, Graph: func(context.Context) (string, error) { return "graph-b", nil }})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("graph drift err=%v", err)
	}
}

func TestCheckpointRejectsConcurrentWriterAndMalformedTerminalState(t *testing.T) {
	s, _, run := fixture(t)
	if _, err := s.Checkpoint(context.Background(), run, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Checkpoint(context.Background(), run, 0); !errors.Is(err, ErrConcurrent) {
		t.Fatalf("stale writer err=%v", err)
	}
	run.Tasks[1].Terminal = false
	if _, err := s.Checkpoint(context.Background(), run, 1); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("terminal regression err=%v", err)
	}
}
