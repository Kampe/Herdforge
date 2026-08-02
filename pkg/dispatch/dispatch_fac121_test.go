package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// --- fakes -----------------------------------------------------------------

type recordingCompensator struct {
	mu    sync.Mutex
	steps []StepRecord
	comps []string
}

func (c *recordingCompensator) RecordStep(_ context.Context, rec StepRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.steps = append(c.steps, rec)
	return nil
}

func (c *recordingCompensator) Compensate(_ context.Context, ticketRef, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.comps = append(c.comps, ticketRef+":"+reason)
	return nil
}

func (c *recordingCompensator) stepsCopy() []StepRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]StepRecord, len(c.steps))
	copy(out, c.steps)
	return out
}

func (c *recordingCompensator) compsCopy() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.comps))
	copy(out, c.comps)
	return out
}

type fakeHerdr struct {
	mu sync.Mutex

	available bool
	workspace string
	wsErr     error

	tabCwd      string
	tabWS       string
	tabLabel    string
	tabErr      error
	tabID       string
	paneID      string
	startErr    error
	deliverErr  error
	deliverRec  *herdr.PromptReceipt
	closedTabs  []string
	startCalls  int
	deliverText string
	model       string
}

func (f *fakeHerdr) Available() bool { return f.available }
func (f *fakeHerdr) RequireWorkspace(string) (string, error) {
	if f.wsErr != nil {
		return "", f.wsErr
	}
	if f.workspace == "" {
		return "", fmt.Errorf("workspace unknown")
	}
	return f.workspace, nil
}
func (f *fakeHerdr) TabCreateForTask(workspaceID, label, cwd string, _ bool) (*herdr.TabInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tabWS = workspaceID
	f.tabLabel = label
	f.tabCwd = cwd
	if f.tabErr != nil {
		return nil, f.tabErr
	}
	id := f.tabID
	if id == "" {
		id = "tab-1"
	}
	pane := f.paneID
	if pane == "" {
		pane = "pane-1"
	}
	return &herdr.TabInfo{
		ID: id, Label: label, Cwd: cwd,
		Pane: herdr.PaneInfo{ID: pane, TabID: id},
	}, nil
}
func (f *fakeHerdr) AgentStart(name, kind, paneID string, _ ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	_ = name
	_ = kind
	_ = paneID
	return f.startErr
}
func (f *fakeHerdr) DeliverAndProve(target, text string, verify bool, _ time.Duration) (*herdr.PromptReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliverText = text
	if f.deliverErr != nil {
		return &herdr.PromptReceipt{Target: target, Consumed: false, Verified: verify}, f.deliverErr
	}
	if f.deliverRec != nil {
		return f.deliverRec, nil
	}
	return &herdr.PromptReceipt{
		Target: target, BaselineStatus: "idle", FinalStatus: "working",
		Consumed: true, Verified: verify, SequenceToken: "idle->working",
	}, nil
}
func (f *fakeHerdr) TabClose(tabID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedTabs = append(f.closedTabs, tabID)
	return nil
}
func (f *fakeHerdr) ResolveHealthyModel(_ context.Context, primary string, _ []string) (string, []herdr.ProbeResult) {
	if f.model != "" {
		return f.model, nil
	}
	if primary == "" {
		return "", nil
	}
	return primary, nil
}

type statusTrackingProvider struct {
	mockTaskProvider
	statuses []string
	comments []string
}

func (p *statusTrackingProvider) UpdateStatus(_ context.Context, _, status string) error {
	p.statuses = append(p.statuses, status)
	return nil
}
func (p *statusTrackingProvider) AddComment(_ context.Context, _, comment string) error {
	p.comments = append(p.comments, comment)
	return nil
}

// --- repo fixture ----------------------------------------------------------

func initDispatchRepo(t *testing.T) (repo string, wm *worktree.WorktreeManager) {
	t.Helper()
	repo = t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command(args[0], args[1:]...)
		c.Dir = repo
		c.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	run("git", "init")
	run("git", "config", "user.email", "t@t.com")
	run("git", "config", "user.name", "T")
	run("git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	run("git", "add", "README.md")
	run("git", "commit", "-m", "initial")
	run("git", "branch", "-M", "main")
	bare := filepath.Join(repo, ".origin.git")
	run("git", "init", "--bare", bare)
	run("git", "remote", "add", "origin", bare)
	run("git", "push", "-u", "origin", "main")
	return repo, worktree.NewWorktreeManager(repo)
}

func testCfg() *config.Config {
	return &config.Config{
		Project:      config.ProjectConfig{Name: "Herdforge", DefaultBranch: "main"},
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Lanes: []config.LaneDef{
			{Name: "worker", Role: "worker", Model: "deepseek-v4-flash", AgentKind: "opencode", Prompt: ".herd/prompts/worker.md"},
		},
	}
}

// --- tests -----------------------------------------------------------------

func TestDispatch_NoLaunch_UsesActualGitBranchAndImmutableBase(t *testing.T) {
	repo, wm := initDispatchRepo(t)
	originSHA := revParse(t, repo, "origin/main")

	// Make local main diverge so HEAD ≠ origin/main.
	if err := os.WriteFile(filepath.Join(repo, "local-only.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "local-only.txt")
	runGit(t, repo, "commit", "-m", "local ahead")

	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-1", Title: "Do the thing", Status: "to-do", Priority: "high"},
		}},
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Compensator = comp
	d.Herdr = &fakeHerdr{available: false}

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(res.Worktree) })

	if res.Branch != "herd/fac-1" {
		t.Fatalf("Branch = %q, want herd/fac-1", res.Branch)
	}
	if res.BaseSHA != originSHA {
		t.Fatalf("BaseSHA = %s want origin %s", res.BaseSHA, originSHA)
	}
	if res.AnchorRef != worktree.AnchorRefFor("FAC-1") {
		t.Fatalf("AnchorRef = %q", res.AnchorRef)
	}
	// Packet branch == Git branch
	body, _ := os.ReadFile(res.TaskPacket)
	if !strings.Contains(string(body), res.Branch) {
		t.Fatalf("packet missing actual branch %q", res.Branch)
	}
	if strings.Contains(string(body), "task/fac-1-") {
		t.Fatal("packet must not invent task/<slug> branch alias")
	}
	// Comment carries actual branch
	if len(tp.comments) == 0 || !strings.Contains(tp.comments[0], res.Branch) {
		t.Fatalf("comment must carry actual branch: %v", tp.comments)
	}
	// Compensator saw worktree + board steps
	steps := comp.stepsCopy()
	if len(steps) < 2 {
		t.Fatalf("expected recorded steps, got %v", steps)
	}
	if steps[0].Step != StepWorktree || steps[0].Branch != res.Branch {
		t.Fatalf("first step: %+v", steps[0])
	}
}

func TestDispatch_Launch_SetsCwdAndProvesPrompt(t *testing.T) {
	repo, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-9", Title: "Launch me", Status: "to-do"},
		}},
	}
	fh := &fakeHerdr{
		available: true,
		workspace: "wHerd",
		model:     "deepseek-v4-flash",
		tabID:     "tab-9",
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), DispatchOptions{
		TicketRef: "FAC-9", SkipPromptVerify: true,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(res.Worktree) })

	if !res.Launched {
		t.Fatal("expected Launched")
	}
	if fh.tabCwd != res.Worktree {
		t.Fatalf("herdr cwd = %q, want worktree %q", fh.tabCwd, res.Worktree)
	}
	if fh.tabWS != "wHerd" {
		t.Fatalf("workspace = %q", fh.tabWS)
	}
	if res.TabID != "tab-9" {
		t.Fatalf("TabID = %q", res.TabID)
	}
	if res.Receipt == nil || !res.Receipt.Consumed {
		t.Fatalf("receipt: %+v", res.Receipt)
	}
	// Integration: spawned process cwd equals assigned worktree
	if err := worktree.RejectSharedRoot(repo, fh.tabCwd); err != nil {
		t.Fatalf("launched cwd failed shared-root check: %v", err)
	}
}

func TestDispatch_CrashPoint_AgentStartClosesOrphanTab(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-7", Title: "Crash start", Status: "to-do"},
		}},
	}
	fh := &fakeHerdr{
		available: true,
		workspace: "w1",
		model:     "m",
		tabID:     "orphan-tab",
		startErr:  fmt.Errorf("agent_pane_busy forever"),
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-7"})
	if err == nil {
		t.Fatal("expected agent start failure")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if len(fh.closedTabs) != 1 || fh.closedTabs[0] != "orphan-tab" {
		t.Fatalf("expected orphan tab closed, got %v", fh.closedTabs)
	}
	comps := comp.compsCopy()
	found := false
	for _, c := range comps {
		if strings.Contains(c, "agent_start_failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected compensate agent_start_failed, got %v", comps)
	}
	if res != nil && res.Launched {
		t.Fatal("must not report Launched after start failure")
	}
}

func TestDispatch_CrashPoint_PromptFailureClosesTab(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-8", Title: "Prompt fail", Status: "to-do"},
		}},
	}
	fh := &fakeHerdr{
		available:  true,
		workspace:  "w1",
		model:      "m",
		tabID:      "tab-prompt",
		deliverErr: fmt.Errorf("never confirmed consumption"),
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-8"})
	if err == nil {
		t.Fatal("expected prompt failure")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if len(fh.closedTabs) != 1 || fh.closedTabs[0] != "tab-prompt" {
		t.Fatalf("expected tab closed after prompt fail: %v", fh.closedTabs)
	}
	comps := comp.compsCopy()
	found := false
	for _, c := range comps {
		if strings.Contains(c, "prompt_delivery_failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected prompt_delivery_failed compensate: %v", comps)
	}
}

func TestDispatch_UnknownWorkspaceFailsClosed(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{
			{ID: "1", Ref: "FAC-3", Title: "No ws", Status: "to-do"},
		}},
	}
	fh := &fakeHerdr{
		available: true,
		wsErr:     fmt.Errorf("herdr workspace unknown: refusing hardcoded fallback"),
		model:     "m",
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-3"})
	if err == nil {
		t.Fatal("expected workspace failure")
	}
	if !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("error: %v", err)
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if fh.tabCwd != "" {
		t.Fatal("must not create tab when workspace unknown")
	}
}

func TestDispatch_RejectsSharedRootLaunch(t *testing.T) {
	// Unit-level: RejectSharedRoot is enforced inside launch with real paths.
	repo, wm := initDispatchRepo(t)
	// Force a malicious worktree manager path equal to repo root via attach path.
	// Dispatch uses CreateTaskWorktreeFrom which cannot produce root path for
	// normal refs; assert the gate directly for the launch path contract.
	if err := worktree.RejectSharedRoot(repo, repo); err == nil {
		t.Fatal("shared root must be denied")
	}
	_ = wm
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func revParse(t *testing.T, dir, rev string) string {
	t.Helper()
	c := exec.Command("git", "rev-parse", rev)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse %s: %v (%s)", rev, err, out)
	}
	return strings.TrimSpace(string(out))
}
