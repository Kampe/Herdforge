package daemon

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

const (
	tickWorkerProvider = "codex"
	tickWorkerModel    = "gpt-5.6-luna"
	tickWorkerEffort   = "medium"
)

// --- fakes -----------------------------------------------------------------

type tickFakeHerdr struct {
	mu sync.Mutex

	available   bool
	workspace   string
	wsErr       error
	tabErr      error
	startErr    error
	deliverErr  error
	startCalls  int
	deliverCalls int
	tabCwd      string
	tabLabel    string
	startReq    launch.Request
	startName   string
	startKind   string
	startPane   string
	deliverText string
	closedTabs  []string
	tabID       string
	paneID      string
}

func (f *tickFakeHerdr) Available() bool { return f.available }
func (f *tickFakeHerdr) RequireWorkspace(string) (string, error) {
	if f.wsErr != nil {
		return "", f.wsErr
	}
	if f.workspace == "" {
		return "ws-1", nil
	}
	return f.workspace, nil
}
func (f *tickFakeHerdr) TabCreateForTask(workspaceID, label, cwd string, _ bool, _ ...string) (*herdr.TabInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tabLabel = label
	f.tabCwd = cwd
	if f.tabErr != nil {
		return nil, f.tabErr
	}
	id := f.tabID
	if id == "" {
		id = "tab-daemon-1"
	}
	pane := f.paneID
	if pane == "" {
		pane = "pane-daemon-1"
	}
	return &herdr.TabInfo{ID: id, Label: label, Cwd: cwd, Pane: herdr.PaneInfo{ID: pane, TabID: id}}, nil
}
func (f *tickFakeHerdr) AgentStart(req launch.Request, name, kind, paneID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	f.startReq = req
	f.startName = name
	f.startKind = kind
	f.startPane = paneID
	return f.startErr
}
func (f *tickFakeHerdr) DeliverAndProve(target, text string, _ time.Duration) (*herdr.PromptReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deliverCalls++
	f.deliverText = text
	if f.deliverErr != nil {
		return nil, f.deliverErr
	}
	return &herdr.PromptReceipt{
		Target: target, BaselineStatus: "idle", FinalStatus: "working",
		Consumed: true, Verified: true, SequenceToken: "idle->working",
	}, nil
}
func (f *tickFakeHerdr) TabClose(tabID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closedTabs = append(f.closedTabs, tabID)
	return nil
}

type tickFakeWorktree struct {
	root    string
	path    string
	branch  string
	baseSHA string
	err     error
	calls   int
}

func (w *tickFakeWorktree) CreateTaskWorktreeFrom(_ context.Context, taskRef, _ string) (*worktree.WorktreeInfo, error) {
	w.calls++
	if w.err != nil {
		return nil, w.err
	}
	path := w.path
	if path == "" {
		path = filepath.Join(w.root, ".herd", "worktrees", strings.ToLower(taskRef))
	}
	branch := w.branch
	if branch == "" {
		branch = "herd/" + strings.ToLower(taskRef)
	}
	base := w.baseSHA
	if base == "" {
		base = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	return &worktree.WorktreeInfo{Path: path, Branch: branch, BaseSHA: base, AnchorRef: "origin/main"}, nil
}
func (w *tickFakeWorktree) RepoRoot() string {
	if w.root != "" {
		return w.root
	}
	return "/tmp/repo-root-never-used"
}

func tickRouter(t *testing.T) *router.SurfaceRouter {
	t.Helper()
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", t.TempDir())
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{
		CLIPresent: func(cli string) bool { return cli == router.PiHarness },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	return r
}

func validTickDecision(t *testing.T, laneName string) *router.LaunchDecision {
	t.Helper()
	d, err := tickRouter(t).Decide(router.LaunchRequest{
		Role:              router.RoleWorker,
		Shape:             launch.Implementation,
		RequestedProvider: tickWorkerProvider,
		RequestedModel:    tickWorkerModel,
		RequestedEffort:   tickWorkerEffort,
		TaskRef:           laneName,
		Scope:             router.ScopeLane,
		ProbeResults:      map[string]bool{router.ProbeKey(tickWorkerProvider, tickWorkerModel): true},
	})
	if err != nil {
		t.Fatalf("build launch decision: %v", err)
	}
	return d
}

func fence(ref, id string) string {
	return "```herd-deps-v1\n{\"version\":1,\"task_ref\":\"" + ref + "\",\"task_id\":\"" + id + "\",\"edges\":[]}\n```\n"
}

func tickEngine(t *testing.T, mp *provider.MemoryProvider, project string) (*Engine, *deps.LeaseOwnership) {
	t.Helper()
	cfg := &config.Config{
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: project},
		Lanes: []config.LaneDef{{
			Name: "worker", Role: "worker", Provider: tickWorkerProvider,
			Model: tickWorkerModel, Effort: tickWorkerEffort, TaskShape: launch.Implementation,
			AgentKind: router.PiHarness, Harness: router.PiHarness,
		}},
	}
	eng := NewEngine(cfg, mp, nil, nil, nil, nil)
	own, err := deps.OpenLeaseOwnership(filepath.Join(t.TempDir(), "lease.db"), "test-repo", "memory", project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = own.Close() })
	own.LaneResolver = func(role string) (string, error) {
		if role == "worker" || role == "herd-smith" || role == "pulse" {
			return "worker", nil
		}
		return "", fmt.Errorf("unknown role %q", role)
	}
	eng.Ownership = own
	return eng, own
}

func baseTickOpts(t *testing.T, h *tickFakeHerdr, wt *tickFakeWorktree) TickOptions {
	t.Helper()
	lane := &config.LaneDef{
		Name: "worker", Role: "worker", Provider: tickWorkerProvider,
		Model: tickWorkerModel, Effort: tickWorkerEffort, TaskShape: launch.Implementation,
		AgentKind: router.PiHarness, Harness: router.PiHarness,
	}
	return TickOptions{
		Decision:   validTickDecision(t, lane.Name),
		Lane:       lane,
		Repository: "test-repo",
		Herdr:      h,
		Worktree:   wt,
		Packet: func(task *provider.Task, _ *config.LaneDef, path string) string {
			return fmt.Sprintf("PACKET %s cwd=%s", task.Ref, path)
		},
	}
}

// --- production-shaped fixture: claim-only is forbidden --------------------

// TestDaemonTick_ClaimWithoutLaunchIsForbidden proves RunDaemonTick rejects a
// claim that cannot be followed by a verified worker launch, and leaves no
// orphan In Progress card. A mutant that returns success after RunPulse alone
// (the pre-FAC-196 daemon shape) turns this test red.
func TestDaemonTick_ClaimWithoutLaunchIsForbidden(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "t-1", Ref: "FAC-196", Title: "orphan risk", Priority: provider.PriorityUrgent,
		Status: "to-do", ProjectID: "p1", Labels: []string{"worker"},
		Description: fence("FAC-196", "t-1"),
	})
	eng, _ := tickEngine(t, mp, "p1")
	root := t.TempDir()
	// Herdr available for prep, but agent start fails after the fenced claim —
	// the exact orphan shape: board would be In Progress with zero worker
	// unless compensation runs.
	h := &tickFakeHerdr{available: true, startErr: errors.New("agent start refused")}
	wt := &tickFakeWorktree{root: root, path: filepath.Join(root, ".herd", "worktrees", "fac-196")}
	opts := baseTickOpts(t, h, wt)

	rec, err := eng.RunDaemonTick(context.Background(), "worker", opts)
	if err == nil {
		t.Fatal("RunDaemonTick must fail when launch cannot complete after claim")
	}
	if !errors.Is(err, ErrClaimWithoutLaunch) {
		t.Fatalf("want ErrClaimWithoutLaunch, got %v", err)
	}
	if rec != nil && rec.Launched {
		t.Fatalf("must not return a launched receipt on claim-without-launch: %+v", rec)
	}
	// Board must not remain In Progress with no worker.
	got, gerr := mp.GetTask(context.Background(), "t-1")
	if gerr != nil {
		t.Fatalf("GetTask: %v", gerr)
	}
	if got.Status != provider.StatusToDo {
		t.Fatalf("orphan In Progress after failed launch: status=%s (compensation required)", got.Status)
	}
	// Claim path was exercised (not a prep-only rejection before board claim).
	if h.startCalls != 1 {
		t.Fatalf("expected one AgentStart attempt after claim, got %d (prep-only failure would be 0)", h.startCalls)
	}
	// Mutation guard: a success path that only RunPulses would leave
	// In Progress and return nil error — both are rejected above.
}

// --- success path ----------------------------------------------------------

func TestDaemonTick_Success_OneClaimOneLaunchOnePacket(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "t-1", Ref: "FAC-196", Title: "tick success", Priority: provider.PriorityUrgent,
		Status: "to-do", ProjectID: "p1", Labels: []string{"worker"},
		Description: fence("FAC-196", "t-1"),
	})
	eng, _ := tickEngine(t, mp, "p1")
	root := t.TempDir()
	h := &tickFakeHerdr{available: true, workspace: "ws-1"}
	wt := &tickFakeWorktree{root: root, path: filepath.Join(root, ".herd", "worktrees", "fac-196")}
	lc, err := lifecycle.NewMachine(filepath.Join(t.TempDir(), "lc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lc.Close() })

	opts := baseTickOpts(t, h, wt)
	opts.Lifecycle = lc

	rec, err := eng.RunDaemonTick(context.Background(), "worker", opts)
	if err != nil {
		t.Fatalf("RunDaemonTick: %v", err)
	}
	if rec == nil || !rec.Launched {
		t.Fatalf("expected launched receipt, got %+v", rec)
	}
	if rec.TaskRef != "FAC-196" || rec.LeaseGeneration == 0 {
		t.Fatalf("receipt identity incomplete: %+v", rec)
	}
	if rec.Model != tickWorkerModel || rec.Effort != tickWorkerEffort {
		t.Fatalf("want Luna/medium, got %s/%s", rec.Model, rec.Effort)
	}
	if h.startCalls != 1 || h.deliverCalls != 1 {
		t.Fatalf("start=%d deliver=%d want 1 each", h.startCalls, h.deliverCalls)
	}
	if wt.calls != 1 {
		t.Fatalf("worktree calls=%d want 1", wt.calls)
	}
	if rec.PacketDigest == "" || rec.PromptSequence != "idle->working" {
		t.Fatalf("packet/prompt proof missing: digest=%q seq=%q", rec.PacketDigest, rec.PromptSequence)
	}
	if rec.LifecycleState != lifecycle.StateDispatched {
		t.Fatalf("lifecycle=%s want dispatched", rec.LifecycleState)
	}
	// Board remains In Progress after successful dispatch (worker owns it).
	got, _ := mp.GetTask(context.Background(), "t-1")
	if got.Status != provider.StatusInProgress {
		t.Fatalf("status after success=%s want in-progress", got.Status)
	}
	// Exact Luna-medium argv retained.
	wantArgv := router.ArgvFor(tickWorkerProvider, tickWorkerModel, tickWorkerEffort)
	if len(rec.Argv) != len(wantArgv) {
		t.Fatalf("argv=%v want %v", rec.Argv, wantArgv)
	}
	for i := range wantArgv {
		if rec.Argv[i] != wantArgv[i] {
			t.Fatalf("argv[%d]=%q want %q", i, rec.Argv[i], wantArgv[i])
		}
	}
}

// --- prep failures before claim --------------------------------------------

func TestDaemonTick_PrepFailuresLeaveNoBoardClaim(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*TickOptions, *tickFakeHerdr)
	}{
		{"missing lane", func(o *TickOptions, _ *tickFakeHerdr) { o.Lane = nil }},
		{"empty repository", func(o *TickOptions, _ *tickFakeHerdr) { o.Repository = "" }},
		{"nil decision", func(o *TickOptions, _ *tickFakeHerdr) { o.Decision = nil }},
		{"herdr unavailable", func(_ *TickOptions, h *tickFakeHerdr) { h.available = false }},
		{"nil herdr", func(o *TickOptions, _ *tickFakeHerdr) { o.Herdr = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := provider.NewMemoryProvider()
			mp.AddTask(&provider.Task{
				ID: "t-1", Ref: "FAC-196", Title: "prep", Priority: provider.PriorityHigh,
				Status: "to-do", ProjectID: "p1", Labels: []string{"worker"},
				Description: fence("FAC-196", "t-1"),
			})
			eng, _ := tickEngine(t, mp, "p1")
			h := &tickFakeHerdr{available: true}
			wt := &tickFakeWorktree{root: t.TempDir()}
			opts := baseTickOpts(t, h, wt)
			tc.mut(&opts, h)

			_, err := eng.RunDaemonTick(context.Background(), "worker", opts)
			if err == nil {
				t.Fatal("expected prep error")
			}
			if !errors.Is(err, ErrDaemonPrep) && !strings.Contains(err.Error(), "preparation refused") {
				// nil herdr / unavailable also wrap ErrDaemonPrep
				if !strings.Contains(err.Error(), "daemon:") {
					t.Fatalf("want ErrDaemonPrep, got %v", err)
				}
			}
			got, _ := mp.GetTask(context.Background(), "t-1")
			if got.Status != provider.StatusToDo {
				t.Fatalf("board mutated on prep failure: status=%s", got.Status)
			}
			if h.startCalls != 0 {
				t.Fatalf("launch must not run after prep failure, startCalls=%d", h.startCalls)
			}
		})
	}
}

// --- post-claim failures compensate ----------------------------------------

func TestDaemonTick_LaunchFailuresCompensateBoard(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*tickFakeHerdr, *tickFakeWorktree)
	}{
		{"worktree fail", func(_ *tickFakeHerdr, wt *tickFakeWorktree) { wt.err = errors.New("wt boom") }},
		{"tab fail", func(h *tickFakeHerdr, _ *tickFakeWorktree) { h.tabErr = errors.New("tab boom") }},
		{"start fail", func(h *tickFakeHerdr, _ *tickFakeWorktree) { h.startErr = errors.New("start boom") }},
		{"prompt fail", func(h *tickFakeHerdr, _ *tickFakeWorktree) { h.deliverErr = errors.New("prompt boom") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := provider.NewMemoryProvider()
			mp.AddTask(&provider.Task{
				ID: "t-1", Ref: "FAC-196", Title: "comp", Priority: provider.PriorityHigh,
				Status: "to-do", ProjectID: "p1", Labels: []string{"worker"},
				Description: fence("FAC-196", "t-1"),
			})
			eng, _ := tickEngine(t, mp, "p1")
			root := t.TempDir()
			h := &tickFakeHerdr{available: true}
			wt := &tickFakeWorktree{root: root, path: filepath.Join(root, ".herd", "worktrees", "fac-196")}
			tc.mut(h, wt)
			opts := baseTickOpts(t, h, wt)

			_, err := eng.RunDaemonTick(context.Background(), "worker", opts)
			if err == nil {
				t.Fatal("expected launch failure")
			}
			if !errors.Is(err, ErrClaimWithoutLaunch) {
				t.Fatalf("want ErrClaimWithoutLaunch, got %v", err)
			}
			got, _ := mp.GetTask(context.Background(), "t-1")
			if got.Status != provider.StatusToDo {
				t.Fatalf("orphan In Progress after %s: status=%s", tc.name, got.Status)
			}
		})
	}
}

// --- standing reuse with identity readback ---------------------------------

func TestDaemonTick_StandingReuseRequiresIdentityReadback(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "t-1", Ref: "FAC-196", Title: "standing", Priority: provider.PriorityHigh,
		Status: "to-do", ProjectID: "p1", Labels: []string{"worker"},
		Description: fence("FAC-196", "t-1"),
	})
	eng, _ := tickEngine(t, mp, "p1")
	root := t.TempDir()
	cwd := filepath.Join(root, ".herd", "worktrees", "fac-196")
	h := &tickFakeHerdr{available: true}
	wt := &tickFakeWorktree{root: root, path: cwd}
	opts := baseTickOpts(t, h, wt)
	opts.StandingName = "forge-worker"
	opts.ResolveStanding = func(_ context.Context, name string, req launch.Request) (*StandingAgent, error) {
		return &StandingAgent{
			Name: name, TabID: "tab-standing", PaneID: "pane-standing",
			Session: "sess-1", CWD: cwd, Model: req.Decision.Model, Harness: req.Decision.Harness,
		}, nil
	}

	rec, err := eng.RunDaemonTick(context.Background(), "worker", opts)
	if err != nil {
		t.Fatalf("RunDaemonTick: %v", err)
	}
	if !rec.ReusedStanding || rec.TabID != "tab-standing" {
		t.Fatalf("expected standing reuse, got %+v", rec)
	}
	if h.startCalls != 0 {
		t.Fatalf("standing reuse must not AgentStart again, calls=%d", h.startCalls)
	}
	if h.deliverCalls != 1 {
		t.Fatalf("standing reuse still requires packet prove, deliver=%d", h.deliverCalls)
	}
}

func TestDaemonTick_StandingModelMismatchRefuses(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "t-1", Ref: "FAC-196", Title: "mismatch", Priority: provider.PriorityHigh,
		Status: "to-do", ProjectID: "p1", Labels: []string{"worker"},
		Description: fence("FAC-196", "t-1"),
	})
	eng, _ := tickEngine(t, mp, "p1")
	root := t.TempDir()
	cwd := filepath.Join(root, ".herd", "worktrees", "fac-196")
	h := &tickFakeHerdr{available: true}
	wt := &tickFakeWorktree{root: root, path: cwd}
	opts := baseTickOpts(t, h, wt)
	opts.StandingName = "forge-worker"
	opts.ResolveStanding = func(_ context.Context, name string, _ launch.Request) (*StandingAgent, error) {
		return &StandingAgent{
			Name: name, TabID: "tab-standing", PaneID: "pane-standing",
			CWD: cwd, Model: "gpt-5.6-sol", Harness: router.PiHarness,
		}, nil
	}

	_, err := eng.RunDaemonTick(context.Background(), "worker", opts)
	if err == nil {
		t.Fatal("expected model mismatch failure")
	}
	got, _ := mp.GetTask(context.Background(), "t-1")
	if got.Status != provider.StatusToDo {
		t.Fatalf("must compensate on standing mismatch, status=%s", got.Status)
	}
}

// --- concurrent daemons cannot double-dispatch -----------------------------

func TestDaemonTick_TwoConcurrentDaemonsOneWinner(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "t-1", Ref: "FAC-196", Title: "race", Priority: provider.PriorityUrgent,
		Status: "to-do", ProjectID: "p1", Labels: []string{"worker"},
		Description: fence("FAC-196", "t-1"),
	})
	leasePath := filepath.Join(t.TempDir(), "lease.db")
	makeEng := func() *Engine {
		cfg := &config.Config{TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "p1"}}
		eng := NewEngine(cfg, mp, nil, nil, nil, nil)
		own, err := deps.OpenLeaseOwnership(leasePath, "test-repo", "memory", "p1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = own.Close() })
		own.LaneResolver = func(string) (string, error) { return "worker", nil }
		eng.Ownership = own
		return eng
	}
	engA, engB := makeEng(), makeEng()
	root := t.TempDir()
	var starts atomic.Int32
	hA := &tickFakeHerdr{available: true}
	hB := &tickFakeHerdr{available: true}
	// Count starts via wrappers: use shared counter by checking after join.

	var wg sync.WaitGroup
	var errA, errB error
	var recA, recB *TickReceipt
	wg.Add(2)
	go func() {
		defer wg.Done()
		opts := baseTickOpts(t, hA, &tickFakeWorktree{root: root, path: filepath.Join(root, "a")})
		recA, errA = engA.RunDaemonTick(context.Background(), "worker", opts)
		if errA == nil && recA != nil && recA.Launched {
			starts.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		opts := baseTickOpts(t, hB, &tickFakeWorktree{root: root, path: filepath.Join(root, "b")})
		recB, errB = engB.RunDaemonTick(context.Background(), "worker", opts)
		if errB == nil && recB != nil && recB.Launched {
			starts.Add(1)
		}
	}()
	wg.Wait()

	launched := 0
	if errA == nil && recA != nil && recA.Launched {
		launched++
	}
	if errB == nil && recB != nil && recB.Launched {
		launched++
	}
	if launched != 1 {
		t.Fatalf("expected exactly one successful dispatch, got %d (errA=%v errB=%v)", launched, errA, errB)
	}
	// Exactly one AgentStart across both.
	totalStarts := hA.startCalls + hB.startCalls
	if totalStarts != 1 {
		t.Fatalf("agent starts=%d want 1", totalStarts)
	}
}

// --- repeated ticks do not double-dispatch the same task -------------------

func TestDaemonTick_RepeatedTickDoesNotDoubleDispatch(t *testing.T) {
	mp := provider.NewMemoryProvider()
	mp.AddTask(&provider.Task{
		ID: "t-1", Ref: "FAC-196", Title: "once", Priority: provider.PriorityUrgent,
		Status: "to-do", ProjectID: "p1", Labels: []string{"worker"},
		Description: fence("FAC-196", "t-1"),
	})
	eng, _ := tickEngine(t, mp, "p1")
	root := t.TempDir()
	h := &tickFakeHerdr{available: true}
	wt := &tickFakeWorktree{root: root, path: filepath.Join(root, ".herd", "worktrees", "fac-196")}
	opts := baseTickOpts(t, h, wt)

	rec1, err := eng.RunDaemonTick(context.Background(), "worker", opts)
	if err != nil || rec1 == nil || !rec1.Launched {
		t.Fatalf("first tick: rec=%+v err=%v", rec1, err)
	}
	// Second tick: task is In Progress / leased — no second claim of same todo.
	rec2, err2 := eng.RunDaemonTick(context.Background(), "worker", opts)
	// Either no task (nil,nil) or claim conflict error; must not launch again.
	if err2 == nil && rec2 != nil && rec2.Launched {
		t.Fatal("second tick must not re-dispatch the same task")
	}
	if h.startCalls != 1 {
		t.Fatalf("startCalls=%d want 1", h.startCalls)
	}
}

// --- source mutation guards ------------------------------------------------

// TestDaemonTick_SourceRequiresLaunchAfterRunPulse fails if someone deletes
// the launch call while retaining RunPulse (the orphan regression).
func TestDaemonTick_SourceRequiresLaunchAfterRunPulse(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(packageDir(t), "tick.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	fn := extractFunc(body, "func (e *Engine) RunDaemonTick")
	if fn == "" {
		t.Fatal("RunDaemonTick not found")
	}
	if !strings.Contains(fn, "RunPulse") {
		t.Fatal("RunDaemonTick must call RunPulse for the fenced claim")
	}
	if !strings.Contains(fn, "launchAfterClaim") {
		t.Fatal("RunDaemonTick must call launchAfterClaim; deleting launch while retaining RunPulse is the orphan bug")
	}
	// Prep must precede claim.
	prepIdx := strings.Index(fn, "prepareDaemonTick")
	pulseIdx := strings.Index(fn, "RunPulse")
	if prepIdx < 0 || pulseIdx < 0 || prepIdx > pulseIdx {
		t.Fatalf("prepareDaemonTick must precede RunPulse (prep=%d pulse=%d)", prepIdx, pulseIdx)
	}
}

// TestDaemonTick_CrashPointBoardClaimAfterPrep proves board claim cannot move
// before non-compensable launch preparation in the source ordering.
func TestDaemonTick_CrashPointBoardClaimAfterPrep(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(packageDir(t), "tick.go"))
	if err != nil {
		t.Fatal(err)
	}
	prep := extractFunc(string(src), "func prepareDaemonTick")
	if prep == "" {
		t.Fatal("prepareDaemonTick not found")
	}
	for _, need := range []string{"Lane", "Repository", "Decision", "Available"} {
		if !strings.Contains(prep, need) {
			t.Fatalf("prepareDaemonTick must check %s before claim", need)
		}
	}
	// prepareDaemonTick must not call claim/board APIs.
	for _, forbid := range []string{"RunPulse", "ClaimTask", "claimTaskBound", "FencedClaim", "UpdateStatus"} {
		if strings.Contains(prep, forbid) {
			t.Fatalf("prepareDaemonTick must not perform %s (non-compensable prep only)", forbid)
		}
	}
}

// TestDaemonTick_EmptyTickIsNotSuccess ensures nil task is not reported as launched.
func TestDaemonTick_EmptyQueue(t *testing.T) {
	mp := provider.NewMemoryProvider()
	eng, _ := tickEngine(t, mp, "p1")
	h := &tickFakeHerdr{available: true}
	wt := &tickFakeWorktree{root: t.TempDir()}
	rec, err := eng.RunDaemonTick(context.Background(), "worker", baseTickOpts(t, h, wt))
	if err != nil {
		t.Fatalf("empty queue: %v", err)
	}
	if rec != nil {
		t.Fatalf("expected nil receipt on empty queue, got %+v", rec)
	}
	if h.startCalls != 0 {
		t.Fatalf("no launch on empty queue, starts=%d", h.startCalls)
	}
}

func packageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	return filepath.Dir(file)
}

func extractFunc(src, sig string) string {
	idx := strings.Index(src, sig)
	if idx < 0 {
		return ""
	}
	// Brace match from first '{' after sig.
	rest := src[idx:]
	brace := strings.Index(rest, "{")
	if brace < 0 {
		return ""
	}
	depth := 0
	for i := brace; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i+1]
			}
		}
	}
	return rest
}

// TestDaemonTick_CmdWiringCallsRunDaemonTick proves the production herd daemon
// entrypoint uses the claim-to-dispatch transaction rather than bare RunPulse.
func TestDaemonTick_CmdWiringCallsRunDaemonTick(t *testing.T) {
	root := filepath.Clean(filepath.Join(packageDir(t), "..", ".."))
	src, err := os.ReadFile(filepath.Join(root, "cmd", "herd", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	fn := extractFunc(string(src), "func runDaemon()")
	if fn == "" {
		t.Fatal("runDaemon not found in cmd/herd/main.go")
	}
	if !strings.Contains(fn, "RunDaemonTick") {
		t.Fatal("runDaemon must call RunDaemonTick (FAC-196 claim-to-dispatch)")
	}
	// Bare claim-only print is the orphan regression.
	if strings.Contains(fn, "Claimed:") && !strings.Contains(fn, "Dispatched:") {
		t.Fatal("runDaemon still reports Claimed without Dispatched — orphan path")
	}
	// Non-compensable admission must precede the tick (which claims).
	admitIdx := strings.Index(fn, "launchAdmissionWithLifecycle")
	tickIdx := strings.Index(fn, "RunDaemonTick")
	if admitIdx < 0 || tickIdx < 0 || admitIdx > tickIdx {
		t.Fatalf("launch admission must precede RunDaemonTick (admit=%d tick=%d)", admitIdx, tickIdx)
	}
}

// Compile-time check that AST packages stay available for mutation tests.
var _ = parser.ParseFile
var _ = token.NewFileSet
var _ = ast.File{}
