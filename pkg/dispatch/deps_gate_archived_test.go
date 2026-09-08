package dispatch

import (
	"context"
	"fmt"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// endpointFaultTaskProvider wraps provider.MemoryProvider to reproduce,
// hermetically, the real Kaneo characteristic FAC-777 review R3's production-
// boundary regression targets: a single-task GetTask can fail for an id that
// a project-scoped archived-status listing still resolves, and an unfiltered
// listing can omit a row a status-scoped read finds. No live board.
type endpointFaultTaskProvider struct {
	*provider.MemoryProvider
	hideFromUnfiltered map[string]bool
	failGetTask        map[string]error
}

func newEndpointFaultTaskProvider() *endpointFaultTaskProvider {
	return &endpointFaultTaskProvider{
		MemoryProvider:     provider.NewMemoryProvider(),
		hideFromUnfiltered: map[string]bool{},
		failGetTask:        map[string]error{},
	}
}

func (p *endpointFaultTaskProvider) GetTask(ctx context.Context, id string) (*provider.Task, error) {
	if err, ok := p.failGetTask[id]; ok {
		return nil, err
	}
	return p.MemoryProvider.GetTask(ctx, id)
}

func (p *endpointFaultTaskProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	tasks, err := p.MemoryProvider.ListTasks(ctx, projectID, status)
	if err != nil {
		return nil, err
	}
	if status != "" {
		return tasks, nil
	}
	out := make([]*provider.Task, 0, len(tasks))
	for _, t := range tasks {
		if t != nil && p.hideFromUnfiltered[t.ID] {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// TestDispatch_ArchivedBlockerViaColumnEvidence_PassesGate is the production
// -boundary regression for FAC-777 review R3: a genuinely archived
// prerequisite -- single-task GetTask miss, exact project-scoped archived-
// list positive -- reaches the real Dispatcher.Dispatch entry path (not just
// the deps helper), and the gate passes before any worktree/claim/provider
// write. mockWorktree.err makes the mock fail immediately AFTER the gate, so
// mw.calls==1 proves the gate itself ran and allowed the attempt.
func TestDispatch_ArchivedBlockerViaColumnEvidence_PassesGate(t *testing.T) {
	p := newEndpointFaultTaskProvider()
	p.AddTask(&provider.Task{
		ID: "b1", Ref: "FAC-136", Title: "blocker", Status: provider.StatusArchived,
		Priority: provider.PriorityHigh, ProjectID: "test",
		Description: emptyDepsFence("FAC-136", "b1"),
	})
	p.AddTask(&provider.Task{
		ID: "t1", Ref: "FAC-75", Title: "dependent", Status: "to-do",
		Priority: provider.PriorityHigh, ProjectID: "test",
		Description: "```herd-deps-v1\n" +
			`{"version":1,"task_ref":"FAC-75","task_id":"t1","edges":[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]}` +
			"\n```\n",
	})
	if _, err := p.CreateRelation(context.Background(), "b1", "t1", provider.RelationBlocks); err != nil {
		t.Fatal(err)
	}
	// Hidden from the unfiltered hydrate listing, and its single GetTask
	// fails by both id and ref -- forces resolution through the archived-
	// column evidence path, not a cache hit or a lucky ref-based GetTask.
	p.hideFromUnfiltered["b1"] = true
	notFound := fmt.Errorf("kaneo: GetTask b1: 404 not found")
	p.failGetTask["b1"] = notFound
	p.failGetTask["FAC-136"] = notFound

	cfg := &config.Config{
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Project:      config.ProjectConfig{DefaultBranch: "main"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "m", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
		Verification: config.Verification{TestCommand: "go test ./..."},
	}
	mw := &mockWorktree{err: context.Canceled} // fail after gate, proves the gate itself ran
	d := withTestLease(t, &Dispatcher{
		Config:       cfg,
		TaskProvider: p,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
		Deps:         deps.StoreFor(p, "test"),
	})

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-75", NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if mw.calls != 1 {
		t.Fatalf("expected worktree attempt after archived-terminal gate passes, calls=%d err=%v", mw.calls, err)
	}
	if err == nil {
		t.Fatal("expected worktree error after gate (mock always fails past the gate)")
	}
	if deps.IsBlocked(err) {
		t.Fatalf("archived (column-evidenced) blocker must not leave BLOCKED: %v", err)
	}
}

// TestDispatch_InvalidArchivedEvidenceRefusesBeforeWorktree is the negative
// half of the same production-boundary regression: the prerequisite's single
// GetTask fails and there is no valid archived-column evidence for it (wrong
// status, mirroring FAC-777 review R3's HIGH finding). Dispatch must refuse
// before any worktree/claim/provider write.
func TestDispatch_InvalidArchivedEvidenceRefusesBeforeWorktree(t *testing.T) {
	p := newEndpointFaultTaskProvider()
	p.AddTask(&provider.Task{
		ID: "b1", Ref: "FAC-136", Title: "blocker", Status: "to-do",
		Priority: provider.PriorityHigh, ProjectID: "test",
		Description: emptyDepsFence("FAC-136", "b1"),
	})
	p.AddTask(&provider.Task{
		ID: "t1", Ref: "FAC-75", Title: "dependent", Status: "to-do",
		Priority: provider.PriorityHigh, ProjectID: "test",
		Description: "```herd-deps-v1\n" +
			`{"version":1,"task_ref":"FAC-75","task_id":"t1","edges":[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]}` +
			"\n```\n",
	})
	if _, err := p.CreateRelation(context.Background(), "b1", "t1", provider.RelationBlocks); err != nil {
		t.Fatal(err)
	}
	// Same unreadable-single-GetTask shape as the positive case, but the
	// task's real status is "to-do" (never archived) -- the archived-column
	// query legitimately returns nothing for it. No evidence exists.
	p.hideFromUnfiltered["b1"] = true
	notFound := fmt.Errorf("kaneo: GetTask b1: 404 not found")
	p.failGetTask["b1"] = notFound
	p.failGetTask["FAC-136"] = notFound

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
		TaskProvider: p,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
		Deps:         deps.StoreFor(p, "test"),
	}

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-75", NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if err == nil {
		t.Fatal("expected refusal: unreadable prerequisite with no archived evidence")
	}
	if mw.calls != 0 {
		t.Fatalf("worktree must not be created before invalid evidence is refused: calls=%d", mw.calls)
	}
}
