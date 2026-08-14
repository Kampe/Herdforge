package verifier

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestWaitHandleGoneUsesPidfdOverReusedPIDToken(t *testing.T) {
	previousToken, previousZombie, previousExited := tokenOfFn, processIsZombieFn, pidfdExitedFn
	t.Cleanup(func() {
		tokenOfFn, processIsZombieFn, pidfdExitedFn = previousToken, previousZombie, previousExited
	})

	// Simulate a new, live process that reused the numeric PID in the same
	// observable /proc start-time tick. The old token-only wait reaches its
	// timeout and turns a successful verification into BLOCKED; the pidfd is
	// bound to the original process and reports that it has exited.
	tok := procToken{pid: 42, startSec: 7, startUsec: 0}
	tokenOfFn = func(int) (procToken, error) { return tok, nil }
	processIsZombieFn = func(int) bool { return false }
	pidfdExitedFn = func(fd int) (bool, error) {
		if fd != 17 {
			t.Fatalf("pidfd exit probe fd=%d, want 17", fd)
		}
		return true, nil
	}

	started := time.Now()
	if err := waitHandleGone(ownedHandle{tok: tok, fd: 17}, 50*time.Millisecond); err != nil {
		t.Fatalf("exited pidfd must close ownership: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 50*time.Millisecond {
		t.Fatalf("pidfd exit proof waited for stale token timeout: %s", elapsed)
	}
}

func TestPidfdExitedHandlesInvalidFD(t *testing.T) {
	if _, err := pidfdExited(-1); !errors.Is(err, errPidfdUnsupported) {
		t.Fatalf("negative fd got %v, want %v", err, errPidfdUnsupported)
	}
	if _, err := pidfdExited(1 << 31); !errors.Is(err, errPidfdUnsupported) {
		t.Fatalf("overflow fd got %v, want %v", err, errPidfdUnsupported)
	}
}

func TestOwnedHandlePidfdSignalsOnlyLiveExactToken(t *testing.T) {
	previousToken, previousZombie, previousSignal := tokenOfFn, processIsZombieFn, pidfdSendSignalFn
	t.Cleanup(func() {
		tokenOfFn, processIsZombieFn, pidfdSendSignalFn = previousToken, previousZombie, previousSignal
	})

	tok := procToken{pid: 42, startSec: 7, startUsec: 9}
	current := tok
	zombie := false
	tokenOfFn = func(pid int) (procToken, error) {
		if pid != tok.pid {
			return procToken{}, errors.New("missing token")
		}
		return current, nil
	}
	processIsZombieFn = func(int) bool { return zombie }
	signals := 0
	pidfdSendSignalFn = func(fd int, sig syscall.Signal) error {
		if fd != 17 || sig != syscall.SIGKILL {
			t.Fatalf("unexpected signal backend call fd=%d sig=%v", fd, sig)
		}
		signals++
		return nil
	}

	h := ownedHandle{tok: tok, fd: 17}
	current = procToken{}
	if signaled, err := h.kill(); err != nil || signaled {
		t.Fatalf("dead exact handle: signaled=%v err=%v", signaled, err)
	}
	if signals != 0 {
		t.Fatalf("dead exact handle attempted %d pidfd signals", signals)
	}

	current = tok
	zombie = true
	if signaled, err := h.kill(); err != nil || signaled {
		t.Fatalf("zombie exact handle: signaled=%v err=%v", signaled, err)
	}
	if signals != 0 {
		t.Fatalf("zombie exact handle attempted %d pidfd signals", signals)
	}

	zombie = false
	if signaled, err := h.kill(); err != nil || !signaled {
		t.Fatalf("live exact handle: signaled=%v err=%v", signaled, err)
	}
	if signals != 1 {
		t.Fatalf("live exact handle made %d pidfd signals, want 1", signals)
	}

	current = procToken{pid: tok.pid, startSec: tok.startSec + 1, startUsec: tok.startUsec}
	if signaled, err := h.kill(); err != nil || signaled {
		t.Fatalf("PID-reused handle: signaled=%v err=%v", signaled, err)
	}
	if signals != 1 {
		t.Fatalf("PID-reused handle attempted an extra signal: %d", signals)
	}
}

func TestOwnedHandlePidfdPropagatesPermissionError(t *testing.T) {
	previousToken, previousZombie, previousSignal := tokenOfFn, processIsZombieFn, pidfdSendSignalFn
	t.Cleanup(func() {
		tokenOfFn, processIsZombieFn, pidfdSendSignalFn = previousToken, previousZombie, previousSignal
	})
	tok := procToken{pid: 42, startSec: 7, startUsec: 9}
	tokenOfFn = func(int) (procToken, error) { return tok, nil }
	processIsZombieFn = func(int) bool { return false }
	pidfdSendSignalFn = func(int, syscall.Signal) error { return syscall.EPERM }

	signaled, err := (ownedHandle{tok: tok, fd: 17}).kill()
	if signaled || !errors.Is(err, syscall.EPERM) {
		t.Fatalf("permission error must fail closed: signaled=%v err=%v", signaled, err)
	}
}

func TestOwnedSamplingDoesNotAdoptAmbientOrAmbiguousPIDs(t *testing.T) {
	previousToken, previousZombie, previousOpen, previousSignal, previousSnapshot := tokenOfFn, processIsZombieFn, pidfdOpenFn, pidfdSendSignalFn, processSnapshotFn
	t.Cleanup(func() {
		tokenOfFn, processIsZombieFn, pidfdOpenFn, pidfdSendSignalFn, processSnapshotFn = previousToken, previousZombie, previousOpen, previousSignal, previousSnapshot
	})

	leader := procToken{pid: 100, startSec: 1, startUsec: 1}
	child := procToken{pid: 200, startSec: 2, startUsec: 2}
	ambientLow := procToken{pid: 2, startSec: 3, startUsec: 3}
	ambientHigh := procToken{pid: 9000, startSec: 4, startUsec: 4}
	tokens := map[int]procToken{100: leader, 200: child, 2: ambientLow, 9000: ambientHigh}
	tokenOfFn = func(pid int) (procToken, error) {
		tok, ok := tokens[pid]
		if !ok {
			return procToken{}, errors.New("missing token")
		}
		return tok, nil
	}
	processIsZombieFn = func(int) bool { return false }
	pidfdOpenFn = func(pid int) (int, error) { return pid + 1000, nil }
	var signaled []int
	pidfdSendSignalFn = func(fd int, _ syscall.Signal) error {
		signaled = append(signaled, fd)
		return nil
	}

	o := &ownedSubprocess{
		leader: 100,
		pgid:   100,
		handles: map[int]ownedHandle{
			100: {tok: leader, fd: 1100},
		},
	}
	processSnapshotFn = func() (processSnapshot, error) {
		return processSnapshot{
			byPID: map[int]procToken{100: leader, 200: child, 2: ambientLow, 9000: ambientHigh},
			ppid:  map[int]int{200: 100, 2: 999, 9000: 998},
			pgid:  map[int]int{100: 100, 2: 2, 9000: 9000},
		}, nil
	}

	if err := o.sample(); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if err := o.killTracked(false); err != nil {
		t.Fatalf("killTracked: %v", err)
	}
	if len(signaled) != 1 || signaled[0] != 1200 {
		t.Fatalf("signal backend received %v, want only exact child fd 1200", signaled)
	}
	if _, ok := o.handles[2]; ok {
		t.Fatal("ambient low PID was adopted")
	}
	if _, ok := o.handles[9000]; ok {
		t.Fatal("ambient high PID was adopted")
	}
}

func TestOwnedSamplingRejectsMalformedAndMissingIdentity(t *testing.T) {
	previousToken, previousZombie, previousOpen, previousSignal, previousSnapshot := tokenOfFn, processIsZombieFn, pidfdOpenFn, pidfdSendSignalFn, processSnapshotFn
	t.Cleanup(func() {
		tokenOfFn, processIsZombieFn, pidfdOpenFn, pidfdSendSignalFn, processSnapshotFn = previousToken, previousZombie, previousOpen, previousSignal, previousSnapshot
	})
	leader := procToken{pid: 100, startSec: 1, startUsec: 1}
	tokenOfFn = func(pid int) (procToken, error) {
		if pid == 100 {
			return leader, nil
		}
		return procToken{}, errors.New("missing or malformed identity")
	}
	processIsZombieFn = func(int) bool { return false }
	pidfdOpenFn = func(int) (int, error) { return 1100, nil }
	pidfdSendSignalFn = func(int, syscall.Signal) error {
		t.Fatal("malformed or missing identity reached signal backend")
		return nil
	}
	o := &ownedSubprocess{leader: 100, pgid: 100, handles: map[int]ownedHandle{100: {tok: leader, fd: 1100}}}
	processSnapshotFn = func() (processSnapshot, error) {
		return processSnapshot{
			byPID: map[int]procToken{100: leader, 200: {pid: 200, startSec: 2}},
			ppid:  map[int]int{200: 100},
			pgid:  map[int]int{100: 100},
		}, nil
	}
	if err := o.sample(); err != nil {
		t.Fatalf("missing child identity sampling: %v", err)
	}
	if err := o.killTracked(false); err != nil {
		t.Fatalf("missing child identity kill: %v", err)
	}
}
