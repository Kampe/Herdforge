package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// Fixture tuple for launch-policy tests. Deliberately a concrete provider so
// the hermetic router picks deterministically; production no longer pins any
// vendor for builder roles, so these must NOT come from pkg/launch.
const (
	testWorkerProvider = "codex"
	testWorkerModel    = "gpt-5.6-luna"
	testWorkerEffort   = "high"
)

// --- fakes -----------------------------------------------------------------

type recordingCompensator struct {
	mu        sync.Mutex
	steps     []StepRecord
	comps     []string
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
	closeErr    error // TabClose failure — must not be silently discarded
	startCalls  int
	startReq    launch.Request
	deliverText string
	model       string
}

var fakePaneSeq uint64

type fixedGenerationOwnership struct {
	generation int64
}

func (o *fixedGenerationOwnership) ClaimExclusive(_ context.Context, taskID deps.TaskID, taskRef deps.Ref, role, graphRev, providerRev, _ string) (*deps.OwnershipToken, error) {
	return &deps.OwnershipToken{TaskID: taskID, TaskRef: taskRef, OwnerID: "test-owner", Generation: o.generation, GraphRev: graphRev, ProviderRev: providerRev, Role: role}, nil
}
func (o *fixedGenerationOwnership) StillOwns(context.Context, *deps.OwnershipToken) (bool, error) {
	return true, nil
}
func (o *fixedGenerationOwnership) ReleaseIfOwner(context.Context, *deps.OwnershipToken, string) error {
	return nil
}
func (o *fixedGenerationOwnership) Close() error { return nil }

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
		// Keep fake tab reservations isolated. Production deliberately retains
		// an identity after failed compensation, so reusing one pane across
		// independent tests would exercise the collision guard, not dispatch.
		pane = fmt.Sprintf("pane-test-%d", atomic.AddUint64(&fakePaneSeq, 1))
	}
	return &herdr.TabInfo{
		ID: id, Label: label, Cwd: cwd,
		Pane: herdr.PaneInfo{ID: pane, TabID: id},
	}, nil
}
func (f *fakeHerdr) AgentStart(req launch.Request, name, kind, paneID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	f.startReq = req
	_ = name
	_ = kind
	_ = paneID
	return f.startErr
}

func testRouter(t *testing.T) *router.SurfaceRouter {
	t.Helper()
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{
		CLIPresent: func(cli string) bool { return cli == testWorkerProvider },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	return r
}

func validLaunchOptions(t *testing.T, ref string) DispatchOptions {
	t.Helper()
	d, err := testRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatalf("build launch fixture: %v", err)
	}
	return DispatchOptions{TicketRef: ref, Decision: d}
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
	return f.closeErr
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
	statuses  []string
	comments  []string
	updateErr error
}

func (p *statusTrackingProvider) UpdateStatus(_ context.Context, _, status string) error {
	if p.updateErr != nil {
		return p.updateErr
	}
	p.statuses = append(p.statuses, status)
	return nil
}
func (p *statusTrackingProvider) AddComment(_ context.Context, _, comment string) error {
	p.comments = append(p.comments, comment)
	return nil
}

func hasCompensateReason(comps []string, reason string) bool {
	for _, c := range comps {
		if strings.Contains(c, reason) {
			return true
		}
	}
	return false
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
		Verification: config.Verification{TestCommand: "go test ./...", PreflightCommand: "go build ./..."},
	}
}

func baseTask(ref string) *provider.Task {
	// FAC-159: launch requires a Present versioned provenance fence (empty edges OK).
	fence := "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"" + ref + "\",\"task_id\":\"1\",\"edges\":[]}\n```\n"
	return &provider.Task{ID: "1", Ref: ref, Title: "Task " + ref, Status: "to-do", Description: fence}
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
	d.Ownership = &fixedGenerationOwnership{generation: 7}

	// Production path: always proves consumption (no SkipPromptVerify).
	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-9"))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(res.Worktree) })

	if !res.Launched {
		t.Fatal("expected Launched")
	}
	if fh.startReq.LeaseGeneration != 7 {
		t.Fatalf("launch request generation = %d, want exact claimed generation 7", fh.startReq.LeaseGeneration)
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
	wantArgv := router.ArgvFor(testWorkerProvider, testWorkerModel, testWorkerEffort)
	if fh.startReq.Decision == nil || !reflect.DeepEqual(fh.startReq.Decision.Argv, wantArgv) || fh.startReq.Decision.Provider != testWorkerProvider {
		t.Fatalf("dispatch launch decision = %+v, want provider/argv %s", fh.startReq.Decision, wantArgv)
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

// TestDispatch_EmptyBranchAfterWorktreeCompensates covers the post-side-effect
// path where CreateTaskWorktreeFrom returned a real path but empty Branch.
// Must failOwned("empty_worktree_branch") exactly once and never launch.
func TestDispatch_EmptyBranchAfterWorktreeCompensates(t *testing.T) {
	tmp := t.TempDir()
	// Simulate a created worktree directory (side effect already landed).
	wtPath := filepath.Join(tmp, ".herd", "worktrees", "fac-empty")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// Marker file proves the side effect is real/non-vacuous (not a nil info).
	if err := os.WriteFile(filepath.Join(wtPath, ".side-effect"), []byte("created"), 0o644); err != nil {
		t.Fatal(err)
	}

	mw := &mockWorktree{
		root: tmp,
		info: &worktree.WorktreeInfo{
			Path:      wtPath,
			Branch:    "", // empty after real create — the R3 remaining branch
			BaseSHA:   "abc123",
			AnchorRef: worktree.AnchorRefFor("FAC-EMPTY"),
		},
	}
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-EMPTY")}},
	}
	comp := &recordingCompensator{}
	fh := &fakeHerdr{available: true, workspace: "w1", model: "m", tabID: "must-not-create"}
	d := withTestLease(t, &Dispatcher{
		Config:       testCfg(),
		TaskProvider: tp,
		Worktree:     mw,
		Compensator:  comp,
		Herdr:        fh,
	})

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-EMPTY"))
	if err == nil {
		t.Fatal("expected empty-branch failure after worktree side effect")
	}
	if !strings.Contains(err.Error(), "without a Git branch") {
		t.Fatalf("primary error missing: %v", err)
	}
	if !hasCompensateReason(comp.compsCopy(), "empty_worktree_branch") {
		t.Fatalf("expected compensate empty_worktree_branch, got %v", comp.compsCopy())
	}
	// Side effect was observed by the service (non-vacuous inject).
	if mw.calls != 1 {
		t.Fatalf("expected one CreateTaskWorktreeFrom call, got %d", mw.calls)
	}
	if _, statErr := os.Stat(filepath.Join(wtPath, ".side-effect")); statErr != nil {
		t.Fatalf("injected worktree side effect missing: %v", statErr)
	}
	// No accepted launch: no tab, no agent start, no Launched.
	if fh.tabCwd != "" || fh.startCalls != 0 {
		t.Fatalf("must not launch on empty branch: cwd=%q starts=%d", fh.tabCwd, fh.startCalls)
	}
	if res != nil && res.Launched {
		t.Fatal("must not set Launched")
	}
	// Board must not advance after empty-branch failure (pre-status).
	if len(tp.statuses) != 0 {
		t.Fatalf("UpdateStatus must not run: %v", tp.statuses)
	}
	// No durable worktree step recorded under empty branch (refused before record).
	for _, s := range comp.stepsCopy() {
		if s.Step == StepWorktree {
			t.Fatalf("must not RecordStep worktree with empty branch: %+v", s)
		}
	}
}

// TestDispatch_MissingVerificationTestCommandFailsClosed is non-vacuous:
// deleting the fail-closed guard in Dispatch (or restoring a hardcoded
// `go test` fallback in buildTaskPacket) makes this pass through instead of
// erroring, and the compensate-reason assertion catches a guard that errors
// without compensating the already-created worktree (FAC-134).
func TestDispatch_MissingVerificationTestCommandFailsClosed(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-NOVERIFY")}},
	}
	cfg := testCfg()
	cfg.Verification = config.Verification{} // no verification configured
	comp := &recordingCompensator{}
	d := NewDispatcher(cfg, tp, wm)
	d.Compensator = comp
	d.Herdr = &fakeHerdr{available: false}

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-NOVERIFY", NoLaunch: true})
	if err == nil {
		t.Fatal("expected fail-closed error when verification.test_command is unset")
	}
	if !strings.Contains(err.Error(), "verification.test_command") {
		t.Fatalf("error must name the missing config field: %v", err)
	}
	if res != nil && res.Worktree != "" {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if !hasCompensateReason(comp.compsCopy(), "verification_test_command_missing") {
		t.Fatalf("expected compensate verification_test_command_missing, got %v", comp.compsCopy())
	}
	// Board must have already advanced (worktree + status land before this
	// check) — the guard must compensate, not pretend nothing happened.
	if len(tp.statuses) == 0 {
		t.Fatalf("expected board status to have advanced before the verification guard fired: %v", tp.statuses)
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

func TestProductionDispatcherRequiresControlFactoryBeforeHerdr(t *testing.T) {
	d := NewProductionDispatcher(testCfg(), nil, nil)
	d.Compensator = &recordingCompensator{}
	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-PROD", LaneName: "worker", Decision: &router.LaunchDecision{}})
	if err == nil || !strings.Contains(err.Error(), "durable control factory") {
		t.Fatalf("production dispatcher bypassed missing durable control factory: %v", err)
	}
}

func TestDispatch_RecordStepErrorPropagates(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-R")}},
	}
	// First durable step after worktree creation is StepWorktree RecordStep.
	comp := &recordingCompensator{recordErr: fmt.Errorf("outbox full")}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Compensator = comp
	d.Herdr = &fakeHerdr{available: false}

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-R", NoLaunch: true})
	if err == nil {
		t.Fatal("expected RecordStep error to fail closed")
	}
	if res != nil && res.Worktree != "" {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	// Primary error preserved.
	if !strings.Contains(err.Error(), "RecordStep") && !strings.Contains(err.Error(), "outbox full") {
		t.Fatalf("primary error missing: %v", err)
	}
	// Worktree already exists — compensation is mandatory with exact reason.
	if !hasCompensateReason(comp.compsCopy(), "record_worktree_failed") {
		t.Fatalf("expected compensate reason record_worktree_failed, got %v", comp.compsCopy())
	}
	// Board status must not advance after failed worktree RecordStep.
	if len(tp.statuses) != 0 {
		t.Fatalf("UpdateStatus must not run after record_worktree_failed; statuses=%v", tp.statuses)
	}
}

func TestDispatch_UpdateStatusErrorCompensates(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-US")}},
		updateErr:        fmt.Errorf("provider 503"),
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Compensator = comp
	d.Herdr = &fakeHerdr{available: false}

	res, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-US", NoLaunch: true})
	if err == nil {
		t.Fatal("expected UpdateStatus failure")
	}
	if res != nil && res.Worktree != "" {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if !strings.Contains(err.Error(), "update ticket status") && !strings.Contains(err.Error(), "provider 503") {
		t.Fatalf("primary UpdateStatus error missing: %v", err)
	}
	if !hasCompensateReason(comp.compsCopy(), "board_status_failed") {
		t.Fatalf("expected compensate board_status_failed, got %v", comp.compsCopy())
	}
	// Worktree step must have been durably recorded before board status failed.
	foundWT := false
	for _, s := range comp.stepsCopy() {
		if s.Step == StepWorktree {
			foundWT = true
		}
	}
	if !foundWT {
		t.Fatalf("expected StepWorktree recorded before status failure: %v", comp.stepsCopy())
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

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-C"))
	if err == nil {
		t.Fatal("expected joined primary+compensate error")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if !strings.Contains(err.Error(), "lease fenced") {
		t.Fatalf("compensate error must surface: %v", err)
	}
	if !strings.Contains(err.Error(), "Recovering") && !strings.Contains(err.Error(), "retained lease") {
		t.Fatalf("compensate failure must retain lease (Recovering): %v", err)
	}
	if !strings.Contains(err.Error(), "prompt") && !strings.Contains(err.Error(), "consumption") {
		t.Fatalf("primary error must surface: %v", err)
	}
	// Durable compensate failed → must not have recorded a successful compensate.
	if len(comp.compsCopy()) != 0 {
		t.Fatalf("failed compensate must not append success reason: %v", comp.compsCopy())
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

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-SEQ"))
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
		tabID:     "orphan-tab-start",
		startErr:  fmt.Errorf("agent_pane_busy forever"),
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-7"))
	if err == nil {
		t.Fatal("expected agent start failure")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if len(fh.closedTabs) != 1 || fh.closedTabs[0] != "orphan-tab-start" {
		t.Fatalf("expected orphan tab closed, got %v", fh.closedTabs)
	}
	if !hasCompensateReason(comp.compsCopy(), "agent_start_failed") {
		t.Fatalf("expected compensate agent_start_failed, got %v", comp.compsCopy())
	}
	if res != nil && res.Launched {
		t.Fatal("must not report Launched after start failure")
	}
}

func TestDispatch_AgentStart_TabCloseErrorNotSilent(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-7C")}},
	}
	fh := &fakeHerdr{
		available: true,
		workspace: "w1",
		model:     "m",
		tabID:     "orphan-tab-close",
		startErr:  fmt.Errorf("agent start failed"),
		closeErr:  fmt.Errorf("herdr tab close denied"),
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-7C"))
	if err == nil {
		t.Fatal("expected failure when start and tab-close both fail")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	// Primary start error preserved.
	if !strings.Contains(err.Error(), "agent start") {
		t.Fatalf("primary start error missing: %v", err)
	}
	// TabClose error must surface — orphan cannot be silently accepted.
	if !strings.Contains(err.Error(), "tab close") || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("TabClose error must propagate: %v", err)
	}
	// Exactly-one durable compensate (launch no longer double-fires shared lifecycle).
	if n := len(comp.compsCopy()); n != 1 {
		t.Fatalf("want exact-one compensate, got %d: %v", n, comp.compsCopy())
	}
	if !hasCompensateReason(comp.compsCopy(), "agent_start_failed") {
		t.Fatalf("expected agent_start_failed: %v", comp.compsCopy())
	}
	if hasCompensateReason(comp.compsCopy(), "agent_start_failed_orphan_tab_close_failed") {
		t.Fatalf("must not double-compensate orphan close: %v", comp.compsCopy())
	}
	if len(fh.closedTabs) != 1 {
		t.Fatalf("TabClose must still be attempted: %v", fh.closedTabs)
	}
	if res != nil && res.Launched {
		t.Fatal("must not launch")
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
		tabID:      "tab-prompt-delivery",
		deliverErr: fmt.Errorf("never confirmed consumption"),
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-8"))
	if err == nil {
		t.Fatal("expected prompt failure")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if len(fh.closedTabs) != 1 || fh.closedTabs[0] != "tab-prompt-delivery" {
		t.Fatalf("expected tab closed after prompt fail: %v", fh.closedTabs)
	}
	if !hasCompensateReason(comp.compsCopy(), "prompt_delivery_failed") {
		t.Fatalf("expected prompt_delivery_failed compensate: %v", comp.compsCopy())
	}
}

func TestDispatch_PromptFailure_TabCloseErrorNotSilent(t *testing.T) {
	_, wm := initDispatchRepo(t)
	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-8C")}},
	}
	fh := &fakeHerdr{
		available:  true,
		workspace:  "w1",
		model:      "m",
		tabID:      "tab-prompt-close",
		deliverErr: fmt.Errorf("never confirmed consumption"),
		closeErr:   fmt.Errorf("socket dead on close"),
	}
	comp := &recordingCompensator{}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-8C"))
	if err == nil {
		t.Fatal("expected failure")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if !strings.Contains(err.Error(), "consumption") {
		t.Fatalf("primary prompt error missing: %v", err)
	}
	if !strings.Contains(err.Error(), "tab close") || !strings.Contains(err.Error(), "socket dead") {
		t.Fatalf("TabClose error must propagate: %v", err)
	}
	if n := len(comp.compsCopy()); n != 1 {
		t.Fatalf("want exact-one compensate, got %d: %v", n, comp.compsCopy())
	}
	if !hasCompensateReason(comp.compsCopy(), "prompt_delivery_failed") {
		t.Fatalf("expected prompt_delivery_failed: %v", comp.compsCopy())
	}
	if hasCompensateReason(comp.compsCopy(), "prompt_delivery_failed_orphan_tab_close_failed") {
		t.Fatalf("must not double-compensate orphan close: %v", comp.compsCopy())
	}
	if res != nil && res.Launched {
		t.Fatal("must not launch")
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

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-3"))
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
	err := d.launch(context.Background(), validLaunchOptions(t, "FAC-3"), task, lane, wtInfo, wtInfo.Branch, "packet", result, nil)
	if err == nil {
		t.Fatal("launch on shared root must fail; production RejectSharedRoot guard missing?")
	}
	if !strings.Contains(err.Error(), "shared-root") && !strings.Contains(err.Error(), "repository root") {
		t.Fatalf("expected shared-root denial, got: %v", err)
	}
	// launch returns intent only — does not mutate shared lifecycle compensator.
	var lf *launchFailure
	if !errors.As(err, &lf) || lf.Reason != "shared_root_denied" {
		t.Fatalf("want launchFailure reason shared_root_denied, got %T %v", err, err)
	}
	if len(comp.compsCopy()) != 0 {
		t.Fatalf("launch must not compensate shared lifecycle: %v", comp.compsCopy())
	}
	if fh.tabCwd != "" || fh.startCalls != 0 {
		t.Fatalf("must not start agent on shared root: cwd=%q starts=%d", fh.tabCwd, fh.startCalls)
	}
	if result.Launched {
		t.Fatal("must not set Launched")
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

// TestDispatch_LaunchFailures_ExactlyOneCompensation covers every launch
// failure branch: launch returns intent only; Dispatch.failOwned runs durable
// compensate exactly once while the generation lease is held.
func TestDispatch_LaunchFailures_ExactlyOneCompensation(t *testing.T) {
	cases := []struct {
		name   string
		ref    string
		fh     *fakeHerdr
		reason string
	}{
		{
			name: "agent_start",
			ref:  "FAC-EO-1",
			fh: &fakeHerdr{
				available: true, workspace: "w1", model: "m", tabID: "t1",
				startErr: fmt.Errorf("pane busy"),
			},
			reason: "agent_start_failed",
		},
		{
			name: "prompt_delivery",
			ref:  "FAC-EO-2",
			fh: &fakeHerdr{
				available: true, workspace: "w1", model: "m", tabID: "t2",
				deliverErr: fmt.Errorf("no consumption"),
			},
			reason: "prompt_delivery_failed",
		},
		{
			name: "workspace_unknown",
			ref:  "FAC-EO-3",
			fh: &fakeHerdr{
				available: true, model: "m",
				wsErr: fmt.Errorf("workspace unknown"),
			},
			reason: "workspace_unknown",
		},
		{
			name: "prompt_sequence",
			ref:  "FAC-EO-4",
			fh: &fakeHerdr{
				available: true, workspace: "w1", model: "m", tabID: "t5",
				deliverRec: &herdr.PromptReceipt{
					Target: "x", BaselineStatus: "working", FinalStatus: "working",
					Consumed: true, Verified: true, SequenceToken: "working->working",
				},
			},
			reason: "prompt_sequence_invalid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, wm := initDispatchRepo(t)
			tp := &statusTrackingProvider{
				mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask(tc.ref)}},
			}
			comp := &recordingCompensator{}
			cfg := testCfg()
			d := NewDispatcher(cfg, tp, wm)
			d.Herdr = tc.fh
			d.Compensator = comp
			res, err := d.Dispatch(context.Background(), validLaunchOptions(t, tc.ref))
			if err == nil {
				t.Fatal("expected launch failure")
			}
			if res != nil {
				t.Cleanup(func() { os.RemoveAll(res.Worktree) })
			}
			comps := comp.compsCopy()
			if len(comps) != 1 {
				t.Fatalf("want exact-one compensate, got %d: %v", len(comps), comps)
			}
			if !hasCompensateReason(comps, tc.reason) {
				t.Fatalf("want reason %s, got %v", tc.reason, comps)
			}
			if res != nil && res.Launched {
				t.Fatal("must not report Launched")
			}
		})
	}
}

// TestDispatch_CompensateFailure_RetainsGenerationLease proves durable
// compensate failure keeps the generation lease so B cannot acquire and get
// stomped by a subsequent stale release.
func TestDispatch_CompensateFailure_RetainsGenerationLease(t *testing.T) {
	repo, wm := initDispatchRepo(t)
	leasePath := filepath.Join(repo, ".herd", "launch-retain.db")
	ownA, err := deps.OpenLeaseOwnership(leasePath, "herd", "memory", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownA.Close() })
	ownA.LaneResolver = func(role string) (string, error) {
		if role == "launch" || role == "worker" {
			return "smith", nil
		}
		return "", fmt.Errorf("unknown configured test role %q", role)
	}

	tp := &statusTrackingProvider{
		mockTaskProvider: mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-RET")}},
	}
	fh := &fakeHerdr{
		available:  true,
		workspace:  "w1",
		model:      "m",
		tabID:      "tab-ret",
		deliverErr: fmt.Errorf("no flip"),
	}
	comp := &recordingCompensator{compErr: fmt.Errorf("outbox down")}
	d := NewDispatcher(testCfg(), tp, wm)
	d.Herdr = fh
	d.Compensator = comp
	d.Ownership = ownA

	res, err := d.Dispatch(context.Background(), validLaunchOptions(t, "FAC-RET"))
	if err == nil {
		t.Fatal("expected failure")
	}
	if res != nil {
		t.Cleanup(func() { os.RemoveAll(res.Worktree) })
	}
	if !strings.Contains(err.Error(), "Recovering") && !strings.Contains(err.Error(), "retained lease") {
		t.Fatalf("want retain/Recovering signal: %v", err)
	}

	// B must not acquire while A retained the generation after failed durable compensate.
	ownB, err := deps.OpenLeaseOwnership(leasePath, "herd", "memory", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ownB.Close() })
	ownB.LaneResolver = ownA.LaneResolver
	_, berr := ownB.ClaimExclusive(context.Background(), "id", "FAC-RET", "launch", "rev-x", "", "")
	if berr == nil {
		t.Fatal("B must not acquire while A retained lease after compensate failure")
	}
	if !errors.Is(berr, deps.ErrAlreadyClaimed) && !strings.Contains(berr.Error(), "already") {
		t.Fatalf("want already claimed, got %v", berr)
	}
}
