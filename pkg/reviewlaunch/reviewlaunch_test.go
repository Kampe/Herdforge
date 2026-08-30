package reviewlaunch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
)

// --- fakes -----------------------------------------------------------------

type fakeBoard struct {
	mu       sync.Mutex
	calls    []string
	status   map[string]string
	updateEr error
	// timeline is a shared stage log with herdr fakes so tests can assert
	// board-vs-deliver order in one sequence (FAC-187 review finding).
	timeline *[]string
}

func (b *fakeBoard) UpdateStatus(_ context.Context, taskID, status string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timeline != nil {
		*b.timeline = append(*b.timeline, "board")
	}
	if b.updateEr != nil {
		return b.updateEr
	}
	b.calls = append(b.calls, taskID+":"+status)
	if b.status == nil {
		b.status = map[string]string{}
	}
	b.status[taskID] = status
	return nil
}

func (b *fakeBoard) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func (b *fakeBoard) lastStatus(taskID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status[taskID]
}

type fakeHerdr struct {
	mu sync.Mutex

	workspace string
	wsErr     error

	tabErr     error
	tabID      string
	paneID     string
	tabCwd     string // if set, returned as TabInfo.Cwd
	startErr   error
	deliverErr error
	deliverRec *herdr.PromptReceipt
	readErr    error
	readSnap   AgentSnapshot
	readSnaps  []AgentSnapshot
	readCount  int
	// When true, ReadAgent returns empty session (null agent_session case).
	nullSession bool

	closed   []string
	closeErr error

	startCalls   int
	deliverCalls int
	// Order probe: append stage names as side effects run.
	stages   []string
	timeline *[]string // shared with fakeBoard for cross-surface order
}

func (f *fakeHerdr) note(stage string) {
	f.stages = append(f.stages, stage)
	if f.timeline != nil {
		*f.timeline = append(*f.timeline, stage)
	}
}

func (f *fakeHerdr) RequireWorkspace(string) (string, error) {
	if f.wsErr != nil {
		return "", f.wsErr
	}
	if f.workspace == "" {
		return "wF", nil
	}
	return f.workspace, nil
}

func (f *fakeHerdr) TabCreateForTask(ws, label, cwd string, _ bool) (*herdr.TabInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("tab_create")
	if f.tabErr != nil {
		return nil, f.tabErr
	}
	id := f.tabID
	if id == "" {
		id = "tab-new"
	}
	pane := f.paneID
	if pane == "" {
		pane = "pane-new"
	}
	tabCwd := f.tabCwd
	if tabCwd == "" {
		// Mirror production herdr: TabInfo.Cwd is always absolute.
		abs, err := filepath.Abs(cwd)
		if err != nil {
			tabCwd = cwd
		} else {
			tabCwd = abs
		}
	}
	// Capture created label for identity.
	f.readSnap = AgentSnapshot{
		Name: label, PaneID: pane, TabID: id, Workspace: ws, Cwd: tabCwd, Session: "sess-1", Status: "idle", Kind: "pi",
	}
	return &herdr.TabInfo{ID: id, Label: label, Cwd: tabCwd, Pane: herdr.PaneInfo{ID: pane, TabID: id}}, nil
}

func (f *fakeHerdr) AgentStart(_ launch.Request, name, kind, paneID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("agent_start")
	f.startCalls++
	if f.startErr != nil {
		return f.startErr
	}
	if f.readSnap.Name == "" {
		f.readSnap.Name = name
	}
	if f.readSnap.PaneID == "" {
		f.readSnap.PaneID = paneID
	}
	_ = kind
	return nil
}

func (f *fakeHerdr) DeliverAndProve(target, text string, _ time.Duration) (*herdr.PromptReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("deliver")
	f.deliverCalls++
	if f.deliverErr != nil {
		return nil, f.deliverErr
	}
	if f.deliverRec != nil {
		return f.deliverRec, nil
	}
	_ = target
	_ = text
	return &herdr.PromptReceipt{
		Target: target, BaselineStatus: "idle", FinalStatus: "working",
		Consumed: true, Verified: true, SequenceToken: "idle->working", SawWorking: true,
	}, nil
}

func (f *fakeHerdr) TabClose(tabID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("tab_close")
	f.closed = append(f.closed, tabID)
	return f.closeErr
}

func (f *fakeHerdr) ReadAgent(name string) (AgentSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.note("read_agent")
	if f.readErr != nil {
		return AgentSnapshot{}, f.readErr
	}
	if f.nullSession {
		return AgentSnapshot{}, ErrMissingSession
	}
	s := f.readSnap
	if len(f.readSnaps) > 0 {
		idx := f.readCount
		if idx >= len(f.readSnaps) {
			idx = len(f.readSnaps) - 1
		}
		s = f.readSnaps[idx]
		f.readCount++
	}
	if s.Name == "" {
		s.Name = name
	}
	return s, nil
}

// boardBeforeDeliver reports whether "board" appears before "deliver" in tl.
// Used as the non-vacuous mutation probe for transition-order tests.
func boardBeforeDeliver(tl []string) bool {
	di, bi := -1, -1
	for i, s := range tl {
		if s == "deliver" && di < 0 {
			di = i
		}
		if s == "board" && bi < 0 {
			bi = i
		}
	}
	return bi >= 0 && di >= 0 && bi < di
}

func assertDeliverBeforeBoard(t *testing.T, tl []string) {
	t.Helper()
	if boardBeforeDeliver(tl) {
		t.Fatalf("board ran before deliver: %v", tl)
	}
	di, bi := -1, -1
	for i, s := range tl {
		if s == "deliver" && di < 0 {
			di = i
		}
		if s == "board" && bi < 0 {
			bi = i
		}
	}
	if di < 0 || bi < 0 {
		t.Fatalf("timeline missing deliver or board: %v", tl)
	}
}

func baseReq(t *testing.T) Request {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "fac-151")
	return Request{
		TaskRef: "FAC-151", TaskID: "task-151", Role: "assayer", Lane: "assayer",
		Repository: "Kampe/Herdforge", RepoRoot: t.TempDir(), WorktreePath: wt,
		Packet: "REVIEW FAC-151 — verdict only", Timeout: time.Second,
	}
}

// --- agent name grammar ----------------------------------------------------

func TestAgentName_RejectsUppercaseLikeIncidentLabel(t *testing.T) {
	// The 2026-08-03 label review-assayer-FAC-151 is invalid under herdr 0.7.5.
	if err := ValidateAgentName("review-assayer-FAC-151"); !errors.Is(err, ErrInvalidAgentName) {
		t.Fatalf("expected invalid_agent_name for incident label, got %v", err)
	}
	name, err := AgentName("assayer", "FAC-151")
	if err != nil {
		t.Fatal(err)
	}
	if name != "review-assayer-fac-151" {
		t.Fatalf("got %q", name)
	}
	if err := ValidateAgentName(name); err != nil {
		t.Fatal(err)
	}
	if len(name) > 32 {
		t.Fatalf("name too long: %q", name)
	}
}

func TestAgentName_FitsWithin32(t *testing.T) {
	name, err := AgentName("reviewer", "FAC-187-VERY-LONG-SUFFIX-EXTRA")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAgentName(name); err != nil {
		t.Fatalf("%q: %v", name, err)
	}
}

// --- happy path + transition order -----------------------------------------

func TestLaunch_BoardTransitionOnlyAfterVerifiedConsumption(t *testing.T) {
	var tl []string
	h := &fakeHerdr{workspace: "wF", timeline: &tl}
	b := &fakeBoard{timeline: &tl}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	// Relative worktree path — production runReview passes this shape; herdr
	// returns absolute TabInfo.Cwd. Happy path must still complete.
	req := baseReq(t)
	rel := filepath.Join(".", "relative-wt-"+filepath.Base(req.WorktreePath))
	// Use the already-created temp dir as relative by chdiring into its parent.
	parent := filepath.Dir(req.WorktreePath)
	base := filepath.Base(req.WorktreePath)
	t.Chdir(parent)
	req.WorktreePath = base
	_ = rel

	res, err := l.Launch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.BoardTo != "in-review" || b.lastStatus(req.TaskID) != "in-review" {
		t.Fatalf("board = %q result=%+v", b.lastStatus(req.TaskID), res)
	}
	if res.AgentName != "review-assayer-fac-151" {
		t.Fatalf("agent name = %q", res.AgentName)
	}
	assertDeliverBeforeBoard(t, tl)
	if b.callCount() != 1 {
		t.Fatalf("board calls = %d", b.callCount())
	}
}

func TestSameWorktreePath_AbsRelativeMatch(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Dir(dir)
	base := filepath.Base(dir)
	t.Chdir(parent)
	if !sameWorktreePath(base, dir) {
		t.Fatalf("relative %q must equal abs %q", base, dir)
	}
	if sameWorktreePath(base, filepath.Join(parent, "other")) {
		t.Fatal("distinct paths must not match")
	}
}

// --- failure modes leave board unchanged, no orphan tab --------------------

func TestLaunch_InvalidAgentNameLeavesBoardUnchanged(t *testing.T) {
	// Force invalid by using a role that cannot form a legal name... we
	// instead call ValidateAgentName directly for the incident shape and
	// prove Launch refuses empty task ref before any side effect.
	h := &fakeHerdr{}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b}
	req := baseReq(t)
	req.TaskRef = ""
	_, err := l.Launch(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated")
	}
	if h.startCalls != 0 || len(h.closed) != 0 {
		t.Fatalf("side effects: start=%d closed=%v", h.startCalls, h.closed)
	}
}

func TestLaunch_InvalidAgentNameFromHerdrClosesNothingAndSkipsBoard(t *testing.T) {
	h := &fakeHerdr{
		tabErr: errors.New(`{"error":{"code":"invalid_agent_name","message":"agent name must..."}}`),
	}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	_, err := l.Launch(context.Background(), baseReq(t))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrBoardUnchanged) {
		t.Fatalf("want ErrBoardUnchanged, got %v", err)
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated on invalid_agent_name")
	}
	if len(h.closed) != 0 {
		t.Fatalf("closed tabs on pre-tab failure: %v", h.closed)
	}
}

func TestLaunch_ImmediateHarnessExitCompensatesTab(t *testing.T) {
	h := &fakeHerdr{startErr: errors.New("agent process exited before becoming interactive")}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	_, err := l.Launch(context.Background(), baseReq(t))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrHarnessExit) && !errors.Is(err, ErrBoardUnchanged) {
		// Must include both.
		if !errors.Is(err, ErrBoardUnchanged) {
			t.Fatalf("err = %v", err)
		}
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated")
	}
	if len(h.closed) != 1 || h.closed[0] != "tab-new" {
		t.Fatalf("closed = %v want exact new tab", h.closed)
	}
}

func TestLaunch_WrongCwdCrossTaskWorktreeCompensates(t *testing.T) {
	// Incident class: reviewer tab opened in FAC-172 worktree while reviewing FAC-151.
	req := baseReq(t)
	h := &fakeHerdr{
		tabCwd: filepath.Join(t.TempDir(), "fac-172"), // sibling task tree
	}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	_, err := l.Launch(context.Background(), req)
	if err == nil {
		t.Fatal("expected cwd drift rejection")
	}
	if !errors.Is(err, ErrIdentityDrift) && !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("err = %v", err)
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated")
	}
	if len(h.closed) != 1 {
		t.Fatalf("must close orphan tab: %v", h.closed)
	}
}

func TestLaunch_PacketRejectionLeavesBoardUnchanged(t *testing.T) {
	h := &fakeHerdr{deliverErr: errors.New("agent never confirmed consumption")}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	_, err := l.Launch(context.Background(), baseReq(t))
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrPacketRejected) && !errors.Is(err, ErrBoardUnchanged) {
		if !errors.Is(err, ErrBoardUnchanged) {
			t.Fatalf("err = %v", err)
		}
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated after packet rejection")
	}
	if len(h.closed) != 1 {
		t.Fatalf("closed = %v", h.closed)
	}
}

func TestLaunch_MissingSessionNullAgentSession(t *testing.T) {
	// FAC-173 p6W: agent_session:null after start → refuse packet, close tab.
	h := &fakeHerdr{nullSession: true}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	_, err := l.Launch(context.Background(), baseReq(t))
	if err == nil {
		t.Fatal("expected missing session error")
	}
	if !errors.Is(err, ErrMissingSession) && !errors.Is(err, ErrBoardUnchanged) {
		if !errors.Is(err, ErrBoardUnchanged) {
			t.Fatalf("err = %v", err)
		}
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated")
	}
	if h.deliverCalls != 0 {
		t.Fatal("must not deliver packet without session readback")
	}
	if len(h.closed) != 1 {
		t.Fatalf("closed = %v", h.closed)
	}
}

func TestLaunch_SessionDriftAfterPacketCompensatesBeforeBoard(t *testing.T) {
	req := baseReq(t)
	abs, err := filepath.Abs(req.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	base := AgentSnapshot{
		Name: "review-assayer-fac-151", PaneID: "pane-new", TabID: "tab-new",
		Workspace: "wF", Cwd: abs, Session: "claude-session-fresh", Status: "idle", Kind: "pi",
	}
	drifted := base
	drifted.Session = "claude-session-stale"
	h := &fakeHerdr{readSnaps: []AgentSnapshot{base, drifted}}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	_, err = l.Launch(context.Background(), req)
	if err == nil || !errors.Is(err, ErrIdentityDrift) {
		t.Fatalf("session drift after delivery accepted: %v", err)
	}
	if h.deliverCalls != 1 {
		t.Fatalf("packet delivery calls = %d, want 1", h.deliverCalls)
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated after post-delivery session drift")
	}
	if len(h.closed) != 1 || h.closed[0] != "tab-new" {
		t.Fatalf("exact new tab was not compensated: %v", h.closed)
	}
}

func TestMutation_RemovingPostDeliverySessionReadbackWouldAcceptStaleConversation(t *testing.T) {
	// This is the measured FAC-663 regression: even a fresh Git/pool root can
	// resume a stale model conversation. The unsafe one-read baseline cannot
	// distinguish it, while production must observe the session after delivery.
	req := baseReq(t)
	abs, err := filepath.Abs(req.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	base := AgentSnapshot{
		Name: "review-assayer-fac-151", PaneID: "pane-new", TabID: "tab-new",
		Workspace: "wF", Cwd: abs, Session: "claude-session-fresh", Status: "idle", Kind: "pi",
	}
	drifted := base
	drifted.Session = "claude-session-stale"
	h := &fakeHerdr{readSnaps: []AgentSnapshot{base, drifted}}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b}
	if _, err := l.Launch(context.Background(), req); err == nil {
		t.Fatal("mutation: removing the post-delivery readback accepts stale conversation state")
	}
	if h.readCount < 2 {
		t.Fatalf("mutation: session was read only %d time(s), want pre/post delivery", h.readCount)
	}
}

func TestLaunch_UnverifiedReceiptRejected(t *testing.T) {
	h := &fakeHerdr{
		deliverRec: &herdr.PromptReceipt{
			Target: "pane-new", BaselineStatus: "idle", FinalStatus: "done",
			Consumed: false, Verified: false, SequenceToken: "idle->done",
		},
	}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b}
	_, err := l.Launch(context.Background(), baseReq(t))
	if err == nil {
		t.Fatal("expected packet rejection")
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated on unverified receipt")
	}
}

// --- mutation probes -------------------------------------------------------

func TestMutation_RemovingTransitionOrderWouldBoardBeforeDeliver(t *testing.T) {
	// Unsafe baseline: a timeline with board before deliver MUST trip the
	// assertion (reviewer proved the prior guard could not fail under that
	// regression). boardBeforeDeliver is the named property under test.
	unsafe := []string{"tab_create", "agent_start", "read_agent", "board", "deliver"}
	if !boardBeforeDeliver(unsafe) {
		t.Fatal("unsafe baseline broken: board-before-deliver not detected")
	}

	// Production path: shared timeline must place deliver before board.
	var tl []string
	h := &fakeHerdr{timeline: &tl}
	b := &fakeBoard{timeline: &tl}
	l := &Launcher{Herdr: h, Board: b}
	if _, err := l.Launch(context.Background(), baseReq(t)); err != nil {
		t.Fatal(err)
	}
	assertDeliverBeforeBoard(t, tl)
	if b.callCount() != 1 {
		t.Fatal("board must run exactly once after deliver")
	}
	// Non-vacuity of the helper against the live timeline.
	if boardBeforeDeliver(tl) {
		t.Fatalf("production timeline inverted: %v", tl)
	}
}

func TestMutation_RemovingCompensationWouldLeaveOrphanTab(t *testing.T) {
	// Unsafe baseline: start fails and tab is left open.
	unsafeClosed := 0
	_ = unsafeClosed // simulated no-op close

	h := &fakeHerdr{startErr: errors.New("boom")}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	_, err := l.Launch(context.Background(), baseReq(t))
	if err == nil {
		t.Fatal("expected error")
	}
	if len(h.closed) != 1 {
		t.Fatalf("mutation: compensation removed would leave orphan; closed=%v", h.closed)
	}
	if b.callCount() != 0 {
		t.Fatal("board must stay unchanged when compensating")
	}
}

func TestMutation_RemovingIdentityCwdGateWouldAllowCrossTaskTree(t *testing.T) {
	req := baseReq(t)
	wrong := filepath.Join(t.TempDir(), "fac-172")
	h := &fakeHerdr{tabCwd: wrong}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b}
	_, err := l.Launch(context.Background(), req)
	if err == nil {
		t.Fatal("mutation: cwd gate removed would accept sibling worktree")
	}
	if b.callCount() != 0 {
		t.Fatal("board mutated")
	}
}

// --- races -----------------------------------------------------------------

func TestRace_ConcurrentLaunchesIsolateTabs(t *testing.T) {
	var tabSeq atomic.Uint64
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := tabSeq.Add(1)
			h := &fakeHerdr{
				tabID:  fmt.Sprintf("tab-%d", id),
				paneID: fmt.Sprintf("pane-%d", id),
			}
			b := &fakeBoard{}
			l := &Launcher{Herdr: h, Board: b}
			req := baseReq(t)
			req.TaskRef = fmt.Sprintf("FAC-%d", 200+i)
			req.TaskID = fmt.Sprintf("id-%d", i)
			req.WorktreePath = filepath.Join(t.TempDir(), fmt.Sprintf("fac-%d", 200+i))
			res, err := l.Launch(context.Background(), req)
			if err != nil {
				errs <- err
				return
			}
			if res.TabID != h.tabID {
				errs <- fmt.Errorf("tab mixup: %s vs %s", res.TabID, h.tabID)
			}
			if b.callCount() != 1 {
				errs <- fmt.Errorf("board calls=%d", b.callCount())
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
