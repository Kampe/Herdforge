package procsignal

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
)

// OwnedGroup is an opaque handle for a process group claimed from a live
// *os.Process that this package observed. CancelOwnedGroup is the only
// production cancel entrypoint for exec.Cmd process-group teardown.
type OwnedGroup struct {
	pgid  int
	token uint64
}

// ownedEntry is the registry row for a claimed group.
type ownedEntry struct {
	pgid     int
	ownerPID int
}

var (
	ownedMu    sync.Mutex
	ownedByTok = map[uint64]ownedEntry{}
	ownedToken atomic.Uint64

	// getPgid is a seam for tests; production uses syscall.Getpgid.
	getPgid = syscall.Getpgid
)

// ClaimSpawnedGroup registers a live *os.Process as an owned process-group
// leader. The process must still be alive, must be the leader of its group
// (pgid == pid — the Setpgid child convention), and must pass ValidatePGID.
// If the process has already exited, Claim returns a zero handle so
// CancelOwnedGroup is a no-op.
func ClaimSpawnedGroup(p *os.Process) (OwnedGroup, error) {
	if p == nil {
		return OwnedGroup{}, ErrNotOwned
	}
	pid := p.Pid
	// Liveness probe without killing. If the child is already gone, cancel
	// is a successful no-op (common race with short commands).
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return OwnedGroup{}, nil
	}
	pgid, err := getPgid(pid)
	if err != nil {
		// Process disappeared between Signal(0) and getpgid.
		return OwnedGroup{}, nil
	}
	// Refuse non-leaders: kill(-pid) when pid is not a group leader can hit an
	// unrelated group or the caller's session. Setpgid children always have
	// pgid == pid.
	if pgid != pid {
		return OwnedGroup{}, fmt.Errorf("%w: pid %d is not process-group leader (pgid=%d)", ErrNotOwned, pid, pgid)
	}
	if err := ValidatePGID(pgid); err != nil {
		return OwnedGroup{}, err
	}
	tok := ownedToken.Add(1)
	ownedMu.Lock()
	ownedByTok[tok] = ownedEntry{pgid: pgid, ownerPID: callerPID()}
	ownedMu.Unlock()
	return OwnedGroup{pgid: pgid, token: tok}, nil
}

// CancelOwnedGroup SIGKILLs a previously claimed process group via the
// unexported host backend. The registry entry is single-use and removed
// before the signal so concurrent cancels cannot double-fire.
func CancelOwnedGroup(g OwnedGroup) error {
	if g.token == 0 && g.pgid == 0 {
		return nil // zero handle from already-exited claim
	}
	ownedMu.Lock()
	ent, ok := ownedByTok[g.token]
	if ok {
		delete(ownedByTok, g.token)
	}
	ownedMu.Unlock()
	if !ok || ent.pgid != g.pgid {
		return ErrNotOwned
	}
	// Re-validate at cancel time (caller pgid may have changed; pgid 1 still refused).
	if err := ValidatePGID(ent.pgid); err != nil {
		return err
	}
	return hostBackend{destructive: false}.Kill(-ent.pgid, syscall.SIGKILL)
}

// CancelSpawnedProcess claims then cancels a live *os.Process. This is the
// production helper for exec.Cmd.Cancel after SysProcAttr{Setpgid: true}.
func CancelSpawnedProcess(p *os.Process) error {
	if p == nil {
		return nil
	}
	g, err := ClaimSpawnedGroup(p)
	if err != nil {
		return err
	}
	return CancelOwnedGroup(g)
}

// SignalExactProcess delivers sig to a single PID after ValidatePID, using the
// owned (non-destructive) host backend. Callers (e.g. toolchild) must already
// have proven broker ownership / start-token identity before invoking this.
func SignalExactProcess(pid int, sig syscall.Signal) error {
	if err := ValidatePID(pid); err != nil {
		return err
	}
	return hostBackend{destructive: false}.Kill(pid, sig)
}
