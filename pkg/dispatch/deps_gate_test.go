package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// TestDispatch_DepsGateBlocksBeforeWorktree proves the FAC-159 gate runs
// before CreateTaskWorktreeFrom (no side effects on open blockers).
func TestDispatch_DepsGateBlocksBeforeWorktree(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "b1", Ref: "FAC-136", Title: "blocker", Status: "to-do",
		Priority: provider.PriorityHigh, ProjectID: "test",
	})
	mp.AddTask(&provider.Task{
		ID: "t1", Ref: "FAC-75", Title: "dependent", Status: "to-do",
		Priority: provider.PriorityHigh, ProjectID: "test",
		Description: "```herd-deps-v1\n" +
			`{"version":1,"task_ref":"FAC-75","edges":[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]}` +
			"\n```\n",
	})
	// Board edge present; blocker not Done.
	if _, err := mp.CreateRelation(context.Background(), "b1", "t1", provider.RelationBlocks); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "m", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
		Verification: config.Verification{TestCommand: "go test ./..."},
	}
	mw := &mockWorktree{}
	d := &Dispatcher{
		Config:       cfg,
		TaskProvider: mp,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
		Deps:         deps.StoreFor(mp, "test"),
	}

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-75", NoLaunch: true})
	if err == nil {
		t.Fatal("expected dependency block")
	}
	if !deps.IsBlocked(err) && !strings.Contains(err.Error(), "BLOCKED") && !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("want blocked error, got %v", err)
	}
	if mw.calls != 0 {
		t.Fatalf("worktree must not be created before gate passes: calls=%d", mw.calls)
	}
}

// TestDispatch_DepsGateAllowsWhenBlockerDone unlocks after verified Done.
func TestDispatch_DepsGateAllowsWhenBlockerDone(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "b1", Ref: "FAC-136", Title: "blocker", Status: "done",
		Priority: provider.PriorityHigh, ProjectID: "test",
	})
	mp.AddTask(&provider.Task{
		ID: "t1", Ref: "FAC-75", Title: "dependent", Status: "to-do",
		Priority: provider.PriorityHigh, ProjectID: "test",
	})
	if _, err := mp.CreateRelation(context.Background(), "b1", "t1", provider.RelationBlocks); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Project:      config.ProjectConfig{DefaultBranch: "main"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "m", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
		Verification: config.Verification{TestCommand: "go test ./..."},
	}
	mw := &mockWorktree{err: context.Canceled} // fail after gate
	d := &Dispatcher{
		Config:       cfg,
		TaskProvider: mp,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
		Deps:         deps.StoreFor(mp, "test"),
	}

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-75", NoLaunch: true})
	// Gate passed; worktree mock failed — proves gate did not block Done prerequisite.
	if mw.calls != 1 {
		t.Fatalf("expected worktree attempt after green gate, calls=%d err=%v", mw.calls, err)
	}
	if err == nil {
		t.Fatal("expected worktree error after gate")
	}
	if deps.IsBlocked(err) {
		t.Fatalf("Done blocker must not leave BLOCKED: %v", err)
	}
}
