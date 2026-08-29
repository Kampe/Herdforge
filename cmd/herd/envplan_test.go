package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/security"
)

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
			Provider: "memory", ProjectID: task.ProjectID, Tasks: tasks, GraphRevision: "graph-new", Runs: runs,
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
