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
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type mockTaskProvider struct {
	tasks []*provider.Task
}

func (m *mockTaskProvider) GetTask(_ context.Context, id string) (*provider.Task, error) {
	for _, t := range m.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, nil
}
func (m *mockTaskProvider) ListTasks(_ context.Context, _, _ string) ([]*provider.Task, error) {
	return m.tasks, nil
}

func (m *mockTaskProvider) ClaimTask(_ context.Context, _, _ string) error { return nil }
func (m *mockTaskProvider) UpdateStatus(_ context.Context, _, _ string) error {
	return nil
}
func (m *mockTaskProvider) AddComment(_ context.Context, _, _ string) error { return nil }

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

func TestFindTicket(t *testing.T) {
	tp := &mockTaskProvider{
		tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Title: "First task", Status: "to-do", Priority: "high"},
			{ID: "2", Ref: "FAC-2", Title: "Second task", Status: "to-do", Priority: "medium"},
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
	d := &Dispatcher{
		Config:       cfg,
		TaskProvider: tp,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
	}

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
	packet := buildTaskPacket(task, "/tmp/wt", "herd/fac-33", ".herd/prompts/worker.md", lane)
	if !strings.Contains(packet, "FAC-33") {
		t.Error("packet should contain ticket ref")
	}
	if !strings.Contains(packet, "/tmp/wt") {
		t.Error("packet should contain worktree path")
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
	if lines := strings.Count(packet, "\n"); lines > 25 {
		t.Errorf("packet must stay tight (<25 lines), got %d", lines)
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
			{ID: "1", Ref: "FAC-1", Title: "First task", Status: "to-do", Priority: "high"},
		},
	}
	cfg := &config.Config{
		Project:      config.ProjectConfig{Name: "t", DefaultBranch: "main"},
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "m", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
	}
	mw := &mockWorktree{err: fmt.Errorf("mock isolation: no ambient git")}
	d := &Dispatcher{
		Config: cfg, TaskProvider: tp, Worktree: mw,
		Compensator: &recordingCompensator{},
		Herdr:       &fakeHerdr{available: false},
	}
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
	worktreeList string
	branchFac1   bool
	anchorFac1   bool
	pkgHerdExists bool
	dirty        string
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
