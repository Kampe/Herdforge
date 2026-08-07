package verifier

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// processSnapshot is one native enumeration of the system process table.
type processSnapshot struct {
	byPID map[int]procToken
	ppid  map[int]int // pid -> parent pid
	pgid  map[int]int // pid -> process group id
}

func (s processSnapshot) token(pid int) (procToken, bool) {
	t, ok := s.byPID[pid]
	return t, ok
}

func (s processSnapshot) childrenOf(ppid int) []int {
	var out []int
	for pid, p := range s.ppid {
		if p == ppid {
			out = append(out, pid)
		}
	}
	return out
}

func (s processSnapshot) membersOfGroup(pgid int) []procToken {
	var out []procToken
	for pid, g := range s.pgid {
		if g == pgid {
			if t, ok := s.byPID[pid]; ok {
				out = append(out, t)
			}
		}
	}
	return out
}

// procToken is a causally observed process identity: PID + kernel start time.
// An incarnation is never replaced once recorded. PID reuse yields a different
// start time and is refused for signaling.
type procToken struct {
	pid       int
	startSec  int64
	startUsec int64
}

// Production uses the platform implementations. Tests replace these seams
// to prove identity and signal decisions without touching host processes.
var (
	tokenOfFn         = tokenOf
	processIsZombieFn = processIsZombie
	pidfdOpenFn       = pidfdOpen
	pidfdSendSignalFn = pidfdSendSignal
	processSnapshotFn = snapshotProcesses
)

func (t procToken) valid() bool { return t.pid > 1 }

func (t procToken) equal(o procToken) bool {
	return t.pid == o.pid && t.startSec == o.startSec && t.startUsec == o.startUsec
}

// stillSame reports whether pid still names this incarnation (including zombies).
func (t procToken) stillSame() bool {
	if !t.valid() {
		return false
	}
	cur, err := tokenOfFn(t.pid)
	if err != nil {
		return false
	}
	return t.equal(cur)
}

// isLiveTarget is a running (non-zombie) process matching this incarnation.
func (t procToken) isLiveTarget() bool {
	return t.stillSame() && !processIsZombieFn(t.pid)
}

// ownedHandle is a causally discovered process we may signal.
// On Linux, fd is a pidfd opened at discovery (kernel-bound identity).
// On Darwin, fd is -1 and signaling uses pre-Wait drain + stillSame checks.
type ownedHandle struct {
	tok procToken
	fd  int // pidfd or -1
}

func (h *ownedHandle) close() {
	if h != nil && h.fd >= 0 {
		_ = os.NewFile(uintptr(h.fd), "pidfd").Close()
		h.fd = -1
	}
}

// openHandle records a process at discovery time. Linux opens a pidfd so later
// signals cannot target a reused PID. Darwin stores the token only.
func openHandle(tok procToken) (ownedHandle, error) {
	if !tok.valid() {
		return ownedHandle{}, fmt.Errorf("openHandle: invalid token")
	}
	fd, err := pidfdOpenFn(tok.pid)
	if err != nil {
		// Fall back to token-only if pidfd unavailable (old kernel); still
		// better than bare kill, and Darwin always takes this path.
		if err == errPidfdUnsupported {
			return ownedHandle{tok: tok, fd: -1}, nil
		}
		// Process may have exited between discovery and open.
		if !tok.stillSame() {
			return ownedHandle{}, fmt.Errorf("openHandle: pid %d gone: %w", tok.pid, err)
		}
		// Some confined containers (cap-drop ALL) reject pidfd_open with EPERM
		// even for same-uid processes. Fall back to token-only kill so
		// identity-bound reaping still works without host-wide -pgid signals.
		if errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) {
			return ownedHandle{tok: tok, fd: -1}, nil
		}
		return ownedHandle{}, fmt.Errorf("openHandle pid %d: %w", tok.pid, err)
	}
	return ownedHandle{tok: tok, fd: fd}, nil
}

// kill signals the owned incarnation only.
// Linux: pidfd_send_signal (kernel-bound; no PID reuse).
// Darwin: no pidfd — re-check token immediately before each SIGKILL and never
// signal when the incarnation no longer matches. Residual TOCTOU remains on
// Darwin without a kernel handle; production mitigates by draining residuals
// while the supervisor leader is still live (pre-Wait), then freezing so
// post-Wait Close never adopts a new numeric pgid.
func (h ownedHandle) kill() (signaled bool, err error) {
	if !h.tok.valid() {
		return false, nil
	}
	if h.fd >= 0 {
		// A pidfd prevents PID reuse, but dead/zombie handles are not valid
		// signal targets. Match the token-only live/non-zombie gate first.
		if !h.tok.isLiveTarget() {
			return false, nil
		}
		if err := pidfdSendSignalFn(h.fd, syscallSIGKILL()); err != nil {
			if isESRCH(err) || isNotExistPidfd(err) {
				return false, nil
			}
			return false, fmt.Errorf("pidfd_send_signal pid %d: %w", h.tok.pid, err)
		}
		return true, nil
	}
	// Darwin / token-only path: tight check-then-kill; refuse if token drifts.
	signaledAny := false
	for attempt := 0; attempt < 2; attempt++ {
		if !h.tok.isLiveTarget() {
			return signaledAny, nil
		}
		if err := killPID(h.tok.pid, syscallSIGKILL()); err != nil {
			if isESRCH(err) {
				return signaledAny, nil
			}
			if !h.tok.stillSame() {
				// Incarnation gone or replaced between check and kill — do not
				// escalate; never signal a different start-time identity.
				return signaledAny, nil
			}
			return signaledAny, fmt.Errorf("SIGKILL pid %d: %w", h.tok.pid, err)
		}
		signaledAny = true
		// If the same incarnation is still live, retry once; if token no
		// longer matches, stop (PID may have been reused — do not re-signal).
		if !h.tok.stillSame() {
			return true, nil
		}
	}
	return signaledAny, nil
}
