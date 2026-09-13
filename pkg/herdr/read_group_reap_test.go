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
	"sync"
	"syscall"
	"testing"
	"time"
)

// FAC-36: the bounded-read tests prove the CALL returns promptly. That is not
// the same claim as "the child and everything it spawned are gone", and the
// report made the stronger claim, so it needs the stronger evidence.
//
// These fixtures run a fake herdr that spawns a descendant of its own, records
// both PIDs, and then BLOCKS on a handshake until the test has inspected them.
// Only then does the test release it to hang or flood, cancel, and require the
// recorded PIDs to be gone.
//
// The handshake is the point. An earlier version let the flood start
// immediately, so a correct implementation could fill the byte bound and be
// killed BEFORE the poller ever observed a live descendant — and the fixture
// would then wait out its whole timeout on an already-dead child and fail a
// working implementation. Readiness is now established by a blocking
// rendezvous on a FIFO, never by sleeping and assuming.
//
// Safety, stated rather than assumed:
//   - No live Herdr process is touched, and no shared process group is ever
//     signalled. The only PIDs probed or signalled come from a pid file
//     written by a script this test created in its own temp directory, and
//     NoLiveEnv is set so a lost override fails the read instead of reaching
//     the operator's fleet.
//   - Liveness probes use signal 0, which delivers nothing.
//   - The fixture spawns EXACTLY ONE long-lived descendant and records it.
//     The shell then blocks in builtins (read, wait) or a builtin printf loop,
//     so there is no unrecorded sleeper for cleanup to miss.
//   - Cleanup is registered before the child starts and runs on every exit,
//     including a failed assertion. The observation goroutine is joined on
//     every exit path too, so no test leaves a read running.
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
	// Descendant is the one process the direct child spawns. Reaping it is the
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

// reapFixture is one hermetic fake herdr and the rendezvous that holds it.
type reapFixture struct {
	pidPath string
	goPath  string
}

// installReapFake writes a fake herdr that spawns exactly one descendant,
// records both PIDs atomically, and then BLOCKS until released. It registers
// the mandatory cleanup before the child can start.
func installReapFake(t *testing.T, mode string) *reapFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	fixture := &reapFixture{
		pidPath: filepath.Join(dir, "pids"),
		goPath:  filepath.Join(dir, "release.fifo"),
	}
	modePath := filepath.Join(dir, "mode")
	// Trailing newline: `read` wants a complete line.
	if err := os.WriteFile(modePath, []byte(mode+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fixture.goPath, 0o600); err != nil {
		t.Fatalf("create release fifo: %v", err)
	}

	// Every command here is a shell builtin except `mv`, which is transient
	// and has exited before the pid file it produces becomes readable — so at
	// the moment the test inspects, the only live processes are the recorded
	// two. The pid file is written to a sibling and renamed so a reader can
	// never observe a half-written record.
	script := `#!/bin/sh
read -r mode < "$HERD_FAKE_MODE"
sleep 600 &
desc=$!
printf '%s %s\n' "$$" "$desc" > "$HERD_FAKE_PIDS.partial"
mv "$HERD_FAKE_PIDS.partial" "$HERD_FAKE_PIDS"
read -r _ < "$HERD_FAKE_GO"
case "$mode" in
  flood)
    b=xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
    b=$b$b$b$b ; b=$b$b$b$b ; b=$b$b$b$b ; b=$b$b$b$b
    while : ; do printf '%s' "$b" ; done
    ;;
  *)
    wait "$desc"
    ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(NoLiveEnv, "1")
	t.Setenv(BinaryEnv, bin)
	t.Setenv("HERD_FAKE_PIDS", fixture.pidPath)
	t.Setenv("HERD_FAKE_MODE", modePath)
	t.Setenv("HERD_FAKE_GO", fixture.goPath)

	// Mandatory cleanup, registered before anything can be spawned. It runs on
	// every exit path, a failed assertion included. Registered FIRST, so it
	// runs LAST: the read's own cleanup has already cancelled and joined by
	// the time this looks for leftovers.
	t.Cleanup(func() {
		kids, err := readRecordedPIDs(fixture.pidPath)
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
	return fixture
}

// release completes the rendezvous, letting the held fixture proceed.
//
// The FIFO is opened O_RDWR: on a FIFO that never blocks, so a test can never
// wedge here even if the child died early, and holding a read end guarantees
// the one-byte write fits in the pipe buffer rather than blocking on a reader.
func (f *reapFixture) release(t *testing.T) {
	t.Helper()
	fh, err := os.OpenFile(f.goPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open release fifo: %v", err)
	}
	defer fh.Close()
	if _, err := fh.Write([]byte("go\n")); err != nil {
		t.Fatalf("release the held fixture: %v", err)
	}
}

// awaitRecordedPIDs waits for the fixture to report itself, then proves the
// descendant really is in the direct child's process group — otherwise the
// test would be measuring a lone process and calling it group reaping.
//
// This runs while the fixture is HELD at the rendezvous, so the child cannot
// have finished its work and exited first. The poll interval is a poll
// interval on an observable condition, not a sleep standing in for readiness.
func awaitRecordedPIDs(t *testing.T, f *reapFixture, within time.Duration) fakeChildren {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		kids, err := readRecordedPIDs(f.pidPath)
		if err == nil {
			if !aliveWithoutSignalling(kids.Descendant) {
				t.Fatalf("recorded descendant %d is already gone while the fixture is still held at the rendezvous", kids.Descendant)
			}
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
	t.Fatalf("fixture never recorded its pids within %s (last error: %v)", within, lastErr)
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

type readOutcome struct {
	err     error
	elapsed time.Duration
}

// readRun is one background observation read, with its goroutine owned.
type readRun struct {
	done   chan readOutcome
	cancel context.CancelFunc
	mu     sync.Mutex
	got    *readOutcome
}

// startRead runs one bounded read in the background under a FINITE context,
// and guarantees the goroutine is joined on every exit path: the registered
// cleanup cancels and waits for it, so no test can leave a read running.
func startRead(t *testing.T, limit int, bound time.Duration) *readRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	run := &readRun{done: make(chan readOutcome, 1), cancel: cancel}
	start := time.Now()
	go func() {
		_, err := runHerdrReadContextReal(ctx, limit, "pane", "read", "wT:p1")
		run.done <- readOutcome{err: err, elapsed: time.Since(start)}
	}()
	t.Cleanup(func() { run.join(t) })
	return run
}

// await blocks for the read's outcome, recording it so join does not wait on
// an already-drained channel.
func (r *readRun) await(t *testing.T, within time.Duration) readOutcome {
	t.Helper()
	r.mu.Lock()
	if r.got != nil {
		out := *r.got
		r.mu.Unlock()
		return out
	}
	r.mu.Unlock()
	select {
	case out := <-r.done:
		r.mu.Lock()
		r.got = &out
		r.mu.Unlock()
		return out
	case <-time.After(within):
		t.Fatalf("the read did not return within %s; it is holding the caller", within)
		return readOutcome{}
	}
}

// join cancels and waits. Safe to call after await.
func (r *readRun) join(t *testing.T) {
	r.cancel()
	r.mu.Lock()
	drained := r.got != nil
	r.mu.Unlock()
	if drained {
		return
	}
	select {
	case out := <-r.done:
		r.mu.Lock()
		r.got = &out
		r.mu.Unlock()
	case <-time.After(30 * time.Second):
		t.Errorf("the observation goroutine did not return 30s after cancellation")
	}
}

// Cancelling a hung read must reap the whole owned group, not just the direct
// child. A descendant left running is a leaked process per sweep.
func TestRunHerdrReadContextReapsDescendantsOnCancel(t *testing.T) {
	reapPlatformGuard(t)
	fixture := installReapFake(t, "hang")
	run := startRead(t, DefaultReadTransportLimit, 60*time.Second)

	// Inspected while the fixture is HELD, so liveness is established rather
	// than assumed.
	kids := awaitRecordedPIDs(t, fixture, 20*time.Second)
	fixture.release(t)

	run.cancel()
	got := run.await(t, 20*time.Second)
	if got.err == nil {
		t.Fatal("a cancelled read returned success")
	}
	if !errors.Is(got.err, context.Canceled) && !strings.Contains(got.err.Error(), "context canceled") {
		t.Fatalf("cancelled read error = %v, want a cancellation", got.err)
	}
	if got.elapsed > 20*time.Second {
		t.Fatalf("cancelled read returned after %s", got.elapsed)
	}

	requireGone(t, "direct herdr child", kids.Shell, 10*time.Second)
	requireGone(t, "descendant spawned by the child", kids.Descendant, 10*time.Second)
}

// The byte bound stops the child the same way. An overflowing child that keeps
// a descendant alive is the same leak with a different trigger.
//
// Order matters and is the fix for a real race: the fixture is held BEFORE it
// floods, so the descendant is proven live and in the owned group first, and
// only then is the flood released. Without the handshake a correct
// implementation could overflow and be killed before the poller ever looked.
func TestRunHerdrReadContextReapsDescendantsOnOverflow(t *testing.T) {
	reapPlatformGuard(t)
	fixture := installReapFake(t, "flood")
	// A small bound so the overflow fires immediately once released; the flood
	// block is 8KiB, so this is reached in a handful of writes.
	run := startRead(t, 64<<10, 60*time.Second)

	kids := awaitRecordedPIDs(t, fixture, 20*time.Second)
	fixture.release(t)

	got := run.await(t, 30*time.Second)
	if !errors.Is(got.err, ErrReadTransportOversized) {
		t.Fatalf("err = %v, want ErrReadTransportOversized", got.err)
	}
	if got.elapsed > 30*time.Second {
		t.Fatalf("overflowing read returned after %s", got.elapsed)
	}

	requireGone(t, "direct herdr child", kids.Shell, 10*time.Second)
	requireGone(t, "descendant spawned by the child", kids.Descendant, 10*time.Second)
}

// The control: without cancellation the fixture's descendant stays alive, so
// the two assertions above are not passing because the descendant was never
// there or exits on its own.
//
// Liveness is established by the rendezvous and by the shell blocking in
// `wait`, never by sleeping for a while and assuming.
func TestReapFixtureDescendantOutlivesAnUncancelledRead(t *testing.T) {
	reapPlatformGuard(t)
	fixture := installReapFake(t, "hang")
	run := startRead(t, DefaultReadTransportLimit, 60*time.Second)

	kids := awaitRecordedPIDs(t, fixture, 20*time.Second)
	fixture.release(t)

	// Released and now blocked in `wait`: still alive, with nothing cancelled.
	if !aliveWithoutSignalling(kids.Descendant) {
		t.Fatal("the fixture descendant died without cancellation; the reaping assertions would pass vacuously")
	}
	if !aliveWithoutSignalling(kids.Shell) {
		t.Fatal("the fixture child died without cancellation; the reaping assertions would pass vacuously")
	}

	// Joined explicitly, not left to a deferred cancel.
	run.cancel()
	run.await(t, 20*time.Second)
}
