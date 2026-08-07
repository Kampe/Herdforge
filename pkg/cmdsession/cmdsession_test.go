package cmdsession

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "command-sessions.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// fakeHost is the exact process-lifecycle fake: it models presence, parentage,
// and start tokens for a fixed set of PIDs, and counts every descriptor close
// and wait per session so a test can assert "closed everything, waited exactly
// once" rather than trusting a summary.
type fakeHost struct {
	mu     sync.Mutex
	procs  map[int]Observation
	closes map[string]int
	waits  map[string]int

	// stuck maps a session key to descriptor kinds that refuse to close.
	stuck map[string][]string
	// closeErr / waitErr force a failure for a session key.
	closeErr map[string]error
	waitErr  map[string]error
	// probeErr forces an unreadable probe for a PID.
	probeErr map[int]error
	exit     map[string]int
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		procs: map[int]Observation{}, closes: map[string]int{}, waits: map[string]int{},
		stuck: map[string][]string{}, closeErr: map[string]error{}, waitErr: map[string]error{},
		probeErr: map[int]error{}, exit: map[string]int{},
	}
}

func (h *fakeHost) spawn(pid, ppid int, token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.procs[pid] = Observation{Present: true, ParentPID: ppid, StartToken: token}
}

func (h *fakeHost) vanish(pid int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.procs, pid)
}

func (h *fakeHost) probe(id Identity) (Observation, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.probeErr[id.PID]; err != nil {
		return Observation{}, err
	}
	obs, ok := h.procs[id.PID]
	if !ok {
		return Observation{}, nil
	}
	return obs, nil
}

func (h *fakeHost) closer(id Identity) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := id.Key.String()
	h.closes[key]++
	if err := h.closeErr[key]; err != nil {
		return nil, err
	}
	return h.stuck[key], nil
}

func (h *fakeHost) waiter(id Identity) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := id.Key.String()
	h.waits[key]++
	if err := h.waitErr[key]; err != nil {
		return 0, err
	}
	// A waited session is collected: it no longer exists on the host.
	delete(h.procs, id.PID)
	return h.exit[key], nil
}

func (h *fakeHost) closeCount(key Key) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closes[key.String()]
}

func (h *fakeHost) waitCount(key Key) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.waits[key.String()]
}

func ident(session, call string, pid, ppid int, token string) Identity {
	return Identity{
		Key:           Key{CoordinatorSession: session, ToolCallID: call},
		PID:           pid,
		ParentPID:     ppid,
		StartToken:    token,
		PTY:           fmt.Sprintf("ttys%03d", pid%1000),
		CommandDigest: "sha256:" + call,
		WorkingDir:    "./.herd/worktrees/fac-193",
		TaskRef:       "FAC-193",
		StartedAt:     time.Now().UTC().Add(-2 * time.Hour),
	}
}

// register spawns a session on the fake host and records its receipt.
func register(t *testing.T, s *Store, h *fakeHost, id Identity) Identity {
	t.Helper()
	h.spawn(id.PID, id.ParentPID, id.StartToken)
	if _, err := s.Register(id); err != nil {
		t.Fatalf("Register(%s): %v", id.Key, err)
	}
	return id
}

// TestReconcileReapsRetainedShellsAndPreservesLiveBoundedCLI is the
// production-shaped fixture from the FAC-193 audit: six completed shell/PTY
// command sessions still alive at zero CPU, plus one live bounded CLI child.
// Reconciliation must reap exactly the six and leave the live child entirely
// untouched — not closed, not waited, not signalled.
func TestReconcileReapsRetainedShellsAndPreservesLiveBoundedCLI(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	const coordinator = 900

	var retained []Identity
	for i, call := range []string{"git-status", "kaneo-list", "tag-push", "worktree-add", "git-log", "kaneo-update"} {
		id := register(t, s, h, ident("coord-1", call, 1000+i, coordinator, fmt.Sprintf("token-%d", i)))
		if err := s.MarkCompleted(id.Key, OutcomeNormal, nil); err != nil {
			t.Fatalf("MarkCompleted(%s): %v", id.Key, err)
		}
		retained = append(retained, id)
	}
	// The one genuinely live child: a bounded CLI still running, so its tool
	// call has recorded no terminal outcome.
	live := register(t, s, h, ident("coord-1", "code-review-graph-update", 2000, coordinator, "token-live"))

	rep, err := Reconcile(s, h.probe, h.closer, h.waiter)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Reaped) != 6 {
		t.Fatalf("reaped %d sessions, want exactly 6: %+v", len(rep.Reaped), rep)
	}
	if len(rep.Preserved) != 1 || rep.Preserved[0] != live.Key.String() {
		t.Fatalf("preserved = %v, want exactly the live bounded CLI %s", rep.Preserved, live.Key)
	}
	if len(rep.Blocked) != 0 || len(rep.Settled) != 0 || len(rep.Deferred) != 0 {
		t.Fatalf("unexpected non-reap dispositions: %+v", rep)
	}
	for _, id := range retained {
		if got := h.closeCount(id.Key); got != 1 {
			t.Errorf("%s: closed %d times, want 1", id.Key, got)
		}
		if got := h.waitCount(id.Key); got != 1 {
			t.Errorf("%s: waited %d times, want 1", id.Key, got)
		}
	}
	// The live child must not have been touched at all.
	if got := h.closeCount(live.Key); got != 0 {
		t.Errorf("live bounded CLI was closed %d times, want 0", got)
	}
	if got := h.waitCount(live.Key); got != 0 {
		t.Errorf("live bounded CLI was waited %d times, want 0", got)
	}
	r, err := s.Get(live.Key)
	if err != nil {
		t.Fatalf("Get(live): %v", err)
	}
	if r.State != StateRunning {
		t.Errorf("live bounded CLI state = %s, want %s", r.State, StateRunning)
	}

	// Nothing but the live child is retained afterwards.
	left, err := s.ListRetained()
	if err != nil {
		t.Fatalf("ListRetained: %v", err)
	}
	if len(left) != 1 || left[0].Key != live.Key {
		t.Fatalf("retained after sweep = %d rows, want only the live child", len(left))
	}
}

// TestEveryCompletionPathClosesAndWaitsExactlyOnce covers normal, non-zero
// exit, canceled, timed-out, coordinator-crash and lost-readback completions.
// Each must close every descriptor and wait exactly once — a runner that
// omits the wait leaves the retained terminal this ticket exists to kill.
func TestEveryCompletionPathClosesAndWaitsExactlyOnce(t *testing.T) {
	cases := []struct {
		outcome string
		exit    int
	}{
		{OutcomeNormal, 0},
		{OutcomeNonZeroExit, 2},
		{OutcomeCanceled, 130},
		{OutcomeTimedOut, 124},
		{OutcomeCoordinatorCrash, 137},
		{OutcomeLostReadback, 0},
	}
	for i, tc := range cases {
		t.Run(tc.outcome, func(t *testing.T) {
			s, h := testStore(t), newFakeHost()
			id := register(t, s, h, ident("coord-1", "call-"+tc.outcome, 3000+i, 900, "tok"))
			h.exit[id.Key.String()] = tc.exit
			code := tc.exit
			if err := s.MarkCompleted(id.Key, tc.outcome, &code); err != nil {
				t.Fatalf("MarkCompleted: %v", err)
			}

			d, err := Reap(s, id.Key, h.probe, h.closer, h.waiter)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			if d != DispositionReaped {
				t.Fatalf("disposition = %s, want %s", d, DispositionReaped)
			}
			// A second sweep must not wait again: exec.Cmd.Wait is not safe
			// to call twice, and a double wait can collect an unrelated child.
			if d, err := Reap(s, id.Key, h.probe, h.closer, h.waiter); err != nil || d != DispositionReaped {
				t.Fatalf("second Reap = (%s, %v), want (%s, nil)", d, err, DispositionReaped)
			}
			if got := h.closeCount(id.Key); got != 1 {
				t.Errorf("closed %d times, want exactly 1", got)
			}
			if got := h.waitCount(id.Key); got != 1 {
				t.Errorf("waited %d times, want exactly 1", got)
			}
			r, err := s.Get(id.Key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if r.State != StateReaped {
				t.Fatalf("state = %s, want %s", r.State, StateReaped)
			}
			if r.ReapedAt == nil {
				t.Error("reaped receipt has no reaped_at timestamp")
			}
			if r.Outcome != tc.outcome {
				t.Errorf("outcome = %q, want %q", r.Outcome, tc.outcome)
			}
			if r.ExitCode == nil || *r.ExitCode != tc.exit {
				t.Errorf("exit code = %v, want %d", r.ExitCode, tc.exit)
			}
			if r.Retained() {
				t.Error("a reaped session still counts as retained")
			}
		})
	}
}

// TestAmbiguityPreventsCleanupAndEmitsBlockedEvidence: every ambiguous
// identity or completion signal must leave the session untouched with a
// durable reason. A reaper that trusts the PID alone passes none of these.
func TestAmbiguityPreventsCleanupAndEmitsBlockedEvidence(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(t *testing.T, s *Store, h *fakeHost, id Identity)
		wantInMsg string
	}{
		{
			name: "start token mismatch means the pid was reused",
			setup: func(t *testing.T, s *Store, h *fakeHost, id Identity) {
				h.spawn(id.PID, id.ParentPID, "a-totally-different-process")
			},
			wantInMsg: "pid was reused",
		},
		{
			name: "unknown parentage",
			setup: func(t *testing.T, s *Store, h *fakeHost, id Identity) {
				h.spawn(id.PID, id.ParentPID+77, id.StartToken)
			},
			wantInMsg: "unknown parentage",
		},
		{
			name: "unreadable probe",
			setup: func(t *testing.T, s *Store, h *fakeHost, id Identity) {
				h.probeErr[id.PID] = errors.New("ps unavailable")
			},
			wantInMsg: "identity probe failed",
		},
		{
			name: "output after the terminal receipt",
			setup: func(t *testing.T, s *Store, h *fakeHost, id Identity) {
				if err := s.NoteOutput(id.Key, time.Now().UTC().Add(time.Second)); err != nil {
					t.Fatalf("NoteOutput: %v", err)
				}
			},
			wantInMsg: "completion is ambiguous",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, h := testStore(t), newFakeHost()
			id := register(t, s, h, ident("coord-1", fmt.Sprintf("ambiguous-%d", i), 4000+i, 900, "tok"))
			if err := s.MarkCompleted(id.Key, OutcomeNormal, nil); err != nil {
				t.Fatalf("MarkCompleted: %v", err)
			}
			tc.setup(t, s, h, id)

			d, err := Reap(s, id.Key, h.probe, h.closer, h.waiter)
			if err != nil {
				t.Fatalf("Reap: %v", err)
			}
			if d != DispositionBlocked {
				t.Fatalf("disposition = %s, want %s", d, DispositionBlocked)
			}
			if got := h.closeCount(id.Key); got != 0 {
				t.Errorf("descriptors closed %d times on an ambiguous session, want 0", got)
			}
			if got := h.waitCount(id.Key); got != 0 {
				t.Errorf("waited %d times on an ambiguous session, want 0", got)
			}
			r, err := s.Get(id.Key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if r.State != StateBlocked {
				t.Fatalf("state = %s, want %s", r.State, StateBlocked)
			}
			if !strings.Contains(r.BlockedReason, tc.wantInMsg) {
				t.Errorf("blocked reason %q does not contain %q", r.BlockedReason, tc.wantInMsg)
			}

			// The evidence is durable: a second store over the same file
			// still refuses and still carries the reason.
			d2, err := Reap(s, id.Key, h.probe, h.closer, h.waiter)
			if err != nil || d2 != DispositionBlocked {
				t.Fatalf("re-sweep = (%s, %v), want blocked", d2, err)
			}
			if h.waitCount(id.Key) != 0 {
				t.Error("a re-sweep waited a blocked session")
			}
		})
	}
}

// TestDescriptorLeftOpenBlocksTheReap: a PTY writer that survives the close
// means the session is not finished, so the wait must not happen.
func TestDescriptorLeftOpenBlocksTheReap(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	id := register(t, s, h, ident("coord-1", "pty-writer", 5000, 900, "tok"))
	h.stuck[id.Key.String()] = []string{"pty"}
	if err := s.MarkCompleted(id.Key, OutcomeNormal, nil); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	d, err := Reap(s, id.Key, h.probe, h.closer, h.waiter)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if d != DispositionBlocked {
		t.Fatalf("disposition = %s, want %s", d, DispositionBlocked)
	}
	if got := h.waitCount(id.Key); got != 0 {
		t.Errorf("waited %d times with a descriptor still open, want 0", got)
	}
	r, _ := s.Get(id.Key)
	if !strings.Contains(r.BlockedReason, "descriptors still open") || !strings.Contains(r.BlockedReason, "pty") {
		t.Errorf("blocked reason %q does not name the open pty descriptor", r.BlockedReason)
	}
}

// TestReapRefusesSessionWithNoTerminalOutcome: live work is never torn down
// through the reap path, whatever a caller asks for.
func TestReapRefusesSessionWithNoTerminalOutcome(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	id := register(t, s, h, ident("coord-1", "still-running", 5100, 900, "tok"))

	if _, err := Reap(s, id.Key, h.probe, h.closer, h.waiter); !errors.Is(err, ErrNotCompleted) {
		t.Fatalf("Reap of a running session = %v, want ErrNotCompleted", err)
	}
	if h.closeCount(id.Key)+h.waitCount(id.Key) != 0 {
		t.Error("a running session was closed or waited")
	}
}

// TestPIDReuseAtRegistrationBlocksTheStaleReceipt: once a PID is handed to a
// new command session, the older receipt bound to it can never be re-proved,
// so it must become BLOCKED evidence rather than a future reap target.
func TestPIDReuseAtRegistrationBlocksTheStaleReceipt(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	stale := register(t, s, h, ident("coord-1", "old-call", 6000, 900, "token-old"))
	if err := s.MarkCompleted(stale.Key, OutcomeNormal, nil); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	fresh := register(t, s, h, ident("coord-1", "new-call", 6000, 900, "token-new"))

	r, err := s.Get(stale.Key)
	if err != nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if r.State != StateBlocked {
		t.Fatalf("stale receipt state = %s, want %s", r.State, StateBlocked)
	}
	if !strings.Contains(r.BlockedReason, "reused") {
		t.Errorf("blocked reason %q does not record the pid reuse", r.BlockedReason)
	}
	if fr, err := s.Get(fresh.Key); err != nil || fr.State != StateRunning {
		t.Fatalf("fresh receipt = %+v, %v; want running", fr, err)
	}
}

// TestRegisterRefusesRebindingAKeyToADifferentIdentity.
func TestRegisterRefusesRebindingAKeyToADifferentIdentity(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	id := register(t, s, h, ident("coord-1", "call", 6100, 900, "tok"))

	if _, err := s.Register(id); err != nil {
		t.Fatalf("idempotent re-register: %v", err)
	}
	moved := id
	moved.PID = 6101
	if _, err := s.Register(moved); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("rebinding key to a new pid = %v, want ErrIdentityCollision", err)
	}
	if _, err := s.Register(Identity{Key: Key{CoordinatorSession: "c", ToolCallID: "t"}, PID: 1}); !errors.Is(err, ErrIncompleteIdentity) {
		t.Fatalf("incomplete identity = %v, want ErrIncompleteIdentity", err)
	}
}

// TestDetachedWriterKeepsTheSessionOwnedUntilItSettles: a parent shell exiting
// cannot mint a terminal receipt for a detached background writer that is
// still running. The session stays owned and observable until the descendant
// is settled by exact identity.
func TestDetachedWriterKeepsTheSessionOwnedUntilItSettles(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	id := register(t, s, h, ident("coord-1", "detached-writer", 7000, 900, "tok"))
	if err := s.RegisterDetached(id.Key, 7001, "writer-token"); err != nil {
		t.Fatalf("RegisterDetached: %v", err)
	}
	if err := s.MarkCompleted(id.Key, OutcomeNormal, nil); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	rep, err := Reconcile(s, h.probe, h.closer, h.waiter)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Deferred) != 1 || rep.Deferred[0] != id.Key.String() {
		t.Fatalf("deferred = %v, want the session with the unsettled writer", rep.Deferred)
	}
	if h.waitCount(id.Key) != 0 {
		t.Error("the session was waited while its detached writer was still unaccounted for")
	}
	r, _ := s.Get(id.Key)
	if !r.Retained() {
		t.Error("a session with an unsettled detached writer stopped being retained")
	}
	if r.DetachedOpen != 1 {
		t.Errorf("detached_open = %d, want 1", r.DetachedOpen)
	}

	// Settling by PID alone must not work: a reused PID would release a
	// descendant that is still running.
	if err := s.SettleDetached(id.Key, 7001, "some-other-token"); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("settling with a wrong start token = %v, want a refusal", err)
	}
	if err := s.SettleDetached(id.Key, 7001, "writer-token"); err != nil {
		t.Fatalf("SettleDetached: %v", err)
	}

	rep, err = Reconcile(s, h.probe, h.closer, h.waiter)
	if err != nil {
		t.Fatalf("Reconcile after settle: %v", err)
	}
	if len(rep.Reaped) != 1 || rep.Reaped[0] != id.Key.String() {
		t.Fatalf("reaped = %v, want the settled session", rep.Reaped)
	}
	if h.waitCount(id.Key) != 1 {
		t.Errorf("waited %d times after settling, want exactly 1", h.waitCount(id.Key))
	}
}

// TestLostReadbackSessionSettlesWithoutClaimingAWait: a session whose PID is
// gone with no terminal receipt (the coordinator crashed) is recorded as a
// lost readback and settled — but this process never claims to have waited a
// child it did not own.
func TestLostReadbackSessionSettlesWithoutClaimingAWait(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	id := register(t, s, h, ident("coord-1", "crashed", 7100, 900, "tok"))
	h.vanish(id.PID)

	rep, err := Reconcile(s, h.probe, h.closer, h.waiter)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rep.Settled) != 1 || rep.Settled[0] != id.Key.String() {
		t.Fatalf("settled = %v, want the vanished session", rep.Settled)
	}
	if h.waitCount(id.Key) != 0 {
		t.Error("claimed a wait on a process this store never owned")
	}
	r, _ := s.Get(id.Key)
	if r.State != StateSettledAbsent {
		t.Fatalf("state = %s, want %s", r.State, StateSettledAbsent)
	}
	if r.Outcome != OutcomeLostReadback {
		t.Errorf("outcome = %q, want %q", r.Outcome, OutcomeLostReadback)
	}
	if !strings.Contains(r.BlockedReason, "not waited by this process") {
		t.Errorf("settle evidence %q does not disclaim the wait", r.BlockedReason)
	}
}

// TestLaneOwnedChildIsNeverTornDownHere: FAC-188 owns lane-bound tool
// children. This package tracks one for visibility and must never close or
// wait it, so neither boundary is duplicated or weakened.
func TestLaneOwnedChildIsNeverTornDownHere(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	id := ident("coord-1", "lane-tool-child", 7200, 900, "tok")
	id.LaneOwned = true
	register(t, s, h, id)
	if err := s.MarkCompleted(id.Key, OutcomeNormal, nil); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	d, err := Reap(s, id.Key, h.probe, h.closer, h.waiter)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if d != DispositionPreservedLive {
		t.Fatalf("disposition = %s, want %s", d, DispositionPreservedLive)
	}
	if h.closeCount(id.Key)+h.waitCount(id.Key) != 0 {
		t.Error("a lane-owned tool child was closed or waited by cmdsession")
	}
}

// TestRepeatedCommandWavesLeaveNoRetainedTerminals runs several waves of
// commands through the full lifecycle while one bounded CLI keeps running
// across all of them. After the last sweep nothing is retained except that
// CLI, and it was never interrupted.
func TestRepeatedCommandWavesLeaveNoRetainedTerminals(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	live := register(t, s, h, ident("coord-1", "long-cli", 8000, 900, "tok-live"))

	pid := 8100
	for wave := 0; wave < 4; wave++ {
		for i := 0; i < 5; i++ {
			pid++
			id := register(t, s, h, ident("coord-1", fmt.Sprintf("w%d-c%d", wave, i), pid, 900, fmt.Sprintf("tok-%d", pid)))
			if err := s.MarkCompleted(id.Key, OutcomeNormal, nil); err != nil {
				t.Fatalf("MarkCompleted: %v", err)
			}
		}
		rep, err := Reconcile(s, h.probe, h.closer, h.waiter)
		if err != nil {
			t.Fatalf("wave %d Reconcile: %v", wave, err)
		}
		if len(rep.Reaped) != 5 {
			t.Fatalf("wave %d reaped %d, want 5: %+v", wave, len(rep.Reaped), rep)
		}
		if len(rep.Blocked) != 0 {
			t.Fatalf("wave %d produced BLOCKED evidence: %v", wave, rep.Blocked)
		}
	}

	retained, err := s.ListRetained()
	if err != nil {
		t.Fatalf("ListRetained: %v", err)
	}
	if len(retained) != 1 || retained[0].Key != live.Key {
		t.Fatalf("retained after 4 waves = %d rows, want only the long-running CLI", len(retained))
	}
	if h.closeCount(live.Key)+h.waitCount(live.Key) != 0 {
		t.Error("the long-running CLI was interrupted by a sweep")
	}
}

// TestConcurrentRegistrationDuringReconcile is the focused race test: run it
// with -race. Registration, completion and sweeping all touch the same store
// concurrently; no sweep may double-wait a session.
func TestConcurrentRegistrationDuringReconcile(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			id := ident("coord-1", fmt.Sprintf("racy-%d", i), 9000+i, 900, fmt.Sprintf("tok-%d", i))
			h.spawn(id.PID, id.ParentPID, id.StartToken)
			if _, err := s.Register(id); err != nil {
				t.Errorf("Register: %v", err)
				return
			}
			if err := s.MarkCompleted(id.Key, OutcomeNormal, nil); err != nil {
				t.Errorf("MarkCompleted: %v", err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := Reconcile(s, h.probe, h.closer, h.waiter); err != nil {
				t.Errorf("Reconcile: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	if _, err := Reconcile(s, h.probe, h.closer, h.waiter); err != nil {
		t.Fatalf("final Reconcile: %v", err)
	}
	all, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 40 {
		t.Fatalf("recorded %d sessions, want 40", len(all))
	}
	for _, r := range all {
		if r.State != StateReaped {
			t.Fatalf("%s ended in state %s, want %s (reason: %s)", r.Key, r.State, StateReaped, r.BlockedReason)
		}
		if got := h.waitCount(r.Key); got != 1 {
			t.Fatalf("%s waited %d times under concurrency, want exactly 1", r.Key, got)
		}
	}
}

// TestSummarizeExposesCountAgeTaskAndDisposition: fleet status must be able to
// show retained command sessions so they cannot hide behind an agent-level
// working state.
func TestSummarizeExposesCountAgeTaskAndDisposition(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	old := ident("coord-1", "old", 9500, 900, "tok-old")
	old.StartedAt = time.Now().UTC().Add(-6*time.Hour - 30*time.Minute)
	register(t, s, h, old)
	if err := s.MarkCompleted(old.Key, OutcomeNormal, nil); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
	recent := ident("coord-1", "recent", 9501, 900, "tok-recent")
	recent.StartedAt = time.Now().UTC().Add(-time.Minute)
	register(t, s, h, recent)

	sum, err := s.Summarize(time.Now)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Retained != 2 {
		t.Fatalf("retained = %d, want 2", sum.Retained)
	}
	if sum.OldestAgeSeconds < int64((6 * time.Hour).Seconds()) {
		t.Errorf("oldest age = %ds, want at least 6h", sum.OldestAgeSeconds)
	}
	if len(sum.Rows) != 2 || sum.Rows[0].Key != old.Key {
		t.Fatalf("rows not ordered oldest first: %+v", sum.Rows)
	}
	if sum.Rows[0].TaskRef != "FAC-193" || sum.Rows[0].State != StateCompleted {
		t.Errorf("row 0 = %+v, want the completed FAC-193 session", sum.Rows[0])
	}

	// A BLOCKED session is surfaced too, with its reason.
	if err := s.MarkBlocked(recent.Key, "ambiguous parentage"); err != nil {
		t.Fatalf("MarkBlocked: %v", err)
	}
	sum, err = s.Summarize(time.Now)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sum.Retained != 1 || sum.Blocked != 1 {
		t.Fatalf("retained=%d blocked=%d, want 1 and 1", sum.Retained, sum.Blocked)
	}
	var sawReason bool
	for _, row := range sum.Rows {
		if row.Key == recent.Key && row.BlockedReason == "ambiguous parentage" {
			sawReason = true
		}
	}
	if !sawReason {
		t.Error("summary does not carry the BLOCKED reason")
	}
}

// TestForeignSweepCannotClaimAReap: an out-of-process sweep holds no
// descriptors and is not the session's parent, so a still-present retained
// terminal becomes BLOCKED evidence instead of a fabricated reap.
func TestForeignSweepCannotClaimAReap(t *testing.T) {
	s, h := testStore(t), newFakeHost()
	id := register(t, s, h, ident("coord-1", "foreign", 9600, 900, "tok"))
	if err := s.MarkCompleted(id.Key, OutcomeNormal, nil); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}

	d, err := Reap(s, id.Key, h.probe, ForeignCloser, ForeignWaiter)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if d != DispositionBlocked {
		t.Fatalf("disposition = %s, want %s", d, DispositionBlocked)
	}
	r, _ := s.Get(id.Key)
	if !strings.Contains(r.BlockedReason, "not owned by this process") {
		t.Errorf("blocked reason %q does not explain the foreign ownership", r.BlockedReason)
	}
}

// TestExecSeamsCloseDescriptorsAndWaitExactlyOnce exercises the production
// in-process seams against a real short-lived child: the pipe is closed and
// the child is waited once, yielding its true exit code.
func TestExecSeamsCloseDescriptorsAndWaitExactlyOnce(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	closer, waiter := ExecSeams(cmd, map[string]io.Closer{"stdout": stdout})

	id := ident("coord-1", "real-child", cmd.Process.Pid, 900, "tok")
	stillOpen, err := closer(id)
	if err != nil {
		t.Fatalf("closer: %v", err)
	}
	if len(stillOpen) != 0 {
		t.Fatalf("descriptors still open after close: %v", stillOpen)
	}
	code, err := waiter(id)
	if err != nil {
		t.Fatalf("waiter: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	// Calling Wait twice on an exec.Cmd is an error; the seam must absorb it.
	if code2, err := waiter(id); err != nil || code2 != 7 {
		t.Fatalf("second wait = (%d, %v), want (7, nil)", code2, err)
	}
}
