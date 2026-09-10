package worktree

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// provenDeadPID returns a pid that provably no longer exists.
func provenDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn transient holder: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if errors.Is(syscall.Kill(cmd.Process.Pid, 0), syscall.ESRCH) {
			return cmd.Process.Pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("transient holder pid %d never disappeared", cmd.Process.Pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeIdlePoolTickLock(t *testing.T, repoRoot string, identity idlePoolTickLockIdentity) {
	t.Helper()
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	writeIdlePoolTickLockRaw(t, repoRoot, string(data))
}

func writeIdlePoolTickLockRaw(t *testing.T, repoRoot, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idlePoolLockPath(repoRoot), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustHostname(t *testing.T) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("hostname unavailable: %v", err)
	}
	if strings.TrimSpace(host) == "" {
		t.Skip("hostname empty")
	}
	return host
}

// The tick lock's mutex is the kernel flock on the open file description:
// it is released automatically when the holder dies, so a crashed tick's
// lock is recovered by acquisition itself -- no unlink of any pathname is
// ever part of the protocol, and a stale-looking identity record can never
// talk the implementation into deleting a replacement owner's live lock.

func TestIdlePoolTickLockRecoversCrashedHolderWithoutUnlink(t *testing.T) {
	root := t.TempDir()
	writeIdlePoolTickLock(t, root, idlePoolTickLockIdentity{
		Version:   idlePoolTickLockIdentityVersion,
		Host:      mustHostname(t),
		PID:       provenDeadPID(t),
		StartedAt: time.Now().UTC(),
	})

	ran := false
	err := withIdlePoolTickLock(root, func() error { ran = true; return nil })
	if err != nil {
		t.Fatalf("lock left by a crashed holder must be recovered by acquisition: %v", err)
	}
	if !ran {
		t.Fatal("recovered lock never ran the tick")
	}
	// The lock file is never unlinked -- it persists as a forensics record
	// rewritten by the new holder.
	data, readErr := os.ReadFile(idlePoolLockPath(root))
	if readErr != nil {
		t.Fatalf("lock file must survive recovery, never be unlinked: %v", readErr)
	}
	var identity idlePoolTickLockIdentity
	if json.Unmarshal(data, &identity) != nil || identity.PID != os.Getpid() {
		t.Fatalf("recovered lock must carry the new holder identity, got %s", string(data))
	}
}

func TestIdlePoolTickLockSparesLiveFlockHolder(t *testing.T) {
	root := t.TempDir()
	// A live holder is simulated the only way that is honest: by actually
	// holding the kernel flock on a separate open file description.
	writeIdlePoolTickLockRaw(t, root, `{"version":1,"host":"h","pid":1}`)
	f, err := os.OpenFile(idlePoolLockPath(root), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("test could not take the flock: %v", err)
	}

	ran := false
	err = withIdlePoolTickLock(root, func() error { ran = true; return nil })
	if !errors.Is(err, ErrIdlePoolTickBusy) {
		t.Fatalf("live flock holder must report the benign busy sentinel, got err=%v", err)
	}
	if ran {
		t.Fatal("tick must not run while another holder owns the flock")
	}
	if _, statErr := os.Stat(idlePoolLockPath(root)); statErr != nil {
		t.Fatalf("live holder's lock file must never be unlinked: %v", statErr)
	}

	// Releasing the flock (what a crash or clean exit does) must make the
	// next tick acquire immediately.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	ran = false
	if err := withIdlePoolTickLock(root, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("tick must acquire after the holder releases or dies: %v", err)
	}
	if !ran {
		t.Fatal("tick never ran after the flock was released")
	}
}

func TestIdlePoolTickLockCorruptRecordNeverWedgesNorUnlinks(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "corrupt record", body: "{not json"},
		{name: "version mismatch", body: `{"version":999,"host":"h","pid":1}`},
		{name: "foreign host", body: `{"version":1,"host":"other-host","pid":1}`},
		{name: "host unreadable", body: `{"version":1,"pid":1}`},
		{name: "pidless", body: `{"version":1,"host":"h","pid":0}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeIdlePoolTickLockRaw(t, root, tc.body)
			ran := false
			err := withIdlePoolTickLock(root, func() error { ran = true; return nil })
			if err != nil {
				t.Fatalf("an identity record can never decide liveness -- the kernel flock arbitrates, got err=%v", err)
			}
			if !ran {
				t.Fatal("tick never ran")
			}
			// The record is rewritten in place under the held flock, never
			// removed: the file is preserved, now carrying the new holder.
			data, readErr := os.ReadFile(idlePoolLockPath(root))
			if readErr != nil {
				t.Fatalf("lock file must survive, never be unlinked: %v", readErr)
			}
			var identity idlePoolTickLockIdentity
			if json.Unmarshal(data, &identity) != nil || identity.PID != os.Getpid() {
				t.Fatalf("rewritten record must carry the new holder identity, got %s", string(data))
			}
		})
	}
}

func TestIdlePoolTickLockRecordsOwnerGenerationIdentity(t *testing.T) {
	root := t.TempDir()
	err := withIdlePoolTickLock(root, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// The identity contract is that a lock file present mid-tick is
	// parseable owner/generation evidence, and that the file persists after
	// release (it is never unlinked) with the last holder's identity.
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- withIdlePoolTickLock(root, func() error { <-release; return nil })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, readErr := os.ReadFile(idlePoolLockPath(root))
		if readErr == nil {
			var identity idlePoolTickLockIdentity
			if json.Unmarshal(data, &identity) != nil {
				t.Fatalf("live lock must carry parseable owner/generation identity: %s", string(data))
			}
			if identity.Version != idlePoolTickLockIdentityVersion || identity.PID != os.Getpid() || identity.StartedAt.IsZero() {
				t.Fatalf("live lock identity=%+v", identity)
			}
			close(release)
			if goErr := <-holderErr; goErr != nil {
				t.Fatalf("holder tick: %v", goErr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder never acquired the lock: %v", readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestIdlePoolTickLockReleaseNeverUnlinksReplacementOwnerLock pins the
// ownership invariant: a holder releasing (or crashing out of) the tick lock
// releases the kernel flock and nothing else. If a replacement owner's lock
// file came to occupy the same pathname, the outgoing holder's release must
// leave that file exactly as it found it.
func TestIdlePoolTickLockReleaseNeverUnlinksReplacementOwnerLock(t *testing.T) {
	root := t.TempDir()
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- withIdlePoolTickLock(root, func() error { <-release; return nil })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(idlePoolLockPath(root)); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat lock: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("holder never acquired the lock")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A replacement owner recreates the lock file at the same pathname (the
	// pathname-based protocol hazard this guard exists for).
	if err := os.Remove(idlePoolLockPath(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idlePoolLockPath(root), []byte("replacement-owner-lock"), 0o600); err != nil {
		t.Fatal(err)
	}

	close(release)
	if err := <-holderErr; err != nil {
		t.Fatalf("holder tick: %v", err)
	}
	data, err := os.ReadFile(idlePoolLockPath(root))
	if err != nil {
		t.Fatalf("replacement owner's lock was unlinked by the outgoing holder: %v", err)
	}
	if string(data) != "replacement-owner-lock" {
		t.Fatalf("replacement owner's lock was clobbered, got %q", string(data))
	}
}

// TestIdlePoolTickLockTwoStaleContendersCannotBothAcquire pins the
// serialization invariant with real concurrent processes: after a crashed
// holder's lock file (a provably dead pid) is left behind, two fresh
// contender processes race for the lock, and the markers they write while
// holding it must never overlap -- exactly one contender runs its critical
// section at a time. Under a pathname remove-and-recreate protocol both
// contenders can read the same dead record and delete each other's locks;
// under the flock protocol the second contender always loses cleanly.
func TestIdlePoolTickLockTwoStaleContendersCannotBothAcquire(t *testing.T) {
	deadPID := provenDeadPID(t)
	for pair := 0; pair < 10; pair++ {
		pair := pair
		t.Run(fmt.Sprintf("pair-%d", pair), func(t *testing.T) {
			root := t.TempDir()
			writeIdlePoolTickLock(t, root, idlePoolTickLockIdentity{
				Version:   idlePoolTickLockIdentityVersion,
				Host:      mustHostname(t),
				PID:       deadPID,
				StartedAt: time.Now().UTC(),
			})
			runTwoLockContenders(t, root)
		})
	}
}

// runTwoLockContenders spawns two helper processes that barrier together,
// then repeatedly contend for the tick lock, writing start/end markers for
// every held critical section. Any overlap fails the test.
func runTwoLockContenders(t *testing.T, root string) {
	t.Helper()
	type childResult struct {
		out string
		err error
	}
	results := make(chan childResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			cmd := exec.Command(os.Args[0], "-test.run=TestIdlePoolTickLockChildHelper", "-test.v")
			cmd.Env = append(os.Environ(), "HERD_IDLE_LOCK_HELPER_DIR="+root)
			out, err := cmd.CombinedOutput()
			results <- childResult{string(out), err}
		}()
	}
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("helper failed: %v\n%s", r.err, r.out)
		}
	}
	assertNoLockOverlap(t, root)
}

func assertNoLockOverlap(t *testing.T, root string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "contender-markers"))
	if err != nil {
		t.Fatalf("contenders never wrote markers: %v", err)
	}
	// Markers are append-ordered across both processes, so mutual exclusion
	// is exactly: no start may appear while any critical section is open.
	open := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			t.Fatalf("malformed marker %q", line)
		}
		switch parts[1] {
		case "start":
			if open > 0 {
				t.Fatalf("two contenders held the tick lock concurrently: %q (full log:\n%s)", line, string(data))
			}
			open++
		case "end":
			open--
			if open < 0 {
				t.Fatalf("end marker without start: %q", line)
			}
		default:
			t.Fatalf("malformed marker %q", line)
		}
	}
	if open != 0 {
		t.Fatalf("critical sections never ended: %d open", open)
	}
}

// TestIdlePoolTickLockChildHelper is not a test in itself; it is the
// process-reentry body the two-contender race test executes. A normal suite
// run falls through immediately.
func TestIdlePoolTickLockChildHelper(t *testing.T) {
	dir := os.Getenv("HERD_IDLE_LOCK_HELPER_DIR")
	if dir == "" {
		return
	}
	appendMarker := func(line string) {
		f, err := os.OpenFile(filepath.Join(dir, "contender-markers"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "marker:", err)
			os.Exit(4)
		}
		defer f.Close()
		if _, err := fmt.Fprintln(f, line); err != nil {
			os.Exit(4)
		}
	}
	// Barrier: each contender records readiness, then both start contending
	// at the same moment so both can observe the stale lock.
	ready := filepath.Join(dir, "contender-ready")
	f, err := os.OpenFile(ready, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, os.Getpid())
	f.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, readErr := os.ReadFile(ready)
		if readErr == nil && strings.Count(string(data), "\n") >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("barrier never satisfied")
		}
		time.Sleep(2 * time.Millisecond)
	}
	for round := 0; round < 5; round++ {
		err := withIdlePoolTickLock(dir, func() error {
			appendMarker(fmt.Sprintf("%d:start:r%d", os.Getpid(), round))
			time.Sleep(30 * time.Millisecond)
			appendMarker(fmt.Sprintf("%d:end:r%d", os.Getpid(), round))
			return nil
		})
		if err != nil && !errors.Is(err, ErrIdlePoolTickBusy) {
			t.Fatalf("contender: %v", err)
		}
	}
}
