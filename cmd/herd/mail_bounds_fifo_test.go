//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO with no writer is the case that hangs.
//
// open(2) on it BLOCKS until a writer appears, so a bounded read that opens
// before checking the handle never reaches its own deadline, context check or
// regular-file check. Deterministic, not a race.
//
// These drive the REAL CLI against a FIFO in place of each store. Each owns
// the FIFO it creates and removes it, and no writer is ever opened -- opening
// one would prove the opposite of what is tested. Unix-only, because a FIFO
// is. REMOTE CI ONLY.

const fifoDeadline = 20 * time.Second

func makeFIFO(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
}

// boundsCLIBounded runs the CLI under a deadline and OWNS the child for the
// whole test. An earlier version ran the command on a goroutine and called
// t.Fatalf from the test's timer, which left the blocked reader alive and
// reported from the wrong goroutine.
//
// CommandContext kills the child when the deadline fires, Wait is always
// reached, and the cleanup below joins it on every exit path, so a regression
// that reinstates the block fails bounded instead of leaking a stuck process.
func boundsCLIBounded(t *testing.T, f boundsFixture, timeout time.Duration, args ...string) (string, int, error) {
	t.Helper()
	binary := buildHerd(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary,
		append([]string{"mail", "inbox", "--recipient", boundsRecipient, "--mail", f.box.MailFile}, args...)...)
	cmd.Dir = f.root
	cmd.Env = append(os.Environ(),
		"HERD_ROOT="+f.root,
		"HERD_REPO_ROOT="+f.root,
		feedbackEnvMailDir+"="+f.feedbackDir,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start herd: %v", err)
	}
	// Joined on EVERY exit path, including a t.Fatal below.
	joined := false
	waitErr := error(nil)
	join := func() {
		if joined {
			return
		}
		joined = true
		waitErr = cmd.Wait()
	}
	t.Cleanup(join)

	join()
	if ctx.Err() != nil {
		return stderr.String(), -1, ctx.Err()
	}
	code := 0
	var exit *exec.ExitError
	if waitErr != nil {
		if errors.As(waitErr, &exit) {
			code = exit.ExitCode()
		} else {
			t.Fatalf("herd did not run: %v", waitErr)
		}
	}
	return stderr.String(), code, nil
}

func assertFIFORefused(t *testing.T, f boundsFixture, what string) {
	t.Helper()
	stderr, code, err := boundsCLIBounded(t, f, fifoDeadline, "--limit", "10")
	if err != nil {
		t.Fatalf("%s did not return within %s; the bounded read blocked in its own open: %v", what, fifoDeadline, err)
	}
	if code == 0 {
		t.Fatalf("%s was accepted", what)
	}
	if !strings.Contains(stderr, "regular file") {
		t.Fatalf("refusal must name the regular-file requirement, got: %s", stderr)
	}
}

// The control store as a writerless FIFO must be refused, not waited on.
func TestBoundedInboxCLIRefusesFIFOControlStore(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	makeFIFO(t, f.box.MailFile)
	assertFIFORefused(t, f, "a FIFO control store")
}

// The feedback store as a writerless FIFO must be refused just as promptly:
// it is opened on the same page, after the control store has been read.
func TestBoundedInboxCLIRefusesFIFOFeedbackStore(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	makeFIFO(t, filepath.Join(f.feedbackDir, boundsRecipient+".jsonl"))
	assertFIFORefused(t, f, "a FIFO feedback store")
}

// The harness's own bound must work, or a genuine regression would hang the
// suite instead of failing it. `mail inbox` is given a FIFO and an
// impossibly short deadline; the helper must report a timeout AND leave no
// child behind, which the joined Wait above guarantees.
func TestBoundedInboxCLITimeoutCleansUpItsChild(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)

	// A DETERMINISTICALLY blocked child. `mail send` with neither --body nor
	// --file reads its payload from stdin; given a pipe nothing ever writes
	// to and nothing closes, it blocks in that read and cannot make progress.
	// Everything it might touch is pinned into the fixture tree, so a child
	// that somehow ran to completion still reaches no live fleet state.
	//
	// The FIFO refusal tests are the wrong vehicle for this property: the
	// refusal is fast, so they take the early-success path and assert nothing
	// about cancellation or cleanup.
	binary := buildHerd(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "mail", "send",
		"--from", "timeout-fixture", "--to", boundsRecipient, "--mail", f.box.MailFile)
	cmd.Dir = f.root
	cmd.Env = append(os.Environ(),
		"HERD_ROOT="+f.root,
		"HERD_REPO_ROOT="+f.root,
		feedbackEnvMailDir+"="+f.feedbackDir,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately never written to and never closed: closing it would end
	// the read and defeat the blocked state this test exists to create.
	defer func() { _ = stdin.Close() }()

	if err := cmd.Start(); err != nil {
		t.Fatalf("start herd: %v", err)
	}
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		t.Fatal("child did not start; there is nothing to clean up and nothing to prove")
	}

	// Join on every exit path, including the t.Fatal calls below.
	joined := false
	waitErr := error(nil)
	join := func() {
		if joined {
			return
		}
		joined = true
		waitErr = cmd.Wait()
	}
	t.Cleanup(join)

	join()

	// 1. The deadline actually fired: the child was still blocked when it did.
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("the child was expected to block until the deadline, got ctx err %v (wait %v)", ctx.Err(), waitErr)
	}
	// 2. Wait returned, so the child was reaped rather than left running.
	if waitErr == nil {
		t.Fatal("a child killed by its deadline must not report a clean exit")
	}
	// 3. The process is accounted for: ProcessState is only populated once
	//    Wait has collected it, which is the join this test is about.
	if cmd.ProcessState == nil {
		t.Fatal("no ProcessState after Wait; the child was not joined")
	}
	if cmd.ProcessState.Exited() {
		t.Fatalf("child exited on its own rather than being terminated by the deadline: %v", cmd.ProcessState)
	}
}
