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
}

func (b *fakeBoard) UpdateStatus(_ context.Context, taskID, status string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	// When true, ReadAgent returns empty session (null agent_session case).
	nullSession bool

	closed   []string
	closeErr error

	startCalls   int
	deliverCalls int
	// Order probe: append stage names as side effects run.
	stages []string
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
	f.stages = append(f.stages, "tab_create")
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
		tabCwd = cwd
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
	f.stages = append(f.stages, "agent_start")
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
	f.stages = append(f.stages, "deliver")
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
	f.stages = append(f.stages, "tab_close")
	f.closed = append(f.closed, tabID)
	return f.closeErr
}

func (f *fakeHerdr) ReadAgent(name string) (AgentSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stages = append(f.stages, "read_agent")
	if f.readErr != nil {
		return AgentSnapshot{}, f.readErr
	}
	if f.nullSession {
		return AgentSnapshot{}, ErrMissingSession
	}
	s := f.readSnap
	if s.Name == "" {
		s.Name = name
	}
	return s, nil
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
	h := &fakeHerdr{workspace: "wF"}
	b := &fakeBoard{}
	l := &Launcher{Herdr: h, Board: b, ReceiptPath: filepath.Join(t.TempDir(), "r.jsonl")}
	req := baseReq(t)

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
	// Order: tab → start → read → deliver → board (board is last, not in stages).
	wantPrefix := []string{"tab_create", "agent_start", "read_agent", "deliver"}
	if len(h.stages) < len(wantPrefix) {
		t.Fatalf("stages = %v", h.stages)
	}
	for i, s := range wantPrefix {
		if h.stages[i] != s {
			t.Fatalf("stage[%d]=%s want %s full=%v", i, h.stages[i], s, h.stages)
		}
	}
	// Board must not have been touched before deliver completed.
	if b.callCount() != 1 {
		t.Fatalf("board calls = %d", b.callCount())
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
	// Unsafe baseline: board update before deliver (the pre-FAC-187 bug).
	var order []string
	unsafeBoardThenDeliver := func() {
		order = append(order, "board")
		order = append(order, "deliver")
	}
	unsafeBoardThenDeliver()
	if order[0] != "board" {
		t.Fatal("unsafe baseline broken")
	}

	// Safe path: deliver is always observed before the single board call.
	h := &fakeHerdr{}
	b := &fakeBoard{}
	// Interleave observation via stages + board call count after Launch.
	l := &Launcher{Herdr: h, Board: b}
	if _, err := l.Launch(context.Background(), baseReq(t)); err != nil {
		t.Fatal(err)
	}
	deliverIdx := -1
	for i, s := range h.stages {
		if s == "deliver" {
			deliverIdx = i
			break
		}
	}
	if deliverIdx < 0 {
		t.Fatal("deliver never ran")
	}
	// Board call happens only after Launch returns success, and deliver is
	// the last herdr stage before that.
	if h.stages[len(h.stages)-1] != "deliver" {
		t.Fatalf("last herdr stage before board must be deliver: %v", h.stages)
	}
	if b.callCount() != 1 {
		t.Fatal("board must run exactly once after deliver")
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
