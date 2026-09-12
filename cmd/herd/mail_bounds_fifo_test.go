//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO with no writer is the case that hangs.
//
// open(2) on it BLOCKS until a writer appears, so a bounded read that opens
// before checking the handle never reaches its own deadline, its own context
// check, or its own regular-file check. That is deterministic, not a race:
// the read simply never returns.
//
// These fixtures drive the REAL CLI against a FIFO in place of each store and
// require it to refuse promptly. Each owns the FIFO it creates and removes it,
// and no writer is ever opened -- a fixture that opened one would prove the
// opposite of what is being tested. They are unix-only because a FIFO is.
//
// REMOTE CI ONLY: these must not be run under a local-execution hold.

// fifoDeadline bounds the fixture itself. If the refusal regresses into a
// block, the test fails on its own timer rather than hanging the suite.
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

// runWithDeadline runs fn and fails if it has not returned in time, so a
// regression reports as a bounded failure instead of a hung run.
func runWithDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(fifoDeadline):
		t.Fatalf("%s did not return within %s; the bounded read blocked in its own open", what, fifoDeadline)
	}
}

// The control store as a writerless FIFO must be refused, not waited on.
func TestBoundedInboxCLIRefusesFIFOControlStore(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	makeFIFO(t, f.box.MailFile)

	runWithDeadline(t, "herd mail inbox --limit over a FIFO control store", func() {
		_, stderr, code := boundsCLI(t, f, "--limit", "10")
		if code == 0 {
			t.Error("a FIFO control store was accepted")
		}
		if !strings.Contains(stderr, "regular file") {
			t.Errorf("refusal must name the regular-file requirement, got: %s", stderr)
		}
	})
}

// The feedback store as a writerless FIFO must be refused just as promptly:
// it is opened on the same page, after the control store has been read.
func TestBoundedInboxCLIRefusesFIFOFeedbackStore(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	makeFIFO(t, filepath.Join(f.feedbackDir, boundsRecipient+".jsonl"))

	runWithDeadline(t, "herd mail inbox --limit over a FIFO feedback store", func() {
		_, stderr, code := boundsCLI(t, f, "--limit", "10")
		if code == 0 {
			t.Error("a FIFO feedback store was accepted")
		}
		if !strings.Contains(stderr, "regular file") {
			t.Errorf("refusal must name the regular-file requirement, got: %s", stderr)
		}
	})
}
