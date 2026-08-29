package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/security"
)

func TestEnvironmentBindingUsesRequestedTaskScopedGraphForCreateAndRecovery(t *testing.T) {
	const (
		globalPublishedGraph    = "f98f2d878fd1dcb85dcf4808845f5e427ac493dbcf35ef6d7f5ca81cfe62483c"
		requestedPublishedGraph = "5932243ce5889d48af1f844fe059e15590ba74bbf4eee0b51ee36895e9ab60eb"
		unrelatedPublishedGraph = "a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091"
	)

	for _, recoverStale := range []bool{false, true} {
		name := "create"
		if recoverStale {
			name = "recover"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			task := &provider.Task{ID: "task-requested", Ref: "FAC-631", ProjectID: "project-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(10, 0)}
			unrelated := &provider.Task{ID: "task-unrelated", Ref: "FAC-999", ProjectID: task.ProjectID, Status: provider.StatusToDo, UpdatedAt: time.Unix(20, 0)}
			tasks := provider.NewMemoryProvider()
			tasks.AddTask(task)
			tasks.AddTask(unrelated)
			runs, err := runstate.Open(filepath.Join(t.TempDir(), "runs.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			if recoverStale {
				stale, buildErr := runstate.FromTasks("dispatch:"+task.ID, "dispatch", task.Ref, globalPublishedGraph, runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
				if buildErr != nil {
					t.Fatal(buildErr)
				}
				if _, checkpointErr := runs.Checkpoint(ctx, stale, 0); checkpointErr != nil {
					t.Fatal(checkpointErr)
				}
			}

			revisions := map[string]string{task.ID: requestedPublishedGraph, unrelated.ID: unrelatedPublishedGraph}
			var scopedReads []runstate.TaskState
			graphForTask := func(_ context.Context, saved runstate.TaskState) (string, error) {
				scopedReads = append(scopedReads, saved)
				revision, ok := revisions[saved.ID]
				if !ok || saved.Ref != task.Ref {
					return "", fmt.Errorf("unexpected scoped graph identity: %+v", saved)
				}
				return revision, nil
			}

			binding, err := environmentBindingFromAuthorities(ctx,
				environmentBindingRequest{TicketRef: task.Ref, TaskID: task.ID, RecoverStale: recoverStale},
				environmentBindingAuthorities{
					Provider: "memory", ProjectID: task.ProjectID, Tasks: tasks,
					GraphForTask: graphForTask, Runs: runs,
					Claims: security.MapClaimLookup{},
					Launches: dispatch.LiveLaunchLookupFunc(func(context.Context, string, string) (bool, error) {
						return false, nil
					}),
				})
			if err != nil {
				t.Fatal(err)
			}
			if binding.GraphRevision != requestedPublishedGraph {
				t.Fatalf("binding graph=%q, want requested task-scoped graph %q", binding.GraphRevision, requestedPublishedGraph)
			}
			if len(scopedReads) == 0 {
				t.Fatal("task-scoped graph authority was not read")
			}
			for _, read := range scopedReads {
				if read.ID != task.ID || read.Ref != task.Ref || read.ProjectID != task.ProjectID {
					t.Fatalf("scoped graph read=%+v, want exact requested task identity", read)
				}
			}
		})
	}
}

func TestEnvironmentBindingExplicitRecoveryReturnsFreshExactRunRevision(t *testing.T) {
	ctx := context.Background()
	task := &provider.Task{ID: "task-1", Ref: "FAC-654", ProjectID: "project-1", Status: provider.StatusToDo, UpdatedAt: time.Unix(10, 0)}
	tasks := provider.NewMemoryProvider()
	tasks.AddTask(task)
	runs, err := runstate.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	stale, err := runstate.FromTasks("dispatch:"+task.ID, "dispatch", task.Ref, "graph-old", runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.Checkpoint(ctx, stale, 0); err != nil {
		t.Fatal(err)
	}

	binding, err := environmentBindingFromAuthorities(ctx,
		environmentBindingRequest{TicketRef: task.Ref, TaskID: task.ID, RecoverStale: true},
		environmentBindingAuthorities{
			Provider: "memory", ProjectID: task.ProjectID, Tasks: tasks,
			GraphForTask: func(context.Context, runstate.TaskState) (string, error) {
				return "graph-new", nil
			},
			Runs:   runs,
			Claims: security.MapClaimLookup{},
			Launches: dispatch.LiveLaunchLookupFunc(func(context.Context, string, string) (bool, error) {
				return false, nil
			}),
		})
	if err != nil {
		t.Fatal(err)
	}
	if binding.TaskRef != task.Ref || binding.TaskID != task.ID || binding.Provider != "memory" || binding.GraphRevision != "graph-new" || binding.RunID != stale.ID || binding.RunRevision != 2 {
		t.Fatalf("fresh binding=%+v", binding)
	}
}

func TestParseEnvironmentPlanCreateRecoveryRequiresExactTaskID(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	req, err := parseEnvironmentPlanCreateArgs([]string{"FAC-654", "--task-id", "task-1", "--recover-stale-run", "--expires-at", now.Add(time.Hour).Format(time.RFC3339), "--capability", "network"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if req.Binding != (environmentBindingRequest{TicketRef: "FAC-654", TaskID: "task-1", RecoverStale: true}) {
		t.Fatalf("recovery selector=%+v", req.Binding)
	}
	if _, err := parseEnvironmentPlanCreateArgs([]string{"FAC-654", "--recover-stale-run", "--expires-at", now.Add(time.Hour).Format(time.RFC3339)}, now); err == nil {
		t.Fatal("recovery without exact --task-id was accepted")
	}
}

func TestEnvironmentPlanHelpPublishesExplicitRecoverySelector(t *testing.T) {
	help := subcommandUsage["envplan"]
	for _, want := range []string{"--recover-stale-run", "--task-id", "exact stale dispatch run"} {
		if !strings.Contains(help, want) {
			t.Fatalf("envplan help omits %q:\n%s", want, help)
		}
	}
}

func TestParseDispatchArgsPreservesExactEnvironmentPlanID(t *testing.T) {
	req, err := parseDispatchArgs([]string{"FAC-311", "--environment-plan", "env-exact"})
	if err != nil {
		t.Fatal(err)
	}
	if req.EnvironmentPlanID != "env-exact" {
		t.Fatalf("environment plan id=%q, want exact selector", req.EnvironmentPlanID)
	}
}

func TestForgeDriverForwardsEnvironmentPlanToDispatch(t *testing.T) {
	var got []string
	restore := setHerdSubprocessForTest(func(args ...string) error { got = append([]string(nil), args...); return nil })
	t.Cleanup(restore)
	d := &cliForgeDriver{environmentPlanID: "env-exact"}
	if err := d.Dispatch(t.Context(), &provider.Task{Ref: "FAC-311"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range got {
		if got[i] == "--environment-plan" && i+1 < len(got) && got[i+1] == "env-exact" {
			found = true
		}
	}
	if !found {
		t.Fatalf("forge dispatch argv omitted exact plan: %v", got)
	}
}
