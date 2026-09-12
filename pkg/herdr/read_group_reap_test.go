//go:build unix

package herdr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// FAC-36: the bounded-read tests prove the CALL returns promptly. That is not
// the same claim as "the child and everything it spawned are gone", and the
// report made the stronger claim, so it needs the stronger evidence.
//
// These fixtures run a fake herdr that spawns a descendant of its own, records
// both PIDs, and then hangs or floods. After cancellation the test asserts the
// recorded PIDs are actually gone.
//
// Safety, stated rather than assumed:
//   - No live Herdr process is touched. The only PIDs ever probed or signalled
//     come from a pid file written by a script this test created in its own
//     temp directory, and NoLiveEnv is set so a lost override fails the read
//     instead of reaching the operator's fleet.
//   - Liveness probes use signal 0, which delivers nothing.
//   - Cleanup is registered before the child starts and runs on every exit,
//     including a failed assertion, so a fixture cannot leak a process.
//   - Cleanup re-checks the process group before signalling, so PID reuse
//     between recording and cleanup cannot redirect a SIGKILL at a stranger.

// reapPlatformGuard states what this evidence covers. procsignal cancels with
// setpgid + kill(-pgid), which is POSIX; no cross-platform claim is made.
func reapPlatformGuard(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "darwin", "linux":
	default:
		t.Skipf("descendant reaping is POSIX process-group behaviour (setpgid + kill(-pgid)); not claimed for %s", runtime.GOOS)
	}
}

// fakeChildren is what one fixture run reported about itself.
type fakeChildren struct {
	// Shell is the direct child herdr process, which procsignal makes a
	// process-group leader.
	Shell int
	// Descendant is a process the direct child spawned. Reaping it is the
	// whole point: killing only the direct child leaves this one running.
	Descendant int
}

// aliveWithoutSignalling reports whether pid exists. Signal 0 performs the
// existence and permission check and delivers nothing.
func aliveWithoutSignalling(pid int) bool {
	if pid <= 1 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// readRecordedPIDs parses the fixture's pid file. It never fails the test: it
// is called from cleanup as well as from the body.
func readRecordedPIDs(path string) (fakeChildren, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fakeChildren{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return fakeChildren{}, fmt.Errorf("pid file has %d fields, want 2", len(fields))
	}
	shell, err := strconv.Atoi(fields[0])
	if err != nil {
		return fakeChildren{}, err
	}
	descendant, err := strconv.Atoi(fields[1])
	if err != nil {
		return fakeChildren{}, err
	}
	if shell <= 1 || descendant <= 1 {
		return fakeChildren{}, fmt.Errorf("refusing recorded pids %d/%d", shell, descendant)
	}
	return fakeChildren{Shell: shell, Descendant: descendant}, nil
}

// installReapFake writes a fake herdr that spawns its own descendant, records
// both PIDs atomically, and then behaves per mode. It registers the mandatory
// cleanup BEFORE the child can start.
func installReapFake(t *testing.T, mode string) (pidPath string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	pidPath = filepath.Join(dir, "pids")
	modePath := filepath.Join(dir, "mode")
	if err := os.WriteFile(modePath, []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}

	// The pid file is written to a sibling and renamed, so a reader can never
	// observe a half-written record.
	script := `#!/bin/sh
sleep 600 &
printf '%s %s\n' "$$" "$!" > "$HERD_FAKE_PIDS.partial"
mv "$HERD_FAKE_PIDS.partial" "$HERD_FAKE_PIDS"
case "$(cat "$HERD_FAKE_MODE")" in
  flood)
    b=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
    b=$b$b$b$b ; b=$b$b$b$b ; b=$b$b$b$b ; b=$b$b$b$b
    while : ; do printf '%s' "$b" ; done
    ;;
  *)
    sleep 600
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(NoLiveEnv, "1")
	t.Setenv(BinaryEnv, bin)
	t.Setenv("HERD_FAKE_PIDS", pidPath)
	t.Setenv("HERD_FAKE_MODE", modePath)

	// Mandatory cleanup, registered before anything can be spawned. It runs on
	// every exit path, a failed assertion included.
	t.Cleanup(func() {
		kids, err := readRecordedPIDs(pidPath)
		if err != nil {
			return // nothing was ever recorded; nothing of ours to reap
		}
		for _, pid := range []int{kids.Descendant, kids.Shell} {
			if !aliveWithoutSignalling(pid) {
				continue
			}
			// PID-reuse guard: only signal a process still in the group this
			// fixture created. A recycled PID belongs to someone else.
			if pgid, pgErr := syscall.Getpgid(pid); pgErr != nil || pgid != kids.Shell {
				t.Logf("leftover pid %d is no longer in fixture group %d; leaving it alone", pid, kids.Shell)
				continue
			}
			t.Logf("cleanup reaped leftover fixture pid %d", pid)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return pidPath
}

// awaitRecordedPIDs waits for the fixture to report itself, then proves the
// descendant really is in the direct child's process group — otherwise the
// test would be measuring a lone process and calling it group reaping.
func awaitRecordedPIDs(t *testing.T, pidPath string, within time.Duration) fakeChildren {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		kids, err := readRecordedPIDs(pidPath)
		if err == nil && aliveWithoutSignalling(kids.Descendant) {
			pgid, pgErr := syscall.Getpgid(kids.Descendant)
			if pgErr != nil {
				t.Fatalf("cannot read the descendant's process group: %v", pgErr)
			}
			if pgid != kids.Shell {
				t.Fatalf("descendant pgid %d != child pid %d; this fixture is not exercising group reaping", pgid, kids.Shell)
			}
			return kids
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("fixture never recorded a live descendant within %s (last error: %v)", within, lastErr)
	return fakeChildren{}
}

// requireGone polls until the pid disappears, or fails. Errorf, not Fatalf, so
// both PIDs are reported in one run.
func requireGone(t *testing.T, label string, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !aliveWithoutSignalling(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%s (pid %d) was still alive %s after cancellation; the process group was not reaped", label, pid, within)
}

// Cancelling a hung read must reap the whole owned group, not just the direct
// child. A descendant left running is a leaked process per sweep.
func TestRunHerdrReadContextReapsDescendantsOnCancel(t *testing.T) {
	reapPlatformGuard(t)
	pidPath := installReapFake(t, "hang")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		_, err := runHerdrReadContextReal(ctx, DefaultReadTransportLimit, "pane", "read", "wT:p1")
		done <- outcome{err: err, elapsed: time.Since(start)}
	}()

	kids := awaitRecordedPIDs(t, pidPath, 20*time.Second)
	cancel()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("a cancelled read returned success")
		}
		if !errors.Is(got.err, context.Canceled) && !strings.Contains(got.err.Error(), "context canceled") {
			t.Fatalf("cancelled read error = %v, want a cancellation", got.err)
		}
		if got.elapsed > 20*time.Second {
			t.Fatalf("cancelled read returned after %s", got.elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancelled read never returned; it is holding the caller")
	}

	requireGone(t, "direct herdr child", kids.Shell, 10*time.Second)
	requireGone(t, "descendant spawned by the child", kids.Descendant, 10*time.Second)
}

// The byte bound stops the child the same way. An overflowing child that keeps
// a descendant alive is the same leak with a different trigger.
func TestRunHerdrReadContextReapsDescendantsOnOverflow(t *testing.T) {
	reapPlatformGuard(t)
	pidPath := installReapFake(t, "flood")

	type outcome struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		// A small bound so the overflow fires immediately; the flood block is
		// 8KiB, so this is reached in a couple of writes.
		_, err := runHerdrReadContextReal(context.Background(), 64<<10, "pane", "read", "wT:p1")
		done <- outcome{err: err, elapsed: time.Since(start)}
	}()

	kids := awaitRecordedPIDs(t, pidPath, 20*time.Second)

	select {
	case got := <-done:
		if !errors.Is(got.err, ErrReadTransportOversized) {
			t.Fatalf("err = %v, want ErrReadTransportOversized", got.err)
		}
		if got.elapsed > 20*time.Second {
			t.Fatalf("overflowing read returned after %s", got.elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("overflowing read never returned; the byte bound did not stop the child")
	}

	requireGone(t, "direct herdr child", kids.Shell, 10*time.Second)
	requireGone(t, "descendant spawned by the child", kids.Descendant, 10*time.Second)
}

// The control: without cancellation the fixture's descendant stays alive, so
// the two assertions above are not passing because the descendant was never
// there or exits on its own.
func TestReapFixtureDescendantOutlivesAnUncancelledRead(t *testing.T) {
	reapPlatformGuard(t)
	pidPath := installReapFake(t, "hang")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = runHerdrReadContextReal(ctx, DefaultReadTransportLimit, "pane", "read", "wT:p1")
	}()

	kids := awaitRecordedPIDs(t, pidPath, 20*time.Second)
	// Long enough that a descendant which exits on its own would have done so,
	// short enough to keep the suite bounded.
	time.Sleep(500 * time.Millisecond)
	if !aliveWithoutSignalling(kids.Descendant) {
		t.Fatal("the fixture descendant died without cancellation; the reaping assertions would pass vacuously")
	}
	if !aliveWithoutSignalling(kids.Shell) {
		t.Fatal("the fixture child died without cancellation; the reaping assertions would pass vacuously")
	}
	// cancel() via defer, then the registered cleanup reaps anything left.
}
