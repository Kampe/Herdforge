package verifier

import (
	"fmt"
	"os"
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

func (t procToken) valid() bool { return t.pid > 1 }

func (t procToken) equal(o procToken) bool {
	return t.pid == o.pid && t.startSec == o.startSec && t.startUsec == o.startUsec
}

// stillSame reports whether pid still names this incarnation (including zombies).
func (t procToken) stillSame() bool {
	if !t.valid() {
		return false
	}
	cur, err := tokenOf(t.pid)
	if err != nil {
		return false
	}
	return t.equal(cur)
}

// isLiveTarget is a running (non-zombie) process matching this incarnation.
func (t procToken) isLiveTarget() bool {
	return t.stillSame() && !processIsZombie(t.pid)
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
	fd, err := pidfdOpen(tok.pid)
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
		return ownedHandle{}, fmt.Errorf("openHandle pid %d: %w", tok.pid, err)
	}
	return ownedHandle{tok: tok, fd: fd}, nil
}

// kill signals the owned incarnation only.
// Linux: pidfd_send_signal (no PID reuse).
// Darwin: SIGKILL only if stillSame (no kernel handle; prefer pre-Wait drain).
func (h ownedHandle) kill() (signaled bool, err error) {
	if !h.tok.valid() {
		return false, nil
	}
	if h.fd >= 0 {
		if err := pidfdSendSignal(h.fd, syscallSIGKILL()); err != nil {
			if isESRCH(err) || isNotExistPidfd(err) {
				return false, nil
			}
			return false, fmt.Errorf("pidfd_send_signal pid %d: %w", h.tok.pid, err)
		}
		return true, nil
	}
	// Darwin / token-only path.
	if !h.tok.isLiveTarget() {
		return false, nil
	}
	if err := killPID(h.tok.pid, syscallSIGKILL()); err != nil {
		if isESRCH(err) {
			return false, nil
		}
		if !h.tok.stillSame() {
			return false, nil
		}
		return false, fmt.Errorf("SIGKILL pid %d: %w", h.tok.pid, err)
	}
	return true, nil
}
