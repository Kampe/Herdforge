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
	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true})
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
