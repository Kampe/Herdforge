package runstate

import (
	"context"
	"errors"
	"path/filepath"
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

func TestResumeGraphAuthorityModes(t *testing.T) {
	tests := []struct {
		name   string
		scoped bool
	}{
		{name: "project graph", scoped: false},
		{name: "task graph", scoped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			providerStore := provider.NewMemoryProvider()
			task := &provider.Task{ID: "task-1", Ref: "FAC-516", Status: "to-do"}
			providerStore.AddTask(task)
			state, err := FromTasks("dispatch:task-1", "dispatch", task.Ref, "graph-1", Policy{Lane: "dispatch", Model: "test"}, 0, 0, []*provider.Task{task})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Checkpoint(context.Background(), state, 0); err != nil {
				t.Fatal(err)
			}
			called := false
			authority := Authority{Tasks: providerStore, Graph: func(context.Context) (string, error) {
				if tt.scoped {
					t.Fatal("project graph authority should not be called")
				}
				return "graph-1", nil
			}}
			if tt.scoped {
				authority.GraphForTask = func(_ context.Context, saved TaskState) (string, error) {
					called = true
					if saved.ID != task.ID || saved.Ref != task.Ref {
						t.Fatalf("wrong task: %+v", saved)
					}
					return "graph-1", nil
				}
			}
			if _, err := store.Resume(context.Background(), state.ID, authority); err != nil {
				t.Fatal(err)
			}
			if tt.scoped != called {
				t.Fatalf("scoped authority called=%v, want %v", called, tt.scoped)
			}
		})
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

func TestRecoverStaleRebuildsOnlyExactRunAndRetryReadsBackSameRevision(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	task := &provider.Task{ID: "task-1", Ref: "FAC-654", ProjectID: "project-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(10, 0)}
	tasks := provider.NewMemoryProvider()
	tasks.AddTask(task)
	stale, err := FromTasks("dispatch:task-1", "dispatch", task.Ref, "graph-old", Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Checkpoint(ctx, stale, 0); err != nil {
		t.Fatal(err)
	}
	unrelatedTask := &provider.Task{ID: "task-2", Ref: "FAC-OTHER", ProjectID: "project-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(20, 0)}
	unrelated, err := FromTasks("dispatch:task-2", "dispatch", unrelatedTask.Ref, "graph-other", Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{unrelatedTask})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Checkpoint(ctx, unrelated, 0); err != nil {
		t.Fatal(err)
	}

	req := RecoveryRequest{RunID: stale.ID, TaskID: task.ID, TaskRef: task.Ref, ProjectID: task.ProjectID}
	authority := RecoveryAuthority{
		Authority: Authority{Tasks: tasks, Graph: func(context.Context) (string, error) { return "graph-new", nil }},
		Guard:     func(context.Context, TaskState) error { return nil },
	}
	recovered, err := store.RecoverStale(ctx, req, authority)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != 2 || recovered.DependencyGraphRevision != "graph-new" || recovered.ID != "dispatch:task-1" {
		t.Fatalf("recovered run did not read back exact replacement: %+v", recovered)
	}
	if len(recovered.Tasks) != 1 || recovered.Tasks[0].ID != task.ID || recovered.Tasks[0].Ref != task.Ref || recovered.Tasks[0].ProjectID != task.ProjectID {
		t.Fatalf("recovered task identity drifted: %+v", recovered.Tasks)
	}

	retry, err := store.RecoverStale(ctx, req, authority)
	if err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if retry.Revision != recovered.Revision || retry.UpdatedAt != recovered.UpdatedAt {
		t.Fatalf("retry rewrote recovered row: first=%+v retry=%+v", recovered, retry)
	}
	kept, err := store.Load(ctx, unrelated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Revision != 1 || kept.DependencyGraphRevision != "graph-other" || kept.Tasks[0].ID != unrelatedTask.ID {
		t.Fatalf("unrelated run changed: %+v", kept)
	}
}

func TestRecoverStaleRefusesInvalidAuthorityWithoutMutation(t *testing.T) {
	tests := []struct {
		name      string
		graph     string
		request   func(RecoveryRequest) RecoveryRequest
		live      func(*provider.Task)
		ambiguous bool
		want      error
	}{
		{name: "non-stale run", graph: "graph-old", want: ErrNotStale},
		{name: "wrong task ref", graph: "graph-new", request: func(r RecoveryRequest) RecoveryRequest { r.TaskRef = "FAC-WRONG"; return r }, want: ErrAmbiguous},
		{name: "cross-project task", graph: "graph-new", request: func(r RecoveryRequest) RecoveryRequest { r.ProjectID = "project-other"; return r }, want: ErrAmbiguous},
		{name: "provider UNKNOWN", graph: "graph-new", live: func(task *provider.Task) { task.Status = "mystery-column" }, want: ErrAmbiguous},
		{name: "terminal task", graph: "graph-new", live: func(task *provider.Task) { task.Status = provider.StatusDone }, want: ErrTerminal},
		{name: "ambiguous saved run", graph: "graph-new", ambiguous: true, want: ErrAmbiguous},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "runs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			task := &provider.Task{ID: "task-1", Ref: "FAC-654", ProjectID: "project-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(10, 0)}
			tasks := provider.NewMemoryProvider()
			storedTasks := []*provider.Task{task}
			if tt.ambiguous {
				other := &provider.Task{ID: "task-2", Ref: "FAC-OTHER", ProjectID: task.ProjectID, Status: provider.StatusToDo, UpdatedAt: time.Unix(11, 0)}
				storedTasks = append(storedTasks, other)
				tasks.AddTask(other)
			}
			stale, err := FromTasks("dispatch:"+task.ID, "dispatch", task.Ref, "graph-old", Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, storedTasks)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Checkpoint(ctx, stale, 0); err != nil {
				t.Fatal(err)
			}
			live := *task
			if tt.live != nil {
				tt.live(&live)
			}
			tasks.AddTask(&live)
			req := RecoveryRequest{RunID: stale.ID, TaskID: task.ID, TaskRef: task.Ref, ProjectID: task.ProjectID}
			if tt.request != nil {
				req = tt.request(req)
			}
			_, err = store.RecoverStale(ctx, req, RecoveryAuthority{
				Authority: Authority{Tasks: tasks, Graph: func(context.Context) (string, error) { return tt.graph, nil }},
				Guard:     func(context.Context, TaskState) error { return nil },
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("recovery err=%v, want %v", err, tt.want)
			}
			kept, err := store.Load(ctx, stale.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kept.Revision != 1 || kept.DependencyGraphRevision != "graph-old" || kept.Recovery != nil {
				t.Fatalf("refused recovery mutated row: %+v", kept)
			}
		})
	}
}

func TestRecoverStaleConcurrentDifferentAuthoritiesCannotBothReplaceRow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	task := &provider.Task{ID: "task-1", Ref: "FAC-654", ProjectID: "project-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(10, 0)}
	tasks := provider.NewMemoryProvider()
	tasks.AddTask(task)
	stale, err := FromTasks("dispatch:"+task.ID, "dispatch", task.Ref, "graph-old", Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Checkpoint(ctx, stale, 0); err != nil {
		t.Fatal(err)
	}

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	guard := func(context.Context, TaskState) error {
		ready <- struct{}{}
		<-release
		return nil
	}
	type result struct {
		state *RunState
		err   error
	}
	results := make(chan result, 2)
	for _, graph := range []string{"graph-a", "graph-b"} {
		graph := graph
		go func() {
			state, err := store.RecoverStale(ctx, RecoveryRequest{RunID: stale.ID, TaskID: task.ID, TaskRef: task.Ref, ProjectID: task.ProjectID}, RecoveryAuthority{
				Authority: Authority{Tasks: tasks, Graph: func(context.Context) (string, error) { return graph, nil }}, Guard: guard,
			})
			results <- result{state: state, err: err}
		}()
	}
	<-ready
	<-ready
	close(release)
	var successes, conflicts int
	for range 2 {
		res := <-results
		switch {
		case res.err == nil && res.state != nil:
			successes++
		case errors.Is(res.err, ErrConcurrent):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent result: state=%+v err=%v", res.state, res.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent outcomes success=%d conflict=%d", successes, conflicts)
	}
	kept, err := store.Load(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Revision != 2 || (kept.DependencyGraphRevision != "graph-a" && kept.DependencyGraphRevision != "graph-b") {
		t.Fatalf("concurrent row=%+v", kept)
	}
}
