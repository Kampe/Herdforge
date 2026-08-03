package verifier

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// processGroupKiller reaps process-group members for Cancel-time use only
// while the original leader is still live. It identity-kills members of pgid
// that are causally under the owned tree when called from KillGroupLive.
// Tests may replace it with a leader-only killer for mutation proofs.
var processGroupKiller = killProcessGroupMembers

// finalizeOwnedTree is the production ownership close after Wait.
var finalizeOwnedTree = (*ownedSubprocess).Close

// processGroupGoneBound is the fail-closed deadline for tracker stop and
// post-kill liveness polls.
const processGroupGoneBound = 500 * time.Millisecond

// trackSampleInterval is the background tracker cadence (native snapshot only).
const trackSampleInterval = 5 * time.Millisecond

// handshakeReadBound is how long adopt waits for the ownership wrapper to
// report the user-command child PID on the control pipe.
const handshakeReadBound = 2 * time.Second

// ErrResidualOwnedTree means live owned writers existed at close.
var ErrResidualOwnedTree = errors.New("residual owned process tree members after leader wait")

// ErrResidualProcessGroup aliases residual for older tests.
var ErrResidualProcessGroup = ErrResidualOwnedTree

// killProcessGroupMembers identity-kills current members of pgid discovered
// via a single native snapshot. Used only while the original leader is live
// (Cancel / pre-Wait drain). Never signals -pgid.
func killProcessGroupMembers(pgid int) error {
	if pgid <= 0 {
		return fmt.Errorf("kill process group members: invalid pgid %d", pgid)
	}
	snap, err := snapshotProcesses()
	if err != nil {
		return fmt.Errorf("kill process group members: snapshot: %w", err)
	}
	var errs []error
	for _, tok := range snap.membersOfGroup(pgid) {
		h, herr := openHandle(tok)
		if herr != nil {
			// Gone between snapshot and open — not an error.
			continue
		}
		if _, kerr := h.kill(); kerr != nil {
			errs = append(errs, kerr)
		}
		h.close()
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// KillProcessGroup is the exported membership kill (identity-bound, no -pgid).
func KillProcessGroup(pgid int) error {
	return processGroupKiller(pgid)
}

// killProcessGroupIfLive is the Cancel-safe name: identity-kill current members.
func killProcessGroupIfLive(pgid int) error {
	return processGroupKiller(pgid)
}

// ownedSubprocess tracks causally discovered process handles for one
// verification command. Sampling freezes when the leader dies; tokens are
// never replaced; Close kills only the frozen handle set.
type ownedSubprocess struct {
	cmd    *exec.Cmd
	leader int
	pgid   int

	mu       sync.Mutex
	handles  map[int]ownedHandle // pid -> handle at first causal discovery
	frozen   bool                // no further discovery after leader death / pre-Wait freeze
	trackErr error
	hadLive  bool // set if any non-leader was live at kill time

	// handshakeR is the read end of the ownership pipe (optional).
	handshakeR *os.File

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

// ownershipWrapperScript runs the user command as a child, reports the child
// PID on FD 3 (ExtraFiles[0]), then waits. This is a synchronized pre-exec
// containment handshake — not a polling race with fork+setsid.
//
//	$1 = user path; remaining args are user argv.
const ownershipWrapperScript = `
user_path="$1"
shift
"$user_path" "$@" &
child=$!
# Report child PID before wait so the parent can open a causal handle.
printf '%s\n' "$child" >&3
wait "$child"
ec=$?
exit "$ec"
`

// prepareOwnedCommand builds a Setpgid command under the ownership wrapper
// with a control pipe for the child-PID handshake.
func prepareOwnedCommand(ctx context.Context, path string, args []string, dir string, env []string) (*exec.Cmd, *os.File, *os.File, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ownership pipe: %w", err)
	}
	wrapArgs := append([]string{"-c", ownershipWrapperScript, "owned-wrap", path}, args...)
	cmd := exec.CommandContext(ctx, "sh", wrapArgs...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = []*os.File{pw}
	return cmd, pr, pw, nil
}

// adoptOwnedCmd begins ownership: records leader handle, reads handshake child,
// then tracks only BFS descendants of causally owned PIDs while leader is live.
func adoptOwnedCmd(cmd *exec.Cmd, handshakeR *os.File) (*ownedSubprocess, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("adopt owned cmd: nil process")
	}
	leader := cmd.Process.Pid
	tok, err := tokenOf(leader)
	if err != nil {
		return nil, fmt.Errorf("adopt owned cmd: leader token: %w", err)
	}
	h, err := openHandle(tok)
	if err != nil {
		return nil, fmt.Errorf("adopt owned cmd: leader handle: %w", err)
	}
	o := &ownedSubprocess{
		cmd:        cmd,
		leader:     leader,
		pgid:       leader,
		handles:    map[int]ownedHandle{leader: h},
		handshakeR: handshakeR,
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	// Synchronized handshake: child PID reported by wrapper before wait.
	var handshakeChild int
	if handshakeR != nil {
		var herr error
		handshakeChild, herr = o.readHandshakeChild(handshakeReadBound)
		if herr != nil {
			o.trackErr = herr
		}
	}
	// Causal sample while the handshake child is still live so grandchildren
	// (background/setsid writers) are discovered before reparent-on-exit.
	_ = o.sample()
	if handshakeChild > 1 {
		o.followUntilExit(handshakeChild, handshakeReadBound)
	}
	go o.trackLoop()
	return o, nil
}

// followUntilExit repeatedly samples the causal tree until childPid exits or
// the bound elapses. This is a synchronized follow of a handshake-owned PID,
// not a free-running cleanup sleep.
func (o *ownedSubprocess) followUntilExit(childPid int, bound time.Duration) {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		_ = o.sample()
		o.mu.Lock()
		h, ok := o.handles[childPid]
		o.mu.Unlock()
		if !ok || !h.tok.stillSame() {
			// Final sample after exit to catch last-moment forks.
			_ = o.sample()
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// readHandshakeChild returns the user-command PID from the ownership pipe.
func (o *ownedSubprocess) readHandshakeChild(bound time.Duration) (int, error) {
	if o.handshakeR == nil {
		return 0, nil
	}
	defer func() {
		_ = o.handshakeR.Close()
		o.handshakeR = nil
	}()
	_ = o.handshakeR.SetReadDeadline(time.Now().Add(bound))
	line, err := bufio.NewReader(o.handshakeR).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("ownership handshake read: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("ownership handshake bad pid %q", strings.TrimSpace(line))
	}
	tok, err := tokenOf(pid)
	if err != nil {
		return 0, fmt.Errorf("ownership handshake token: %w", err)
	}
	if err := o.noteCausal(tok); err != nil {
		return pid, err
	}
	return pid, nil
}

// noteCausal records a process only if not already tracked (never replaces).
func (o *ownedSubprocess) noteCausal(tok procToken) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.frozen {
		return nil
	}
	if _, exists := o.handles[tok.pid]; exists {
		// Never replace an observed incarnation.
		return nil
	}
	h, err := openHandle(tok)
	if err != nil {
		return err
	}
	o.handles[tok.pid] = h
	return nil
}

func (o *ownedSubprocess) trackLoop() {
	defer close(o.stopped)
	ticker := time.NewTicker(trackSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-ticker.C:
			if err := o.sample(); err != nil {
				o.mu.Lock()
				o.trackErr = err
				o.mu.Unlock()
			}
		}
	}
}

// sample discovers children of already-owned live processes only.
// It never seeds from current numeric pgid membership (reuse is not causal).
// When the leader is no longer the original incarnation, sampling freezes.
func (o *ownedSubprocess) sample() error {
	o.mu.Lock()
	if o.frozen {
		o.mu.Unlock()
		return nil
	}
	leaderH, ok := o.handles[o.leader]
	o.mu.Unlock()
	if !ok || !leaderH.tok.stillSame() {
		o.mu.Lock()
		o.frozen = true
		o.mu.Unlock()
		return nil
	}
	snap, err := snapshotProcesses()
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.frozen {
		return nil
	}
	// BFS only from currently owned PIDs whose incarnation is still live.
	queue := make([]int, 0, len(o.handles))
	for pid, h := range o.handles {
		if h.tok.stillSame() {
			queue = append(queue, pid)
		}
	}
	for i := 0; i < len(queue); i++ {
		pid := queue[i]
		for _, c := range snap.childrenOf(pid) {
			if _, exists := o.handles[c]; exists {
				continue // never replace
			}
			tok, ok := snap.token(c)
			if !ok {
				continue
			}
			h, herr := openHandle(tok)
			if herr != nil {
				continue
			}
			o.handles[c] = h
			queue = append(queue, c)
		}
	}
	return nil
}

func (o *ownedSubprocess) freeze() {
	o.mu.Lock()
	o.frozen = true
	o.mu.Unlock()
}

func (o *ownedSubprocess) stopTracker() error {
	o.stopOnce.Do(func() {
		close(o.stop)
	})
	select {
	case <-o.stopped:
		return nil
	case <-time.After(processGroupGoneBound):
		return fmt.Errorf("owned tree tracker stop deadline exceeded (%s)", processGroupGoneBound)
	}
}

// KillGroupLive drains causally owned non-leader processes and the live group
// of the original leader while the leader is still live (Cancel / pre-Wait).
func (o *ownedSubprocess) KillGroupLive() error {
	if o == nil {
		return errors.New("kill group live: nil owned subprocess")
	}
	// Final sample while leader may still be live.
	_ = o.sample()
	var errs []error
	if err := o.killTracked(false); err != nil {
		errs = append(errs, err)
	}
	// While leader is live, also membership-kill original pgid (Cancel path).
	o.mu.Lock()
	leaderH, ok := o.handles[o.leader]
	pgid := o.pgid
	o.mu.Unlock()
	if ok && leaderH.tok.stillSame() {
		if err := processGroupKiller(pgid); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// killTracked signals every causally owned handle except optionally the leader.
func (o *ownedSubprocess) killTracked(includeLeader bool) error {
	o.mu.Lock()
	handles := make([]ownedHandle, 0, len(o.handles))
	for pid, h := range o.handles {
		if !includeLeader && pid == o.leader {
			continue
		}
		handles = append(handles, h)
	}
	o.mu.Unlock()
	var errs []error
	for _, h := range handles {
		if h.tok.isLiveTarget() {
			o.mu.Lock()
			o.hadLive = true
			o.mu.Unlock()
		}
		if _, err := h.kill(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// DrainBeforeWait freezes discovery and kills non-leader owned processes while
// the leader is still unreaped (Darwin-safe window; Linux uses pidfd).
func (o *ownedSubprocess) DrainBeforeWait() error {
	if o == nil {
		return errors.New("drain before wait: nil")
	}
	_ = o.sample()
	o.freeze()
	return o.killTracked(false)
}

// Pgid returns the process group id recorded at Start.
func (o *ownedSubprocess) Pgid() int {
	if o == nil {
		return 0
	}
	return o.pgid
}

// Close freezes discovery, kills only causally owned handles (no numeric
// group re-adoption), and closes pidfds. Never signals -pgid.
func (o *ownedSubprocess) Close() error {
	if o == nil {
		return errors.New("close owned: nil")
	}
	o.freeze()
	stopErr := o.stopTracker()
	if err := o.killTracked(true); err != nil && stopErr == nil {
		stopErr = err
	}
	o.mu.Lock()
	handles := make([]ownedHandle, 0, len(o.handles))
	for _, h := range o.handles {
		handles = append(handles, h)
	}
	hadLive := o.hadLive
	trackErr := o.trackErr
	o.mu.Unlock()

	var killErrs []error
	if stopErr != nil {
		killErrs = append(killErrs, stopErr)
	}
	if trackErr != nil {
		killErrs = append(killErrs, fmt.Errorf("tree sample: %w", trackErr))
	}
	self := os.Getpid()
	for _, h := range handles {
		if h.tok.pid <= 1 || h.tok.pid == self {
			h.close()
			continue
		}
		if err := waitHandleGone(h, processGroupGoneBound); err != nil {
			h.close()
			return fmt.Errorf("close owned: %w", err)
		}
		h.close()
	}
	if len(killErrs) > 0 {
		return fmt.Errorf("close owned: %w", errors.Join(killErrs...))
	}
	if hadLive {
		return fmt.Errorf("%w: pgid=%d tracked=%d", ErrResidualOwnedTree, o.pgid, len(handles))
	}
	return nil
}

// Reap freezes discovery, kills every causally owned handle (including the
// leader) while identities are still valid, Waits the leader, then Close.
func (o *ownedSubprocess) Reap() error {
	if o == nil || o.cmd == nil {
		return errors.New("reap owned: nil")
	}
	_ = o.sample()
	o.freeze()
	// Kill entire owned set before Wait so Wait cannot hang on a live leader.
	killErr := o.killTracked(true)
	if leaderH, ok := o.handles[o.leader]; ok && leaderH.tok.stillSame() {
		// Membership kill only while original leader incarnation is live.
		if err := processGroupKiller(o.pgid); err != nil && killErr == nil {
			killErr = err
		} else if err != nil {
			killErr = errors.Join(killErr, err)
		}
	}
	waitErr := o.cmd.Wait()
	closeErr := finalizeOwnedTree(o)
	if o.cmd.ProcessState == nil {
		return fmt.Errorf("reap owned: wait without ProcessState: wait=%v kill=%v close=%v", waitErr, killErr, closeErr)
	}
	if killErr != nil {
		return fmt.Errorf("reap owned: kill: %w (wait=%v close=%v)", killErr, waitErr, closeErr)
	}
	if waitErr != nil && !isExpectedKillWait(waitErr) {
		if closeErr != nil && !errors.Is(closeErr, ErrResidualOwnedTree) {
			return fmt.Errorf("reap owned: wait: %w; close: %v", waitErr, closeErr)
		}
		return fmt.Errorf("reap owned: wait: %w", waitErr)
	}
	if closeErr != nil && !errors.Is(closeErr, ErrResidualOwnedTree) {
		return closeErr
	}
	return nil
}

// ReapOwnedCmd adopts without handshake (caller already started cmd).
func ReapOwnedCmd(cmd *exec.Cmd) error {
	owned, err := adoptOwnedCmd(cmd, nil)
	if err != nil {
		return err
	}
	return owned.Reap()
}

func closeOwnedAfterWait(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("close owned after wait: nil process")
	}
	if cmd.ProcessState == nil {
		return errors.New("close owned after wait: ProcessState nil")
	}
	// After Wait the leader is dead — only kill handles we open from the
	// leader token if still same (usually not). No numeric group adoption.
	tok, err := tokenOf(cmd.Process.Pid)
	if err != nil {
		return nil // leader gone
	}
	owned := &ownedSubprocess{
		cmd:     cmd,
		leader:  cmd.Process.Pid,
		pgid:    cmd.Process.Pid,
		handles: map[int]ownedHandle{},
		frozen:  true,
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	close(owned.stopped)
	if h, herr := openHandle(tok); herr == nil {
		owned.handles[tok.pid] = h
	}
	return owned.Close()
}

func waitHandleGone(h ownedHandle, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		if !h.tok.stillSame() || processIsZombie(h.tok.pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pid %d token still live after ownership bound", h.tok.pid)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitTokenGone(tok procToken, bound time.Duration) error {
	return waitHandleGone(ownedHandle{tok: tok, fd: -1}, bound)
}

func waitPIDGone(pid int, bound time.Duration) error {
	tok, err := tokenOf(pid)
	if err != nil {
		if kerr := killPID(pid, 0); kerr != nil && isESRCH(kerr) {
			return nil
		}
		return fmt.Errorf("waitPIDGone: tokenOf %d: %w", pid, err)
	}
	return waitTokenGone(tok, bound)
}

func processGroupLive(pgid int) bool {
	snap, err := snapshotProcesses()
	if err != nil {
		return false
	}
	return len(snap.membersOfGroup(pgid)) > 0
}

func waitProcessGroupEmpty(pgid int) error {
	deadline := time.Now().Add(processGroupGoneBound)
	for {
		if !processGroupLive(pgid) {
			return nil
		}
		if err := processGroupKiller(pgid); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process group %d still has live members", pgid)
		}
		time.Sleep(time.Millisecond)
	}
}

func listChildPids(ppid int) ([]int, error) {
	snap, err := snapshotProcesses()
	if err != nil {
		return nil, err
	}
	return snap.childrenOf(ppid), nil
}

func listPidsInGroup(pgid int) ([]int, error) {
	snap, err := snapshotProcesses()
	if err != nil {
		return nil, err
	}
	toks := snap.membersOfGroup(pgid)
	out := make([]int, 0, len(toks))
	for _, t := range toks {
		out = append(out, t.pid)
	}
	return out, nil
}

func reapProcessGroup(pgid int) error {
	return processGroupKiller(pgid)
}

func isESRCH(err error) bool {
	return err != nil && (errors.Is(err, syscall.ESRCH) || err == syscall.ESRCH)
}

func isExpectedKillWait(err error) bool {
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	status, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return false
	}
	sig := status.Signal()
	return sig == syscall.SIGKILL || sig == syscall.SIGTERM
}

func waitStatusSignaled(state *os.ProcessState) (syscall.Signal, bool) {
	if state == nil {
		return 0, false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	return status.Signal(), true
}

// hermeticGitConfig is prepended to every git argv.
var hermeticGitConfig = []string{
	"-c", "gc.auto=0",
	"-c", "gc.autoDetach=false",
	"-c", "maintenance.auto=false",
	"-c", "core.fsmonitor=",
	"-c", "core.useBuiltinFSMonitor=false",
	"-c", "core.hooksPath=/dev/null",
}

func hermeticGitEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+4)
	for _, entry := range env {
		switch {
		case strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL="),
			strings.HasPrefix(entry, "GIT_CONFIG_SYSTEM="),
			strings.HasPrefix(entry, "GIT_CONFIG_NOSYSTEM="),
			strings.HasPrefix(entry, "GIT_CONFIG_COUNT="),
			strings.HasPrefix(entry, "GIT_CONFIG_PARAMETERS="),
			strings.HasPrefix(entry, "GIT_TRACE"):
			continue
		default:
			out = append(out, entry)
		}
	}
	out = append(out,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	return out
}

func hermeticGitCommand(dir string, args ...string) *exec.Cmd {
	full := make([]string, 0, len(hermeticGitConfig)+len(args))
	full = append(full, hermeticGitConfig...)
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func runGit(dir string, args ...string) ([]byte, error) {
	cmd := hermeticGitCommand(dir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}
