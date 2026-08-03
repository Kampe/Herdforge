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

// processGroupKiller identity-kills current members of a process group.
// Used only while the original supervisor leader is still live.
// Never signals -pgid. Tests may replace it for mutation proofs.
var processGroupKiller = killProcessGroupMembers

// finalizeOwnedTree is the production ownership close after residual drain.
var finalizeOwnedTree = (*ownedSubprocess).Close

// residualDrainFn drains residual writers while the supervisor is still live.
// Tests may replace it to mutation-prove that omitting the done-phase drain
// leaves residual writers alive (PASS-with-writer regression).
var residualDrainFn = (*ownedSubprocess).drainResidualsWhileLeaderLive

const (
	processGroupGoneBound = 500 * time.Millisecond
	trackSampleInterval   = 5 * time.Millisecond
	// handshakeReadBound bounds the start line (pre-exec barrier).
	handshakeReadBound = 5 * time.Second
	// handshakeDoneBound bounds the done line after cont. Long-running
	// verification commands use Cancel to fail closed earlier via pipe EOF;
	// this is a hard hang guard, not a success-path sleep.
	handshakeDoneBound = 30 * time.Minute
)

// ErrResidualOwnedTree means live owned writers existed at close.
var ErrResidualOwnedTree = errors.New("residual owned process tree members after leader wait")

// ErrResidualProcessGroup aliases residual for older tests.
var ErrResidualProcessGroup = ErrResidualOwnedTree

func killProcessGroupMembers(pgid int) error {
	return killProcessGroupMembersExcept(pgid, -1)
}

// killProcessGroupMembersExcept identity-kills live members of pgid except
// exceptPID (the supervisor leader must not kill itself mid-drain).
func killProcessGroupMembersExcept(pgid, exceptPID int) error {
	if pgid <= 0 {
		return fmt.Errorf("kill process group members: invalid pgid %d", pgid)
	}
	snap, err := snapshotProcesses()
	if err != nil {
		return fmt.Errorf("kill process group members: snapshot: %w", err)
	}
	var errs []error
	for _, tok := range snap.membersOfGroup(pgid) {
		if exceptPID > 0 && tok.pid == exceptPID {
			continue
		}
		h, herr := openHandle(tok)
		if herr != nil {
			// Process exited between snapshot and open — not an error.
			if !tok.stillSame() {
				continue
			}
			errs = append(errs, fmt.Errorf("open handle pid %d: %w", tok.pid, herr))
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

func killProcessGroupIfLive(pgid int) error {
	return processGroupKiller(pgid)
}

// ownedSubprocess is causal process-tree ownership for one verification run.
//
// Protocol (two-phase handshake with the ownership wrapper):
//  1. Wrapper starts user command, writes "start <pid>" on FD3, waits for user.
//  2. On user exit, wrapper writes "done <ec>" on FD3 and BLOCKS reading FD4.
//  3. Parent, while wrapper is still alive (process group still owned), samples
//     the causal tree + live original pgid members, kills residuals that still
//     hold the inherited ownership marker FD (setsid/double-fork lineage), freezes.
//  4. Parent writes "go" on FD4; wrapper exits; parent Wait returns.
//
// Discovery never replaces a recorded incarnation. After freeze, no new PIDs
// are adopted from numeric pgid membership. Escaped-descendant kill authority
// is the unforgeable inherited marker FD (ExtraFiles FD5), not path contact or
// start-time ordering. Path contact may only corroborate.
type ownedSubprocess struct {
	cmd          *exec.Cmd
	leader       int
	pgid         int
	candidateDir string // verification root (corroboration / diagnostics only)
	markerPath   string // private ownership marker path (lineage authority)
	markerFile   *os.File

	mu       sync.Mutex
	handles  map[int]ownedHandle
	frozen   bool
	trackErr error
	hadLive  bool

	statusR *os.File // FD3 from child (start/done lines)
	ackW    *os.File // FD4 to child (go)

	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
}

// ownershipWrapperScript: pre-exec cont barrier + two-phase residual drain.
//
// FD3 → parent (start/done). FD4 ← parent (cont, then go).
// FD5 = inherited ownership marker (must stay open across user exec).
//
// The user command is launched under a subshell that blocks on FD4 before
// exec. Control pipes FD3/FD4 are closed into the user tree so writers cannot
// steal "go"; FD5 remains open as the unforgeable lineage marker. After the
// user exits, the supervisor writes "done" and blocks on FD4 so residual
// drain runs while the original process group is still owned by a live
// supervisor.
const ownershipWrapperScript = `
user_path="$1"
shift
(
  # Block before exec until parent has recorded causal handles (pre-fork barrier).
  IFS= read -r _cont <&4 || exit 1
  # Close control FDs so user writers cannot steal protocol lines.
  # Keep FD5 open — inherited ownership marker for escaped-descendant lineage.
  exec 3>&- 4>&-
  exec "$user_path" "$@"
) &
child=$!
printf 'start %s\n' "$child" >&3
wait "$child"
ec=$?
printf 'done %s\n' "$ec" >&3
# Hold the process group open until parent finishes residual drain.
IFS= read -r _ack <&4 || true
exit "$ec"
`

// prepareOwnedCommand builds a Setpgid supervisor with status+ack pipes and an
// inherited ownership marker FD. On Linux, also applies PID/user namespace
// containment when the kernel allows it.
func prepareOwnedCommand(ctx context.Context, path string, args []string, dir string, env []string) (cmd *exec.Cmd, statusR, statusW, ackR, ackW, marker *os.File, markerPath string, err error) {
	statusR, statusW, err = os.Pipe()
	if err != nil {
		return nil, nil, nil, nil, nil, nil, "", fmt.Errorf("status pipe: %w", err)
	}
	ackR, ackW, err = os.Pipe()
	if err != nil {
		_ = statusR.Close()
		_ = statusW.Close()
		return nil, nil, nil, nil, nil, nil, "", fmt.Errorf("ack pipe: %w", err)
	}
	marker, markerPath, err = createOwnershipMarker()
	if err != nil {
		_ = statusR.Close()
		_ = statusW.Close()
		_ = ackR.Close()
		_ = ackW.Close()
		return nil, nil, nil, nil, nil, nil, "", fmt.Errorf("ownership marker: %w", err)
	}
	wrapArgs := append([]string{"-c", ownershipWrapperScript, "owned-wrap", path}, args...)
	cmd = exec.CommandContext(ctx, "sh", wrapArgs...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	attr := &syscall.SysProcAttr{Setpgid: true}
	applyOwnershipContainment(attr)
	cmd.SysProcAttr = attr
	// ExtraFiles: child FD3=statusW, FD4=ackR, FD5=marker
	cmd.ExtraFiles = []*os.File{statusW, ackR, marker}
	return cmd, statusR, statusW, ackR, ackW, marker, markerPath, nil
}

// adoptOwnedCmd records the leader and prepares for the two-phase protocol.
// handshake pipes: statusR reads start/done; ackW writes go.
// marker/markerPath are the inherited lineage marker (kill authority for
// escaped descendants). candidateDir is corroboration-only.
func adoptOwnedCmd(cmd *exec.Cmd, statusR, ackW *os.File, candidateDir, markerPath string, marker *os.File) (*ownedSubprocess, error) {
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
		cmd:          cmd,
		leader:       leader,
		pgid:         leader,
		candidateDir: candidateDir,
		markerPath:   markerPath,
		markerFile:   marker,
		handles:      map[int]ownedHandle{leader: h},
		statusR:      statusR,
		ackW:         ackW,
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
	}
	go o.trackLoop()
	return o, nil
}

// RunProtocol drives start→cont→done→drain→go. Fail-closed on protocol,
// sample, and drain errors (never silent continue).
func (o *ownedSubprocess) RunProtocol() (userExitHint int, err error) {
	if o == nil {
		return -1, errors.New("run protocol: nil")
	}
	userExitHint = -1
	if o.statusR == nil {
		// No handshake (ReapOwnedCmd path): sample once and return.
		if serr := o.sample(); serr != nil {
			return -1, serr
		}
		return -1, nil
	}
	// Always release ack pipe lines on return so Wait cannot hang forever.
	// contSent tracks whether the pre-exec barrier was released.
	// Named err is updated in defer so release/close failures fail closed.
	contSent := false
	defer func() {
		var releaseErr error
		if !contSent {
			// Unblock pre-exec reader (if still waiting) then post-done reader.
			if cerr := o.writeAckLine("cont"); cerr != nil && !isBenignPipeErr(cerr) {
				releaseErr = fmt.Errorf("ownership cont (deferred): %w", cerr)
			}
		}
		if rerr := o.releaseSupervisor(); rerr != nil && releaseErr == nil {
			releaseErr = rerr
		}
		if o.statusR != nil {
			if cerr := o.statusR.Close(); cerr != nil && releaseErr == nil && !isBenignPipeErr(cerr) {
				releaseErr = fmt.Errorf("ownership status close: %w", cerr)
			}
			o.statusR = nil
		}
		if releaseErr != nil && err == nil {
			err = releaseErr
		}
	}()

	// Phase 1: start <pid> — child is blocked on FD4 before user exec.
	if err := o.statusR.SetReadDeadline(time.Now().Add(handshakeReadBound)); err != nil {
		return -1, fmt.Errorf("ownership start deadline: %w", err)
	}
	br := bufio.NewReader(o.statusR)
	line, err := br.ReadString('\n')
	if err != nil {
		return -1, fmt.Errorf("ownership start handshake: %w", err)
	}
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "start" {
		return -1, fmt.Errorf("ownership start handshake bad line %q", strings.TrimSpace(line))
	}
	childPid, err := strconv.Atoi(fields[1])
	if err != nil || childPid <= 1 {
		return -1, fmt.Errorf("ownership start handshake bad pid %q", fields[1])
	}
	tok, err := tokenOf(childPid)
	if err != nil {
		_ = killProcessGroupIfLive(o.leader)
		return -1, fmt.Errorf("ownership start handshake token: %w", err)
	}
	if err := o.noteCausal(tok); err != nil {
		_ = killProcessGroupIfLive(o.leader)
		return -1, fmt.Errorf("ownership start handshake note: %w", err)
	}
	// Sample while child is still blocked pre-exec so discovery cannot race fork.
	if serr := o.sample(); serr != nil {
		_ = killProcessGroupIfLive(o.leader)
		return -1, fmt.Errorf("ownership sample after start (pre-cont): %w", serr)
	}
	// Release pre-exec barrier only after causal handles are open.
	if err := o.writeAckLine("cont"); err != nil {
		_ = killProcessGroupIfLive(o.leader)
		return -1, fmt.Errorf("ownership cont: %w", err)
	}
	contSent = true

	// Phase 2: done <ec> — bounded read; Cancel kills supervisor → EOF sooner.
	if err := o.statusR.SetReadDeadline(time.Now().Add(handshakeDoneBound)); err != nil {
		return -1, fmt.Errorf("ownership done deadline: %w", err)
	}
	line, err = br.ReadString('\n')
	if err != nil {
		// EOF/cancel/supervisor death/deadline before done: fail closed.
		return -1, fmt.Errorf("ownership done handshake: %w", err)
	}
	fields = strings.Fields(line)
	if len(fields) != 2 || fields[0] != "done" {
		return -1, fmt.Errorf("ownership done handshake bad line %q", strings.TrimSpace(line))
	}
	if ec, eerr := strconv.Atoi(fields[1]); eerr == nil {
		userExitHint = ec
	}

	// Residual drain WHILE supervisor (leader) is still live.
	if serr := o.sample(); serr != nil {
		return userExitHint, fmt.Errorf("ownership sample at done: %w", serr)
	}
	if derr := residualDrainFn(o); derr != nil {
		return userExitHint, derr
	}
	o.freeze()
	// releaseSupervisor (go) runs via defer and may set named err.
	return userExitHint, nil
}

// writeAckLine writes one line on the parent→child ack pipe without closing it.
func (o *ownedSubprocess) writeAckLine(line string) error {
	if o == nil || o.ackW == nil {
		return errors.New("ack pipe closed")
	}
	_, err := io.WriteString(o.ackW, line+"\n")
	return err
}

// releaseSupervisor unblocks the ownership wrapper's post-done FD4 read.
// Returns write/close errors (fail-closed); still nils ackW after close attempt.
// EPIPE/closed-pipe is not an error: Cancel/SIGKILL may already have reaped
// the supervisor, so the ack reader is gone by design.
func (o *ownedSubprocess) releaseSupervisor() error {
	if o == nil || o.ackW == nil {
		return nil
	}
	_, werr := io.WriteString(o.ackW, "go\n")
	cerr := o.ackW.Close()
	o.ackW = nil
	if werr != nil && !isBenignPipeErr(werr) {
		return fmt.Errorf("ownership go write: %w", werr)
	}
	if cerr != nil && !isBenignPipeErr(cerr) {
		return fmt.Errorf("ownership go close: %w", cerr)
	}
	return nil
}

// isBenignPipeErr reports pipe failures expected after the peer has exited
// (Cancel/SIGKILL/Wait race). Not ownership failures.
func isBenignPipeErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE) || errors.Is(err, os.ErrClosed) {
		return true
	}
	// os.File.Write wraps as *fs.PathError / *os.PathError with Err=EPIPE.
	var pe *os.PathError
	if errors.As(err, &pe) && pe != nil && (errors.Is(pe.Err, syscall.EPIPE) || errors.Is(pe.Err, os.ErrClosed)) {
		return true
	}
	return false
}

// drainResidualsWhileLeaderLive kills non-leader owned handles and live original
// pgid members while the supervisor incarnation is still alive, then adopts
// marker-lineage residuals (setsid/double-fork escapes that still hold the
// inherited ownership marker FD). Path contact is never kill authority.
func (o *ownedSubprocess) drainResidualsWhileLeaderLive() error {
	o.mu.Lock()
	leaderH, ok := o.handles[o.leader]
	pgid := o.pgid
	o.mu.Unlock()
	if !ok || !leaderH.tok.stillSame() {
		return fmt.Errorf("drain residuals: supervisor leader not live")
	}
	// Final sample: BFS + live original pgid members (leader still owns group).
	if err := o.sample(); err != nil {
		return fmt.Errorf("drain residuals sample: %w", err)
	}
	if err := o.killTracked(false); err != nil {
		return fmt.Errorf("drain residuals kill tracked: %w", err)
	}
	// Identity-kill remaining live members of the original pgid while leader
	// live — never kill the supervisor itself.
	if err := killProcessGroupMembersExcept(pgid, o.leader); err != nil {
		return fmt.Errorf("drain residuals group members: %w", err)
	}
	// One more causal sample+kill pass for late forks still in the owned tree.
	if err := o.sample(); err != nil {
		return fmt.Errorf("drain residuals re-sample: %w", err)
	}
	if err := o.killTracked(false); err != nil {
		return fmt.Errorf("drain residuals re-kill: %w", err)
	}
	// Marker lineage residual: processes that still hold the inherited
	// ownership marker FD. Authority is the marker, not path/start-time.
	if err := o.adoptAndKillMarkedResiduals(); err != nil {
		return err
	}
	return nil
}

// adoptAndKillMarkedResiduals discovers processes that still hold the
// inherited ownership marker open and identity-kills them via start-time
// tokens. Self, ancestors, and the ownership supervisor leader are excluded.
// Unrelated processes that merely open a descendant under the candidate —
// without the marker — are never adopted (worktree isolation).
func (o *ownedSubprocess) adoptAndKillMarkedResiduals() error {
	if o == nil || o.markerPath == "" {
		return nil
	}
	toks, err := processesHoldingMarker(o.markerPath)
	if err != nil {
		return fmt.Errorf("drain residuals marker lineage: %w", err)
	}
	toks = filterResidualTokens(toks, o.leader)
	for _, tok := range toks {
		if tok.pid <= 1 || tok.pid == o.leader || tok.pid == os.Getpid() {
			continue
		}
		if err := o.noteCausal(tok); err != nil {
			if !tok.stillSame() {
				continue
			}
			return fmt.Errorf("drain residuals note marker lineage pid %d: %w", tok.pid, err)
		}
	}
	if err := o.killTracked(false); err != nil {
		return fmt.Errorf("drain residuals kill marker lineage: %w", err)
	}
	return nil
}

func (o *ownedSubprocess) noteCausal(tok procToken) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.frozen {
		return nil
	}
	if _, exists := o.handles[tok.pid]; exists {
		return nil // never replace
	}
	h, err := openHandle(tok)
	if err != nil {
		// Gone between discovery and open is not ownership failure.
		if !tok.stillSame() {
			return nil
		}
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

// sample: while leader live, adopt (1) BFS children of owned live PIDs and
// (2) current members of the original pgid (causal: leader still owns group).
// Never replace tokens. Freeze when leader dies. Sample errors return to caller.
func (o *ownedSubprocess) sample() error {
	o.mu.Lock()
	if o.frozen {
		o.mu.Unlock()
		return nil
	}
	leaderH, ok := o.handles[o.leader]
	pgid := o.pgid
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
	// While supervisor owns the process group, its members are causal.
	for _, tok := range snap.membersOfGroup(pgid) {
		if _, exists := o.handles[tok.pid]; exists {
			continue
		}
		h, herr := openHandle(tok)
		if herr != nil {
			if !tok.stillSame() {
				continue
			}
			return fmt.Errorf("sample open group member pid %d: %w", tok.pid, herr)
		}
		o.handles[tok.pid] = h
	}
	// BFS children of owned live processes (setsid path before reparent).
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
				continue
			}
			tok, ok := snap.token(c)
			if !ok {
				continue
			}
			h, herr := openHandle(tok)
			if herr != nil {
				if !tok.stillSame() {
					continue
				}
				return fmt.Errorf("sample open child pid %d: %w", tok.pid, herr)
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
	o.stopOnce.Do(func() { close(o.stop) })
	select {
	case <-o.stopped:
		return nil
	case <-time.After(processGroupGoneBound):
		return fmt.Errorf("owned tree tracker stop deadline exceeded (%s)", processGroupGoneBound)
	}
}

// KillGroupLive is Cancel-path drain while leader may be live.
func (o *ownedSubprocess) KillGroupLive() error {
	if o == nil {
		return errors.New("kill group live: nil")
	}
	if err := o.sample(); err != nil {
		return err
	}
	var errs []error
	if err := o.killTracked(false); err != nil {
		errs = append(errs, err)
	}
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

func (o *ownedSubprocess) Pgid() int {
	if o == nil {
		return 0
	}
	return o.pgid
}

// Close freezes, kills only frozen causal handles, closes pidfds.
// No numeric group re-adoption after freeze.
func (o *ownedSubprocess) Close() error {
	if o == nil {
		return errors.New("close owned: nil")
	}
	o.freeze()
	stopErr := o.stopTracker()
	// Kill remaining tracked (leader included if still live).
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
	if o.ackW != nil {
		_ = o.ackW.Close()
		o.ackW = nil
	}
	if o.markerFile != nil {
		_ = o.markerFile.Close()
		o.markerFile = nil
	}
	if o.markerPath != "" {
		_ = os.Remove(o.markerPath)
		o.markerPath = ""
	}
	if len(killErrs) > 0 {
		return fmt.Errorf("close owned: %w", errors.Join(killErrs...))
	}
	if hadLive {
		return fmt.Errorf("%w: pgid=%d tracked=%d", ErrResidualOwnedTree, o.pgid, len(handles))
	}
	return nil
}

// Reap for non-protocol cmds (fixtures): kill all owned, Wait, Close.
func (o *ownedSubprocess) Reap() error {
	if o == nil || o.cmd == nil {
		return errors.New("reap owned: nil")
	}
	// If protocol pipes present, try protocol first.
	if o.statusR != nil {
		_, perr := o.RunProtocol()
		waitErr := o.cmd.Wait()
		closeErr := finalizeOwnedTree(o)
		if perr != nil {
			return fmt.Errorf("reap owned protocol: %w (wait=%v close=%v)", perr, waitErr, closeErr)
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
	if serr := o.sample(); serr != nil {
		// Still freeze/kill/Wait so the supervisor cannot hang, but fail closed.
		o.freeze()
		_ = o.killTracked(true)
		waitErr := o.cmd.Wait()
		closeErr := finalizeOwnedTree(o)
		return fmt.Errorf("reap owned sample: %w (wait=%v close=%v)", serr, waitErr, closeErr)
	}
	o.freeze()
	killErr := o.killTracked(true)
	o.mu.Lock()
	leaderH, ok := o.handles[o.leader]
	pgid := o.pgid
	o.mu.Unlock()
	if ok && leaderH.tok.stillSame() {
		if err := processGroupKiller(pgid); err != nil {
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

// ReapOwnedCmd adopts without pipes/marker (fixture path). Lineage residual
// drain requires the production marker FD from prepareOwnedCommand.
func ReapOwnedCmd(cmd *exec.Cmd) error {
	dir := ""
	if cmd != nil {
		dir = cmd.Dir
	}
	owned, err := adoptOwnedCmd(cmd, nil, nil, dir, "", nil)
	if err != nil {
		return err
	}
	return owned.Reap()
}

func closeOwnedAfterWait(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState == nil {
		return errors.New("close owned after wait: invalid cmd")
	}
	tok, err := tokenOf(cmd.Process.Pid)
	if err != nil {
		return nil
	}
	owned := &ownedSubprocess{
		cmd:          cmd,
		leader:       cmd.Process.Pid,
		pgid:         cmd.Process.Pid,
		candidateDir: cmd.Dir,
		handles:      map[int]ownedHandle{},
		frozen:       true,
		stop:         make(chan struct{}),
		stopped:      make(chan struct{}),
	}
	close(owned.stopped)
	h, herr := openHandle(tok)
	if herr != nil {
		if !tok.stillSame() {
			return owned.Close()
		}
		return fmt.Errorf("close owned after wait: open handle: %w", herr)
	}
	owned.handles[tok.pid] = h
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

// processGroupLive reports whether pgid still has members. Snapshot failure
// is treated as still-live (fail closed) so callers do not claim empty on error.
func processGroupLive(pgid int) bool {
	snap, err := snapshotProcesses()
	if err != nil {
		return true
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
