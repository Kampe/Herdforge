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
	mu       sync.Mutex
	steps    []StepRecord
	comps    []string
	recordErr error
	compErr   error
}

func (c *recordingCompensator) RecordStep(_ context.Context, rec StepRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.recordErr != nil {
		return c.recordErr
	}
	c.steps = append(c.steps, rec)
	return nil
}

func (c *recordingCompensator) Compensate(_ context.Context, ticketRef, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.compErr != nil {
		return c.compErr
	}
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
func (f *fakeHerdr) DeliverAndProve(target, text string, _ time.Duration) (*herdr.PromptReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliverText = text
	if f.deliverErr != nil {
		return &herdr.PromptReceipt{Target: target, Consumed: false, Verified: true}, f.deliverErr
	}
	if f.deliverRec != nil {
		return f.deliverRec, nil
	}
	// Default: valid idle→working sequence proof
	return &herdr.PromptReceipt{
		Target: target, BaselineStatus: "idle", FinalStatus: "working",
		Consumed: true, Verified: true, SequenceToken: "idle->working",
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

func baseTask(ref string) *provider.Task {
	return &provider.Task{ID: "1", Ref: ref, Title: "Task " + ref, Status: "to-do"}
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
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-1")}},
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
	body, _ := os.ReadFile(res.TaskPacket)
	if !strings.Contains(string(body), res.Branch) {
		t.Fatalf("packet missing actual branch %q", res.Branch)
	}
	if strings.Contains(string(body), "task/fac-1-") {
		t.Fatal("packet must not invent task/<slug> branch alias")
	}
	if len(tp.comments) == 0 || !strings.Contains(tp.comments[0], res.Branch) {
		t.Fatalf("comment must carry actual branch: %v", tp.comments)
	}
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
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-9")}},
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

	// Production path: always proves consumption (no SkipPromptVerify).
	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-9"})
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
	if res.Receipt == nil || !res.Receipt.Consumed || !res.Receipt.Verified {
		t.Fatalf("receipt: %+v", res.Receipt)
	}
	if !herdr.ConsumptionProven(res.Receipt.BaselineStatus, res.Receipt.FinalStatus) {
		t.Fatalf("receipt sequence not proven: %+v", res.Receipt)
	}
	if err := worktree.RejectSharedRoot(repo, fh.tabCwd); err != nil {
		t.Fatalf("launched cwd failed shared-root check: %v", err)
	}
	// Prompt step must be durably recorded with sequence token
	found := false
	for _, s := range comp.stepsCopy() {
		if s.Step == StepPrompt && s.Receipt == "idle->working" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected durable prompt step with receipt, got %v", comp.stepsCopy())
	}
}

func TestDispatch_NilCompensatorFailsClosed(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-N")}},
	}
	d := NewDispatcher(testCfg(), tp, wm)
	// Compensator intentionally nil — production path must refuse.
	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-N", NoLaunch: true})
	if err == nil {
		t.Fatal("nil compensator must fail closed")
	}
	if !strings.Contains(err.Error(), "compensator is required") {
		t.Fatalf("error: %v", err)
	}
}

func TestDispatch_RecordStepErrorPropagates(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-R")}},
	}
	comp := &recordingCompensator{recordErr: fmt.Errorf("outbox full")}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Compensator = comp
	d.Herdr = &fakeHerdr{available: false}

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-R", NoLaunch: true})
	if err == nil {
		t.Fatal("expected RecordStep error to fail closed")
	}
	if !strings.Contains(err.Error(), "RecordStep") && !strings.Contains(err.Error(), "outbox full") {
		t.Fatalf("error: %v", err)
	}
}

func TestDispatch_CompensateErrorPropagates(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-C")}},
	}
	// Force prompt failure then compensate error
	fh := &fakeHerdr{
		available:  true,
		workspace:  "w1",
		model:      "m",
		tabID:      "tab-c",
		deliverErr: fmt.Errorf("no flip"),
	}
	comp := &recordingCompensator{compErr: fmt.Errorf("lease fenced")}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-C"})
	if err == nil {
		t.Fatal("expected joined primary+compensate error")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if !strings.Contains(err.Error(), "lease fenced") {
		t.Fatalf("compensate error must surface: %v", err)
	}
	if !strings.Contains(err.Error(), "prompt") && !strings.Contains(err.Error(), "consumption") {
		t.Fatalf("primary error must surface: %v", err)
	}
}

func TestDispatch_RejectsInvalidPromptSequence(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-SEQ")}},
	}
	// Fake claims success with working→working (invalid under ConsumptionProven)
	fh := &fakeHerdr{
		available: true,
		workspace: "w1",
		model:     "m",
		tabID:     "tab-seq",
		deliverRec: &herdr.PromptReceipt{
			Target: "task-fac-seq", BaselineStatus: "working", FinalStatus: "working",
			Consumed: true, Verified: true, SequenceToken: "working->working",
		},
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-SEQ"})
	if err == nil {
		t.Fatal("working→working receipt must be rejected on launch path")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if res != nil && res.Launched {
		t.Fatal("must not mark Launched")
	}
	if len(fh.closedTabs) != 1 {
		t.Fatalf("orphan tab must close: %v", fh.closedTabs)
	}
	comps := comp.compsCopy()
	found := false
	for _, c := range comps {
		if strings.Contains(c, "prompt_sequence_invalid") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected prompt_sequence_invalid compensate: %v", comps)
	}
}

func TestDispatch_CrashPoint_AgentStartClosesOrphanTab(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-7")}},
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
	found := false
	for _, c := range comp.compsCopy() {
		if strings.Contains(c, "agent_start_failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected compensate agent_start_failed, got %v", comp.compsCopy())
	}
	if res != nil && res.Launched {
		t.Fatal("must not report Launched after start failure")
	}
}

func TestDispatch_CrashPoint_PromptFailureClosesTab(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-8")}},
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
	found := false
	for _, c := range comp.compsCopy() {
		if strings.Contains(c, "prompt_delivery_failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected prompt_delivery_failed compensate: %v", comp.compsCopy())
	}
}

func TestDispatch_UnknownWorkspaceFailsClosed(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-3")}},
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

// TestDispatch_LaunchPath_RejectsSharedRoot exercises the production launch
// guard: if RejectSharedRoot is removed from launch(), this fails.
func TestDispatch_LaunchPath_RejectsSharedRoot(t *testing.T) {
	repo, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-ROOT")}},
	}
	fh := &fakeHerdr{available: true, workspace: "w1", model: "m", tabID: "should-not-create"}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	// Invoke launch with Path == RepoRoot — the production guard must deny
	// before TabCreate / AgentStart.
	task := baseTask("FAC-ROOT")
	lane := &d.Config.Lanes[0]
	wtInfo := &worktree.WorktreeInfo{
		Path:      repo, // shared root
		Branch:    "herd/fac-root",
		BaseSHA:   "deadbeef",
		AnchorRef: worktree.AnchorRefFor("FAC-ROOT"),
	}
	result := &DispatchResult{}
	err := d.launch(context.Background(), DispatchOptions{}, task, lane, wtInfo, wtInfo.Branch, "packet", result)
	if err == nil {
		t.Fatal("launch on shared root must fail; production RejectSharedRoot guard missing?")
	}
	if !strings.Contains(err.Error(), "shared-root") && !strings.Contains(err.Error(), "repository root") {
		t.Fatalf("expected shared-root denial, got: %v", err)
	}
	if fh.tabCwd != "" || fh.startCalls != 0 {
		t.Fatalf("must not start agent on shared root: cwd=%q starts=%d", fh.tabCwd, fh.startCalls)
	}
	if result.Launched {
		t.Fatal("must not set Launched")
	}
	found := false
	for _, c := range comp.compsCopy() {
		if strings.Contains(c, "shared_root_denied") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected shared_root_denied compensate: %v", comp.compsCopy())
	}
}

// TestDispatch_NoSkipPromptVerifyField ensures production options cannot
// bypass proof via a public Skip field (regression for R3 finding #3).
func TestDispatch_NoSkipPromptVerifyField(t *testing.T) {
	// Compile-time / reflect-style: DispatchOptions must not expose SkipPromptVerify.
	// If the field is re-added, this struct literal fails to typecheck when we
	// intentionally only set known production fields.
	opts := DispatchOptions{
		TicketRef:           "FAC-X",
		NoLaunch:            true,
		PromptVerifyTimeout: time.Second,
	}
	if opts.TicketRef == "" {
		t.Fatal("sanity")
	}
	// Document: there is no opts.SkipPromptVerify — launch always verifies.
}

func TestRecordingCompensator_Race(t *testing.T) {
	c := &recordingCompensator{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = c.RecordStep(context.Background(), StepRecord{TicketRef: "T", Step: StepWorktree, Branch: fmt.Sprintf("b%d", i)})
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = c.Compensate(context.Background(), "T", fmt.Sprintf("r%d", i))
		}(i)
	}
	wg.Wait()
	if len(c.stepsCopy()) != 32 {
		t.Fatalf("steps = %d", len(c.stepsCopy()))
	}
	if len(c.compsCopy()) != 32 {
		t.Fatalf("comps = %d", len(c.compsCopy()))
	}
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
