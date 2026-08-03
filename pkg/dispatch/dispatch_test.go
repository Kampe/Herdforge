package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/worktree"
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

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true})
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

	_, err = d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-NONEXIST", NoLaunch: true})
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

func TestDispatchRejectsMissingDecisionBeforeAnyProviderOrWorktreeMutation(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(t.TempDir(), "receipts.jsonl"))
	tp := &mockTaskProvider{tasks: []*provider.Task{{ID: "1", Ref: "FAC-175"}}}
	mw := &mockWorktree{err: fmt.Errorf("must not be called")}
	d := &Dispatcher{Config: &config.Config{TaskProvider: config.TaskProvider{ProjectID: "p"}, Lanes: []config.LaneDef{{Name: "worker"}}}, TaskProvider: tp, Worktree: mw, Compensator: &recordingCompensator{}}
	if _, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-175"}); err == nil {
		t.Fatal("missing routed decision must fail closed")
	}
	if mw.calls != 0 {
		t.Fatalf("rejected launch created worktree: %d", mw.calls)
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
	packet := buildTaskPacket(task, "herd/fac-33", ".herd/prompts/worker.md", "kaneo", lane, verification)
	if !strings.Contains(packet, "FAC-33") {
		t.Error("packet should contain ticket ref")
	}
	if !strings.Contains(packet, "herd/fac-33") {
		t.Error("packet must carry the actual Git branch name")
	}
	// FAC-115: reference-based, NOT an inline spec dump — the agent reads the
	// card itself, and the packet must be tight (context-budget fix).
	if !strings.Contains(packet, "kaneo task get FAC-33") {
		t.Error("packet must tell the agent to read the card by reference")
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
}

// TestBuildTaskPacket_ProviderNeutralTaskReference is non-vacuous: it proves
// the "read the full spec" step only names the `kaneo` CLI when kaneo is the
// configured task provider. A regression that unconditionally emits `kaneo
// task get` (FAC-134 review finding #2) breaks the non-kaneo case even
// though the kaneo case would still pass.
func TestBuildTaskPacket_ProviderNeutralTaskReference(t *testing.T) {
	task := &provider.Task{Ref: "FAC-2", Title: "Task FAC-2"}
	lane := &config.LaneDef{Name: "worker", Prompt: ".herd/prompts/worker.md"}
	verification := config.Verification{TestCommand: "go test ./..."}

	t.Run("kaneo provider gets the kaneo CLI reference", func(t *testing.T) {
		packet := buildTaskPacket(task, "herd/fac-2", ".herd/prompts/worker.md", "kaneo", lane, verification)
		if !strings.Contains(packet, "kaneo task get FAC-2 --full") {
			t.Errorf("kaneo provider must reference the kaneo CLI:\n%s", packet)
		}
	})

	for _, providerType := range []string{"github", "linear", "jira", "memory", ""} {
		t.Run(providerType+" provider does not assume kaneo", func(t *testing.T) {
			packet := buildTaskPacket(task, "herd/fac-2", ".herd/prompts/worker.md", providerType, lane, verification)
			if strings.Contains(packet, "kaneo task get") || strings.Contains(packet, "kaneo") {
				t.Errorf("provider %q must not assume ambient kaneo credentials:\n%s", providerType, packet)
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
			packet := buildTaskPacket(task, "herd/fac-1", ".herd/prompts/worker.md", "kaneo", lane, c.verification)
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
func TestDispatch_PackageCwdNotPolluted(t *testing.T) {
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Ambient git root (may be the shared Herdforge worktree when tests run in-package).
	ambientRoot := findGitRoot(t, pkgDir)
	before := snapshotAmbientGit(t, ambientRoot, pkgDir)

	tp := &mockTaskProvider{
		tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Title: "First task", Status: "to-do", Priority: "high", Description: emptyDepsFence("FAC-1", "1")},
		},
	}
	cfg := &config.Config{
		Project:      config.ProjectConfig{Name: "t", DefaultBranch: "main"},
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "m", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
		Verification: config.Verification{TestCommand: "go test ./...", PreflightCommand: "go build ./..."},
	}
	mw := &mockWorktree{err: fmt.Errorf("mock isolation: no ambient git")}
	d := withTestLease(t, &Dispatcher{
		Config: cfg, TaskProvider: tp, Worktree: mw,
		Compensator: &recordingCompensator{},
		Herdr:       &fakeHerdr{available: false},
	})
	_, _ = d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true})

	// Also exercise temp-repo path used by integration tests (must not leak to ambient).
	tmpRepo, wm := initDispatchRepo(t)
	d2 := NewDispatcher(cfg, tp, wm)
	d2.Compensator = &recordingCompensator{}
	d2.Herdr = &fakeHerdr{available: false}
	res, err := d2.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true})
	if err != nil {
		t.Fatalf("temp-repo dispatch: %v", err)
	}
	// Temp worktree must live under the temp repo, never under package cwd.
	if !strings.HasPrefix(res.Worktree, tmpRepo) {
		t.Fatalf("worktree %q not under temp repo %q", res.Worktree, tmpRepo)
	}
	if strings.Contains(res.Worktree, filepath.Join("pkg", "dispatch")) {
		t.Fatalf("worktree leaked into package tree: %s", res.Worktree)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "-C", tmpRepo, "worktree", "remove", "--force", res.Worktree).Run()
		os.RemoveAll(res.Worktree)
	})

	after := snapshotAmbientGit(t, ambientRoot, pkgDir)
	if after.worktreeList != before.worktreeList {
		t.Fatalf("ambient git worktree list changed:\n before:\n%s\n after:\n%s", before.worktreeList, after.worktreeList)
	}
	if after.branchFac1 != before.branchFac1 || after.anchorFac1 != before.anchorFac1 {
		t.Fatalf("ambient herd/fac-1 branch/anchor changed (before branch=%v anchor=%v after branch=%v anchor=%v)",
			before.branchFac1, before.anchorFac1, after.branchFac1, after.anchorFac1)
	}
	if after.pkgHerdExists != before.pkgHerdExists {
		t.Fatalf("package .herd pollution changed: before=%v after=%v path=%s",
			before.pkgHerdExists, after.pkgHerdExists, filepath.Join(pkgDir, ".herd"))
	}
	if after.dirty != before.dirty {
		t.Fatalf("ambient working tree dirtied by dispatch tests:\n before=%q\n after=%q", before.dirty, after.dirty)
	}
}

type ambientSnap struct {
	worktreeList  string
	branchFac1    bool
	anchorFac1    bool
	pkgHerdExists bool
	dirty         string
}

func snapshotAmbientGit(t *testing.T, ambientRoot, pkgDir string) ambientSnap {
	t.Helper()
	s := ambientSnap{}
	if ambientRoot != "" {
		s.worktreeList = gitOutAmbient(ambientRoot, "worktree", "list", "--porcelain")
		s.branchFac1 = gitOutAmbient(ambientRoot, "show-ref", "--verify", "--quiet", "refs/heads/herd/fac-1") == "ok"
		s.anchorFac1 = gitOutAmbient(ambientRoot, "show-ref", "--verify", "--quiet", "refs/herd/anchors/fac-1") == "ok"
		s.dirty = gitOutAmbient(ambientRoot, "status", "--porcelain")
	}
	if _, err := os.Stat(filepath.Join(pkgDir, ".herd")); err == nil {
		s.pkgHerdExists = true
	}
	// Also flag the historical pollution path specifically.
	if _, err := os.Stat(filepath.Join(pkgDir, ".herd", "worktrees", "fac-1")); err == nil {
		s.pkgHerdExists = true
	}
	return s
}

func findGitRoot(t *testing.T, start string) string {
	t.Helper()
	c := exec.Command("git", "rev-parse", "--show-toplevel")
	c.Dir = start
	out, err := c.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitOutAmbient runs git in ambientRoot. For --quiet verify, returns "ok" on success.
func gitOutAmbient(dir string, args ...string) string {
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := c.CombinedOutput()
	if len(args) >= 1 && args[0] == "show-ref" {
		if err == nil {
			return "ok"
		}
		return ""
	}
	if err != nil {
		return strings.TrimSpace(string(out))
	}
	return strings.TrimSpace(string(out))
}
