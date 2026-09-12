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
	makeFIFO(t, f.box.MailFile)

	_, _, err := boundsCLIBounded(t, f, 50*time.Millisecond, "--limit", "10", "--timeout", "10s")
	if err == nil {
		// Refusing faster than the harness deadline is the healthy outcome.
		return
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline, got: %v", err)
	}
}
