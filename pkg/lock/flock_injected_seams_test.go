package lock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Seam-injected failure cases: deterministic kernel-level errors (EIO,
// EACCES on the syscall itself, pin-open failure) that cannot be produced
// through real filesystem state alone. These ride the production test seams
// and are exercised against the repaired acquire path.

func injectedFlockErr(t *testing.T, injected error) *DirLock {
	t.Helper()
	saved := flockFn
	flockFn = func(int, int) error { return injected }
	t.Cleanup(func() { flockFn = saved })
	dir := filepath.Join(t.TempDir(), "checkout.lock.d")
	return NewDirLock(dir)
}

func TestFlockInjectedIOErrorFailsClosed(t *testing.T) {
	l := injectedFlockErr(t, syscall.EIO)
	err := l.Acquire(context.Background(), 0, "injected-eio-probe")
	if err == nil {
		t.Fatal("flock EIO was silently degraded to mkdir-only fallback")
	}
	if !strings.Contains(err.Error(), "refusing unsafe mkdir-only fallback") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Lstat(l.dir); !os.IsNotExist(statErr) {
		t.Fatalf("failed acquirer took ownership: lock dir exists, stat=%v", statErr)
	}
}

func TestFlockInjectedAccessErrorFailsClosed(t *testing.T) {
	l := injectedFlockErr(t, syscall.EACCES)
	if err := l.Acquire(context.Background(), 0, "injected-eacces-probe"); err == nil {
		t.Fatal("flock EACCES was silently degraded to mkdir-only fallback")
	}
	if _, statErr := os.Lstat(l.dir); !os.IsNotExist(statErr) {
		t.Fatalf("failed acquirer took ownership: lock dir exists, stat=%v", statErr)
	}
}

// A pre-existing lock directory with a LIVE holder must survive a failed
// (seam-injected) acquisition untouched.
func TestFlockInjectedErrorLeavesLiveHolderLockUntouched(t *testing.T) {
	l := injectedFlockErr(t, syscall.EIO)
	if err := os.Mkdir(l.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	holder := filepath.Join(l.dir, HolderFile)
	content := "pid=" + strconv.Itoa(os.Getpid()) + "\nreason=live-holder-probe\n"
	if err := os.WriteFile(holder, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(l.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Acquire(context.Background(), 0, "injected-eio-probe"); err == nil {
		t.Fatal("flock EIO was silently degraded")
	}
	after, lerr := os.Lstat(l.dir)
	if lerr != nil || !os.SameFile(before, after) {
		t.Fatalf("pre-existing live-holder lock was deleted or replaced: stat=%v err=%v", after, lerr)
	}
}

// A directory that was created (mkdir succeeded) but could not be pinned is
// never left behind as unowned ownership: the acquisition fails and removes
// exactly the directory it just created, while the flock is still held.
func TestPinOpenFailureFailsClosedAndCleansOwnDir(t *testing.T) {
	root := t.TempDir()
	l := NewDirLock(filepath.Join(root, "checkout.lock.d"))
	saved := pinLockDir
	pinLockDir = func(string) (*os.File, error) { return nil, errors.New("injected pin-open failure") }
	t.Cleanup(func() { pinLockDir = saved })
	if err := l.Acquire(context.Background(), 0, "pin-open-failure-probe"); err == nil {
		t.Fatal("a failed directory pin was silently skipped")
	}
	if _, statErr := os.Lstat(l.dir); !os.IsNotExist(statErr) {
		t.Fatalf("failed acquirer left an orphan lock dir: stat=%v", statErr)
	}
	if l.flockFile != nil {
		t.Fatal("failed acquisition left the kernel flock held")
	}
}
