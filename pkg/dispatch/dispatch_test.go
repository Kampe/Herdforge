package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/recovery"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/scopefence"
	"github.com/Kampe/Herdforge/pkg/worktree"
	"github.com/Kampe/Herdforge/pkg/worktreebootstrap"
)

// withTestLease injects a durable SQLite launch lease (temp path) so Dispatch
// never opens the mock RepoRoot (/nonexistent-...).
func withTestLease(t *testing.T, d *Dispatcher) *Dispatcher {
	t.Helper()
	own, err := deps.OpenLeaseOwnership(filepath.Join(t.TempDir(), "launch.db"), "herd", "memory", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = own.Close() })
	own.LaneResolver = func(role string) (string, error) {
		if role == "launch" || role == "worker" {
			return "smith", nil
		}
		return "", fmt.Errorf("unknown configured test role %q", role)
	}
	d.Ownership = own
	return d
}

type mockTaskProvider struct {
	tasks     []*provider.Task
	relations []provider.Relation
}

func (m *mockTaskProvider) GetTask(_ context.Context, id string) (*provider.Task, error) {
	for _, t := range m.tasks {
		if t.ID == id || t.Ref == id {
			return t, nil
		}
	}
	return nil, fmt.Errorf("task not found: %s", id)
}
func (m *mockTaskProvider) ListTasks(_ context.Context, _, _ string) ([]*provider.Task, error) {
	return m.tasks, nil
}

func (m *mockTaskProvider) ClaimTask(_ context.Context, _, _ string) error { return nil }
func (m *mockTaskProvider) UpdateStatus(_ context.Context, _, _ string) error {
	return nil
}
func (m *mockTaskProvider) AddComment(_ context.Context, _, _ string) error { return nil }

// RelationProvider (FAC-159) — empty board relations by default.
func (m *mockTaskProvider) ListRelations(_ context.Context, taskID string) ([]provider.Relation, error) {
	var out []provider.Relation
	for _, r := range m.relations {
		if r.SourceTaskID == taskID || r.TargetTaskID == taskID {
			out = append(out, r)
		}
	}
	return out, nil
}
func (m *mockTaskProvider) CreateRelation(_ context.Context, sourceID, targetID string, typ provider.RelationType) (*provider.Relation, error) {
	r := provider.Relation{ID: "mock-rel", SourceTaskID: sourceID, TargetTaskID: targetID, Type: typ}
	m.relations = append(m.relations, r)
	return &r, nil
}
func (m *mockTaskProvider) DeleteRelation(_ context.Context, relationID, sourceID, targetID string) error {
	for i, r := range m.relations {
		if r.ID == relationID {
			m.relations = append(m.relations[:i], m.relations[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("relation not found: %s", relationID)
}

// mockWorktree never touches the ambient git repo or package cwd (FAC-121).
type mockWorktree struct {
	root  string
	info  *worktree.WorktreeInfo
	err   error
	calls int
	refs  []string // task refs requested
}

type failingWorktreeBootstrap struct {
	calls int
	path  string
	err   error
}

func (b *failingWorktreeBootstrap) Execute(_ context.Context, path string, _ config.WorktreeBootstrap) (*worktreebootstrap.Result, error) {
	b.calls++
	b.path = path
	return nil, b.err
}

type recordingScopeAdmission struct {
	called     bool
	request    scopefence.AcquireRequest
	decision   scopefence.Decision
	err        error
	releases   int
	releaseReq scopefence.ReleaseRequest
}

func (f *recordingScopeAdmission) Acquire(_ context.Context, request scopefence.AcquireRequest) (scopefence.Decision, error) {
	f.called = true
	f.request = request
	if f.decision.Granted && f.decision.Lease == nil {
		lease := request.Ownership
		lease.GraphRevision, lease.GraphFiles = "g1", 2
		f.decision.Lease = &lease
	}
	return f.decision, f.err
}

func (f *recordingScopeAdmission) Release(_ context.Context, request scopefence.ReleaseRequest) error {
	f.releases++
	f.releaseReq = request
	return nil
}

func (m *mockWorktree) CreateTaskWorktreeFrom(_ context.Context, taskRef, _ string) (*worktree.WorktreeInfo, error) {
	m.calls++
	m.refs = append(m.refs, taskRef)
	if m.err != nil {
		return nil, m.err
	}
	if m.info != nil {
		return m.info, nil
	}
	return nil, fmt.Errorf("mock worktree: no fixture for %s", taskRef)
}
func (m *mockWorktree) RepoRoot() string {
	if m.root != "" {
		return m.root
	}
	return "/nonexistent-mock-repo-root"
}

func emptyDepsFence(ref, id string) string {
	return "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"" + ref + "\",\"task_id\":\"" + id + "\",\"edges\":[]}\n```\n"
}

func TestFindTicket(t *testing.T) {
	tp := &mockTaskProvider{
		tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Title: "First task", Status: "to-do", Priority: "high", Description: emptyDepsFence("FAC-1", "1")},
			{ID: "2", Ref: "FAC-2", Title: "Second task", Status: "to-do", Priority: "medium", Description: emptyDepsFence("FAC-2", "2")},
		},
	}
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "deepseek-v4-flash", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
	}
	// Fully mocked worktree: no git, no package-cwd .herd/worktrees pollution.
	mw := &mockWorktree{err: fmt.Errorf("mock: refuse real worktree creation")}
	d := withTestLease(t, &Dispatcher{
		Config:       cfg,
		TaskProvider: tp,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
	})

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	// Ticket is found; mock worktree fails after lookup.
	if err == nil {
		t.Fatal("expected worktree error after finding ticket")
	}
	if strings.Contains(err.Error(), "ticket FAC-1 not found") {
		t.Fatalf("ticket should have been found: %v", err)
	}
	if mw.calls != 1 || (len(mw.refs) > 0 && mw.refs[0] != "FAC-1") {
		t.Fatalf("expected one CreateTaskWorktreeFrom(FAC-1), got calls=%d refs=%v", mw.calls, mw.refs)
	}

	_, err = d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-NONEXIST", NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if err == nil {
		t.Fatal("expected error for non-existent ticket")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	// Non-existent ticket must not touch worktree service.
	if mw.calls != 1 {
		t.Fatalf("missing ticket must not create worktree; calls=%d", mw.calls)
	}
}

// TestDispatchRunStateGateBlocksTerminalTaskBeforeMutations exercises the
// production Dispatcher path, including durable checkpoint/load/resume, rather
// than only RunState.Dispatchable in isolation. The worktree assertion is the
// non-vacuous mutation boundary: it would be one on the pre-gate dispatcher.
func TestDispatchRunStateGateBlocksTerminalTaskBeforeMutations(t *testing.T) {
	terminal := &provider.Task{ID: "terminal-id", Ref: "FAC-235", Status: provider.StatusDone, Description: emptyDepsFence("FAC-235", "terminal-id")}
	tp := &mockTaskProvider{tasks: []*provider.Task{terminal}}
	mw := &mockWorktree{err: fmt.Errorf("terminal task must never create a worktree")}
	states, err := runstate.Open(filepath.Join(t.TempDir(), "dispatch-runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = states.Close() })
	d := withTestLease(t, &Dispatcher{
		Production:    true,
		Config:        &config.Config{TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"}, Lanes: []config.LaneDef{{Name: "worker", Role: "worker"}}},
		TaskProvider:  tp,
		Worktree:      mw,
		Compensator:   &recordingCompensator{},
		ScopeFence:    &recordingScopeAdmission{},
		RunStates:     states,
		RunStateGraph: func(context.Context) (string, error) { return "graph-fac-235", nil },
	})

	_, err = d.Dispatch(context.Background(), DispatchOptions{TicketRef: terminal.Ref, NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if !errors.Is(err, runstate.ErrTerminal) {
		t.Fatalf("terminal redispatch error=%v, want ErrTerminal", err)
	}
	if mw.calls != 0 {
		t.Fatalf("terminal run crossed worktree mutation boundary: calls=%d", mw.calls)
	}
	saved, err := states.Load(context.Background(), "dispatch:"+terminal.ID)
	if err != nil {
		t.Fatalf("durable terminal evidence missing: %v", err)
	}
	if len(saved.Tasks) != 1 || !saved.Tasks[0].Terminal || saved.Tasks[0].Ref != terminal.Ref {
		t.Fatalf("durable terminal evidence=%+v", saved.Tasks)
	}
}

// TestProductionDispatchMissingRunStateFailsBeforeWorktree proves the nil
// store guard is a real mutation boundary. Before FAC-235's production guard,
// this fixture reaches mockWorktree.CreateTaskWorktreeFrom and increments
// calls, so the assertion fails against that regression.
func TestProductionDispatchMissingRunStateFailsBeforeWorktree(t *testing.T) {
	task := &provider.Task{ID: "missing-state-id", Ref: "FAC-235", Status: "to-do", Description: emptyDepsFence("FAC-235", "missing-state-id")}
	repo, _ := initDispatchRepo(t)
	mw := &mockWorktree{root: repo, err: fmt.Errorf("missing run-state store must not create a worktree")}
	d := withTestLease(t, &Dispatcher{
		Production:   true,
		Config:       &config.Config{TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"}, Lanes: []config.LaneDef{{Name: "worker", Role: "worker"}}},
		TaskProvider: &mockTaskProvider{tasks: []*provider.Task{task}},
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		ScopeFence:   &recordingScopeAdmission{decision: scopefence.Decision{Granted: true}},
	})

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: task.Ref, NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if err == nil || !strings.Contains(err.Error(), "durable run-state store is required") {
		t.Fatalf("missing production run-state store error=%v", err)
	}
	if mw.calls != 0 {
		t.Fatalf("missing production run-state store crossed worktree boundary: calls=%d", mw.calls)
	}
}

func TestProductionDispatchMissingEnvironmentPlanFailsBeforeWorktree(t *testing.T) {
	task := &provider.Task{ID: "envplan-id", Ref: "FAC-241", Status: "to-do", Description: emptyDepsFence("FAC-241", "envplan-id")}
	repo, _ := initDispatchRepo(t)
	mw := &mockWorktree{root: repo, err: fmt.Errorf("environment plan must block before worktree")}
	states, err := runstate.Open(filepath.Join(t.TempDir(), "dispatch-runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = states.Close() })
	d := withTestLease(t, &Dispatcher{
		Production:   true,
		Config:       &config.Config{TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"}, Lanes: []config.LaneDef{{Name: "worker", Role: "worker"}}},
		TaskProvider: &mockTaskProvider{tasks: []*provider.Task{task}}, Worktree: mw,
		Compensator: &recordingCompensator{}, ScopeFence: &recordingScopeAdmission{decision: scopefence.Decision{Granted: true}},
		RunStates: states, RunStateGraph: func(context.Context) (string, error) { return "graph-fac-241", nil },
	})
	_, err = d.Dispatch(context.Background(), DispatchOptions{TicketRef: task.Ref, NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if err == nil || !strings.Contains(err.Error(), "environment plan store is required") {
		t.Fatalf("missing environment plan error=%v", err)
	}
	if mw.calls != 0 {
		t.Fatalf("missing environment plan crossed worktree boundary: calls=%d", mw.calls)
	}
}

func TestDispatchRecoveryAttemptBudgetBlocksBeforeWorktree(t *testing.T) {
	task := &provider.Task{ID: "recovery-id", Ref: "FAC-236", Status: provider.StatusToDo, Description: emptyDepsFence("FAC-236", "recovery-id")}
	states, err := runstate.Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = states.Close() })
	recoveries, err := recovery.Open(filepath.Join(t.TempDir(), "recovery.json"), 1)
	if err != nil {
		t.Fatal(err)
	}
	mw := &mockWorktree{err: fmt.Errorf("recovery budget must block before worktree")}
	d := withTestLease(t, &Dispatcher{
		Production:   true,
		Config:       &config.Config{TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"}, Lanes: []config.LaneDef{{Name: "worker", Role: "worker"}}},
		TaskProvider: &mockTaskProvider{tasks: []*provider.Task{task}}, Worktree: mw,
		Compensator: &recordingCompensator{}, ScopeFence: &recordingScopeAdmission{decision: scopefence.Decision{Granted: true}},
		RunStates: states, RunStateGraph: func(context.Context) (string, error) { return "graph-fac-236", nil }, Recovery: recoveries,
	})
	opts := DispatchOptions{TicketRef: task.Ref, NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1}
	_, firstErr := d.Dispatch(context.Background(), opts)
	if firstErr == nil || !strings.Contains(firstErr.Error(), "environment plan store is required") {
		t.Fatalf("first dispatch should stop after consuming its bounded attempt: %v", firstErr)
	}
	_, secondErr := d.Dispatch(context.Background(), opts)
	if !errors.Is(secondErr, recovery.ErrMaxAttempts) {
		t.Fatalf("restart-equivalent dispatch error=%v", secondErr)
	}
	if mw.calls != 0 {
		t.Fatalf("recovery budget crossed worktree boundary: calls=%d", mw.calls)
	}
}

func TestDispatchRejectsMissingDecisionBeforeAnyProviderOrWorktreeMutation(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(t.TempDir(), "receipts.jsonl"))
	tp := &mockTaskProvider{tasks: []*provider.Task{{ID: "1", Ref: "FAC-175"}}}
	mw := &mockWorktree{err: fmt.Errorf("must not be called")}
	d := &Dispatcher{Config: &config.Config{TaskProvider: config.TaskProvider{ProjectID: "p"}, Lanes: []config.LaneDef{{Name: "worker"}}}, TaskProvider: tp, Worktree: mw, Compensator: &recordingCompensator{}}
	if _, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-175", LeaseID: "claim:1", LeaseGeneration: 1}); err == nil {
		t.Fatal("missing routed decision must fail closed")
	}
	if mw.calls != 0 {
		t.Fatalf("rejected launch created worktree: %d", mw.calls)
	}
}

func TestDispatchScopeFenceRejectsBeforeWorktreeOrLaunchSideEffects(t *testing.T) {
	tp := &mockTaskProvider{tasks: []*provider.Task{{ID: "1", Ref: "FAC-205", Title: "scope fence test", Status: "to-do", Description: emptyDepsFence("FAC-205", "1")}}}
	mw := &mockWorktree{err: fmt.Errorf("scope fence must reject first")}
	fence := &recordingScopeAdmission{decision: scopefence.Decision{Evidence: scopefence.Evidence{Reason: scopefence.ReasonScopeOverlap}}}
	d := withTestLease(t, &Dispatcher{
		Config:       &config.Config{TaskProvider: config.TaskProvider{ProjectID: "test"}, Lanes: []config.LaneDef{{Name: "worker"}}},
		TaskProvider: tp,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
		ScopeFence:   fence,
	})
	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-205", NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if err == nil || !fence.called || mw.calls != 0 {
		t.Fatalf("scope rejection crossed worktree boundary: err=%v fence_called=%v worktree_calls=%d", err, fence.called, mw.calls)
	}
	if fence.request.Generation <= 0 || fence.request.Identity.Branch != worktree.TaskBranch("FAC-205") || len(fence.request.Scope.Packages) != 0 {
		t.Fatalf("scope admission was not bound to exact pre-side-effect identity: %+v", fence.request)
	}
}

func TestSlugForTask(t *testing.T) {
	cases := []struct {
		ref   string
		title string
		want  string
	}{
		{"FAC-33", "Port herd-next priority-ordered action picker", "fac-33-port-herd-next-priority-ordered-action-picker"},
		{"FAC-1", "Hello World", "fac-1-hello-world"},
	}
	for _, c := range cases {
		got := slugForTask(c.ref, c.title)
		if got != c.want {
			t.Errorf("slugForTask(%q, %q) = %q, want %q", c.ref, c.title, got, c.want)
		}
	}
}

func TestBuildTaskPacket(t *testing.T) {
	task := &provider.Task{
		Ref:         "FAC-33",
		Title:       "Test task",
		Description: "Do the thing",
		Status:      "to-do",
		Priority:    "high",
		Labels:      []string{"go", "core"},
	}
	lane := &config.LaneDef{
		Name: "worker", Role: "worker", AgentKind: "opencode",
		Model: "deepseek-v4-flash", Prompt: ".herd/prompts/worker.md",
	}
	verification := config.Verification{TestCommand: "go test ./...", PreflightCommand: "go build ./..."}
	packet := buildTaskPacket(task, "herd/fac-33", ".herd/prompts/worker.md", "kaneo", "fac-proj", lane, verification, ReplyTarget{Name: "coordinator", LeaseGeneration: 1})
	if !strings.Contains(packet, "FAC-33") {
		t.Error("packet should contain ticket ref")
	}
	if !strings.Contains(packet, "herd/fac-33") {
		t.Error("packet must carry the actual Git branch name")
	}
	// FAC-115: reference-based, NOT an inline spec dump — the agent reads the
	// card itself, and the packet must be tight (context-budget fix).
	// FAC-145: the reference is the receipt-gated broker, never a direct
	// provider CLI with ambient credentials.
	if !strings.Contains(packet, "herd task get FAC-33 --full") {
		t.Error("packet must tell the agent to read the card via the receipt-gated broker")
	}
	if strings.Contains(packet, "kaneo task get") || strings.Contains(packet, "--project") {
		t.Error("packet must not reference a direct provider CLI (FAC-145 broker only)")
	}
	if strings.Contains(packet, "Do the thing") {
		t.Error("packet must NOT dump the card description inline (burns agent context)")
	}
	if !strings.Contains(packet, "herd verify") {
		t.Error("packet must include the self-verify completion contract")
	}
	// FAC-134 review finding #1: packet must never carry an absolute host
	// path (repo-specific or otherwise) — cwd is Herdr-enforced separately.
	if strings.Contains(packet, "chainseer") || strings.Contains(packet, "~/Personal") {
		t.Error("packet must not reference a hardcoded repo-specific path")
	}
	if strings.Contains(packet, "/tmp/") {
		t.Error("packet must not embed any absolute host path")
	}
	// herd verify's flags must precede the positional worktree arg: Go's
	// flag package stops parsing at the first non-flag token, so flags
	// placed after "." are silently ignored (FAC-134 latent bug).
	if !strings.Contains(packet, `herd verify --build "go build ./..." --test "go test ./..." .`) {
		t.Errorf("herd verify flags must precede the positional worktree arg:\n%s", packet)
	}
	if lines := strings.Count(packet, "\n"); lines > 25 {
		t.Errorf("packet must stay tight (<25 lines), got %d", lines)
	}
	// FAC-222: the packet must carry a reply target so agents report to the
	// coordinator instead of relying on the coordinator to notice by polling.
	if !strings.Contains(packet, "Report to the coordinator") {
		t.Error("packet must include the reply target section")
	}
	if !strings.Contains(packet, "herd shot FAC-33 --report complete") {
		t.Error("packet must tell agents to report completion via herd shot --report")
	}
	if !strings.Contains(packet, "herd shot FAC-33 --report blocked") {
		t.Error("packet must tell agents to report BLOCKED via herd shot --report")
	}
	if !strings.Contains(packet, "--lease 1") {
		t.Error("packet must carry the lease generation for callback binding")
	}
}

// TestBuildTaskPacket_ReplyTargetIsNonVacuous is non-vacuous: it proves the
// reply target name and lease generation are actually driven into the packet.
// A regression that hardcodes "coordinator" and lease 0 (or omits the
// reporting section entirely) breaks the named-coordinator case even though
// the default case would still pass.
func TestBuildTaskPacket_ReplyTargetIsNonVacuous(t *testing.T) {
	task := &provider.Task{Ref: "FAC-222", Title: "Reply target test"}
	lane := &config.LaneDef{Name: "worker", Prompt: ".herd/prompts/worker.md"}
	verification := config.Verification{TestCommand: "go test ./..."}

	t.Run("named coordinator and lease are embedded", func(t *testing.T) {
		packet := buildTaskPacket(task, "herd/fac-222", ".herd/prompts/worker.md", "kaneo", "fac-proj", lane, verification,
			ReplyTarget{Name: "forge-coordinator", ReviewSupervisor: "forge-review-harvest-supervisor", LeaseGeneration: 42})
		if !strings.Contains(packet, "forge-coordinator") {
			t.Errorf("packet must carry the named coordinator:\n%s", packet)
		}
		if !strings.Contains(packet, "--lease 42") {
			t.Errorf("packet must carry lease generation 42:\n%s", packet)
		}
		if !strings.Contains(packet, "forge-review-harvest-supervisor") {
			t.Errorf("packet must route review handoff to the supervisor:\n%s", packet)
		}
	})

	t.Run("empty name falls back to default", func(t *testing.T) {
		packet := buildTaskPacket(task, "herd/fac-222", ".herd/prompts/worker.md", "kaneo", "fac-proj", lane, verification,
			ReplyTarget{Name: "", LeaseGeneration: 7})
		if !strings.Contains(packet, "coordinator") {
			t.Errorf("packet must fall back to default coordinator name:\n%s", packet)
		}
		if !strings.Contains(packet, "--lease 7") {
			t.Errorf("packet must carry lease generation 7:\n%s", packet)
		}
	})
}

// TestBuildTaskPacket_ProviderNeutralTaskReference: FAC-145 — EVERY provider
// gets the receipt-gated broker reference; no provider ever gets a direct
// CLI reference that would ride ambient credentials outside the receipt.
func TestBuildTaskPacket_ProviderNeutralTaskReference(t *testing.T) {
	task := &provider.Task{Ref: "FAC-2", Title: "Task FAC-2"}
	lane := &config.LaneDef{Name: "worker", Prompt: ".herd/prompts/worker.md"}
	verification := config.Verification{TestCommand: "go test ./..."}

	for _, providerType := range []string{"kaneo", "github", "linear", "jira", "memory", ""} {
		t.Run(providerType+" provider uses the broker", func(t *testing.T) {
			packet := buildTaskPacket(task, "herd/fac-2", ".herd/prompts/worker.md", providerType, "fac-proj", lane, verification, ReplyTarget{Name: "coordinator", LeaseGeneration: 1})
			if !strings.Contains(packet, "herd task get FAC-2 --full") {
				t.Errorf("provider %q must reference the receipt-gated broker:\n%s", providerType, packet)
			}
			if strings.Contains(packet, "kaneo task get") || strings.Contains(packet, "--project") {
				t.Errorf("provider %q must not reference a direct provider CLI:\n%s", providerType, packet)
			}
			if !strings.Contains(packet, "FAC-2") {
				t.Errorf("packet must still reference the task ref:\n%s", packet)
			}
		})
	}
}

// TestBuildTaskPacket_RepositoryAgnosticVerification is non-vacuous: each
// case's config drives different verify commands into the packet, and each
// case asserts the *other* profiles' commands are ABSENT. Reintroducing a
// hardcoded `go build`/`go test` literal (FAC-134 regression) breaks the
// node and docs-only cases even though the go case would still pass.
func TestBuildTaskPacket_RepositoryAgnosticVerification(t *testing.T) {
	task := &provider.Task{Ref: "FAC-1", Title: "Task FAC-1"}
	lane := &config.LaneDef{Name: "worker", Prompt: ".herd/prompts/worker.md"}

	cases := []struct {
		name         string
		verification config.Verification
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "go profile",
			verification: config.Verification{TestCommand: "go test ./...", PreflightCommand: "go build ./..."},
			wantContains: []string{"go test ./...", "go build ./...", `--build "go build ./..."`, `--test "go test ./..."`},
			wantAbsent:   []string{"npm test", "pytest", "chainseer"},
		},
		{
			name:         "node profile",
			verification: config.Verification{TestCommand: "npm test", PreflightCommand: "npm run build"},
			wantContains: []string{"npm test", "npm run build"},
			wantAbsent:   []string{"go test ./...", "go build ./...", "go vet"},
		},
		{
			name:         "docs-only profile (no preflight command)",
			verification: config.Verification{TestCommand: "make lint-docs"},
			wantContains: []string{"make lint-docs", `--test "make lint-docs"`},
			wantAbsent:   []string{"go build", "go test", "go vet", "--build", "npm"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			packet := buildTaskPacket(task, "herd/fac-1", ".herd/prompts/worker.md", "kaneo", "fac-proj", lane, c.verification, ReplyTarget{Name: "coordinator", LeaseGeneration: 1})
			for _, want := range c.wantContains {
				if !strings.Contains(packet, want) {
					t.Errorf("packet missing %q:\n%s", want, packet)
				}
			}
			for _, absent := range c.wantAbsent {
				if strings.Contains(packet, absent) {
					t.Errorf("packet must not contain %q for this profile:\n%s", absent, packet)
				}
			}
		})
	}
}

// TestDispatch_PackageCwdNotPolluted runs the historical FAC-1 ticket path from
// the package directory and asserts zero ambient git/worktree side effects.
// Regression for: go test ./pkg/dispatch creating pkg/dispatch/.herd/worktrees/fac-1
// and local branch herd/fac-1 (FAC-121 R3 follow-up).
// FAC-145: dispatch must land a SIGNED provider context receipt (the sole
// authority file — no provider-native context is seeded) inside the spawned
// worktree, and must refuse to dispatch at all when the project binding is
// missing — before any worktree side effect.
func TestDispatch_PropagatesTaskContextIntoWorktree(t *testing.T) {
	tp := &mockTaskProvider{tasks: []*provider.Task{
		// Description carries the herd-deps-v1 provenance fence launch requires.
		{ID: "42", Ref: "FAC-9", Title: "Ninth task", Status: "to-do", Priority: "high",
			Description: emptyDepsFence("FAC-9", "42")},
	}}
	cfg := &config.Config{
		Project:      config.ProjectConfig{Name: "t", DefaultBranch: "main"},
		TaskProvider: config.TaskProvider{Type: "kaneo", ProjectID: "proj-live", WorkspaceID: "ws-live"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "m", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
		Verification: config.Verification{TestCommand: "go test ./..."},
	}
	tmpRepo, wm := initDispatchRepo(t)
	d := NewDispatcher(cfg, tp, wm)
	d.Compensator = &recordingCompensator{}
	d.Herdr = &fakeHerdr{available: false}
	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-9", NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", tmpRepo, "worktree", "remove", "--force", res.Worktree).Run()
		os.RemoveAll(res.Worktree)
	})

	data, err := os.ReadFile(filepath.Join(res.Worktree, TaskContextFile))
	if err != nil {
		t.Fatalf("receipt missing from worktree: %v", err)
	}
	var tc TaskContext
	if err := json.Unmarshal(data, &tc); err != nil {
		t.Fatalf("receipt not valid JSON: %v", err)
	}
	if tc.ProviderType != "kaneo" || tc.ProjectID != "proj-live" || tc.ProviderWorkspace != "ws-live" ||
		tc.TaskRef != "FAC-9" || tc.TaskID != "42" || tc.Branch != res.Branch {
		t.Errorf("receipt binding wrong: %+v", tc)
	}
	if _, err := os.Stat(filepath.Join(res.Worktree, ".kaneo.json")); !os.IsNotExist(err) {
		t.Error("no provider-native context may be seeded — the signed receipt is the sole authority (FAC-145)")
	}
	if tc.Repository != RepositoryIdentityOrName(tmpRepo, "t") || tc.Role != RoleWorker {
		t.Errorf("receipt identity wrong: repo=%q role=%q", tc.Repository, tc.Role)
	}
	if strings.TrimSpace(tc.LeaseID) == "" || tc.LeaseGeneration < 1 || tc.BaseSHA == "" {
		t.Errorf("receipt must carry fenceable lease + base identity: %+v", tc)
	}
	if !tc.ExpiresAt.After(time.Now()) {
		t.Errorf("receipt must carry a future expiry, got %s", tc.ExpiresAt)
	}
	for _, op := range tc.AllowedOps {
		if op == "mutate" {
			t.Error("agent receipt must never carry the mutate op (board moves stay coordinator-owned)")
		}
	}
	// The receipt is coordinator-signed and verifies against the repo's
	// published key.
	verifier, vErr := LoadVerifier(tmpRepo)
	if vErr != nil {
		t.Fatalf("published verification key missing after dispatch: %v", vErr)
	}
	if err := verifier.Verify(tc); err != nil {
		t.Fatalf("dispatched receipt must authenticate: %v", err)
	}
	packet, err := os.ReadFile(filepath.Join(res.Worktree, "TASK-PACKET.md"))
	if err != nil {
		t.Fatalf("packet missing: %v", err)
	}
	if !strings.Contains(string(packet), "herd task get FAC-9 --full") {
		t.Error("packet must direct the agent to the receipt-gated broker (FAC-145)")
	}

	// Missing project binding fails closed before any worktree side effect.
	mw := &mockWorktree{}
	cfgNoProj := *cfg
	cfgNoProj.TaskProvider.ProjectID = ""
	dBad := &Dispatcher{
		Config: &cfgNoProj, TaskProvider: tp, Worktree: mw,
		Compensator: &recordingCompensator{},
		Herdr:       &fakeHerdr{available: false},
	}
	if _, err := dBad.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-9", NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1}); err == nil {
		t.Fatal("empty project_id must fail closed")
	}
	if mw.calls != 0 {
		t.Errorf("fail-closed dispatch still created %d worktree(s)", mw.calls)
	}
}

// TestDispatchBootstrapFailureBlocksAgentLaunch is non-vacuous: before the
// FAC-245 boundary, this exact fixture reaches fakeHerdr.AgentStart. The
// bootstrap failure must retain attributable recovery context, compensate the
// owned dispatch, and prevent the first agent side effect.
func TestDispatchBootstrapFailureBlocksAgentLaunch(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-BOOT")}}}
	cfg := testCfg()
	cfg.WorktreeBootstrap = config.WorktreeBootstrap{Version: "v1", Toolchain: "go", Command: []string{"go", "mod", "download"}}
	comp := &recordingCompensator{}
	fh := &fakeHerdr{available: true, workspace: "bootstrap-test", model: "m"}
	bootstrap := &failingWorktreeBootstrap{err: errors.New("module mirror unavailable")}
	d := NewDispatcher(cfg, tp, wm)
	d.Compensator = comp
	d.Herdr = fh
	d.Bootstrap = bootstrap

	_, err := d.Dispatch(context.Background(), leasedLaunchOptions(t, "FAC-BOOT"))
	if err == nil || !strings.Contains(err.Error(), "module mirror unavailable") || !strings.Contains(err.Error(), "recovery:") {
		t.Fatalf("bootstrap failure must provide attributable recovery: %v", err)
	}
	if bootstrap.calls != 1 || bootstrap.path == "" {
		t.Fatalf("bootstrap calls/path = %d/%q, want 1/admitted worktree", bootstrap.calls, bootstrap.path)
	}
	if fh.startCalls != 0 || fh.tabCwd != "" {
		t.Fatalf("bootstrap failure crossed agent launch boundary: starts=%d tab=%q", fh.startCalls, fh.tabCwd)
	}
	if !hasCompensateReason(comp.compsCopy(), "worktree_bootstrap_failed") {
		t.Fatalf("bootstrap failure was not retained for recovery: %v", comp.compsCopy())
	}
}

func TestDispatch_PackageCwdNotPolluted(t *testing.T) {
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpRepo, wm := initDispatchRepo(t)
	otherPath := filepath.Join(tmpRepo, "unrelated")
	runDispatchGit(t, tmpRepo, "worktree", "add", "-b", "unrelated", otherPath, "main")
	before, err := wm.ListWorktrees(context.Background())
	if err != nil {
		t.Fatalf("list fixture worktrees before dispatch: %v", err)
	}

	advanceStart := make(chan struct{})
	advanceDone := make(chan error, 1)
	go func() {
		<-advanceStart
		if err := os.WriteFile(filepath.Join(otherPath, "unrelated.txt"), []byte("advanced\n"), 0644); err != nil {
			advanceDone <- err
			return
		}
		_, err := runDispatchGitOutput(otherPath, "add", "unrelated.txt")
		if err == nil {
			_, err = runDispatchGitOutput(otherPath, "commit", "-m", "fixture: advance unrelated worktree")
		}
		advanceDone <- err
	}()

	service := &synchronizedDispatchWorktree{
		manager: wm,
		start:   advanceStart,
		done:    advanceDone,
	}
	d := withTestLease(t, &Dispatcher{
		Config: testCfg(),
		TaskProvider: &mockTaskProvider{tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Title: "Task FAC-1", Status: "to-do", Description: emptyDepsFence("FAC-1", "1")},
		}},
		Worktree:    service,
		Compensator: &recordingCompensator{},
		Herdr:       &fakeHerdr{available: false},
	})
	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true, LeaseID: "claim:1", LeaseGeneration: 1})
	if err != nil {
		t.Fatalf("temp-repo dispatch: %v", err)
	}
	if service.advanceErr != nil {
		t.Fatalf("advance unrelated fixture worktree: %v", service.advanceErr)
	}
	if !strings.HasPrefix(res.Worktree, tmpRepo+string(filepath.Separator)) ||
		strings.Contains(res.Worktree, filepath.Join("pkg", "dispatch")) {
		t.Fatalf("worktree escaped isolated fixture repo: %s", res.Worktree)
	}
	after, err := wm.ListWorktrees(context.Background())
	if err != nil {
		t.Fatalf("list fixture worktrees after dispatch: %v", err)
	}
	beforeOther := worktreeByPath(before, otherPath)
	if beforeOther == nil {
		t.Fatalf("unrelated fixture worktree identity missing before dispatch: %s", otherPath)
	}
	if got := worktreeByPath(after, res.Worktree); got == nil || got.Branch != worktree.TaskBranch("FAC-1") {
		t.Fatalf("dispatch target identity missing or wrong: %+v", got)
	}
	if got := worktreeByPath(after, otherPath); got == nil || got.Commit == beforeOther.Commit {
		t.Fatalf("unrelated fixture worktree did not advance: before=%+v after=%+v", beforeOther, got)
	}
	if _, err := os.Stat(filepath.Join(pkgDir, ".herd")); err == nil {
		t.Fatalf("dispatch polluted package cwd: %s", filepath.Join(pkgDir, ".herd"))
	}
	if err := wm.RemoveWorktree(context.Background(), res.Worktree); err != nil {
		t.Fatalf("remove attributable dispatch worktree: %v", err)
	}
	if leaked := worktreeByPath(mustListDispatchWorktrees(t, wm), res.Worktree); leaked != nil {
		t.Fatalf("attributable dispatch worktree leaked: %+v", leaked)
	}
}

type synchronizedDispatchWorktree struct {
	manager    *worktree.WorktreeManager
	start      chan struct{}
	done       chan error
	advanceErr error
}

func (s *synchronizedDispatchWorktree) CreateTaskWorktreeFrom(ctx context.Context, ref, branch string) (*worktree.WorktreeInfo, error) {
	close(s.start)
	info, err := s.manager.CreateTaskWorktreeFrom(ctx, ref, branch)
	s.advanceErr = <-s.done
	if s.advanceErr != nil && err == nil {
		err = s.advanceErr
	}
	return info, err
}

func (s *synchronizedDispatchWorktree) RepoRoot() string { return s.manager.RepoRoot }

func worktreeByPath(worktrees []*worktree.WorktreeInfo, path string) *worktree.WorktreeInfo {
	for _, wt := range worktrees {
		if wt.Path == path || sameDispatchFixturePath(wt.Path, path) {
			return wt
		}
	}
	return nil
}

func sameDispatchFixturePath(left, right string) bool {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

func mustListDispatchWorktrees(t *testing.T, wm *worktree.WorktreeManager) []*worktree.WorktreeInfo {
	t.Helper()
	worktrees, err := wm.ListWorktrees(context.Background())
	if err != nil {
		t.Fatalf("list fixture worktrees: %v", err)
	}
	return worktrees
}

func runDispatchGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := runDispatchGitOutput(dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

func runDispatchGitOutput(dir string, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=commit.gpgsign",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=tag.gpgSign",
		"GIT_CONFIG_VALUE_1=false",
		"GIT_CONFIG_KEY_2=merge.verifySignatures",
		"GIT_CONFIG_VALUE_2=false",
		"GIT_CONFIG_KEY_3=core.hooksPath",
		"GIT_CONFIG_VALUE_3=.dispatch-disabled-hooks",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/usr/bin/false",
		"SSH_ASKPASS=/usr/bin/false",
	)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
