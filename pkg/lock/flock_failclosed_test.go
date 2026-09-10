package lock

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Any open or flock failure must fail the acquisition with a non-nil error,
// must never fall back to unsafe mkdir-only semantics, and must have no
// ownership or deletion side effects: no lock directory is created by the
// failed acquirer and a pre-existing lock directory owned by a live holder
// is never touched. These cases inject failure through real filesystem
// state (no test seams), so they compile and behave identically against the
// admitted candidate and the repair.

func TestFlockFileIsDirectoryFailsClosed(t *testing.T) {
	root := t.TempDir()
	l := NewDirLock(filepath.Join(root, "checkout.lock.d"))
	if err := os.Mkdir(l.flockPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Acquire(context.Background(), 0, "flock-file-is-dir-probe"); err == nil {
		t.Fatal("an unopenable flock file (directory) was silently degraded to mkdir-only fallback")
	}
	if _, statErr := os.Lstat(l.dir); !os.IsNotExist(statErr) {
		t.Fatalf("failed acquirer took ownership: lock dir exists, stat=%v", statErr)
	}
}

func TestFlockOpenEACCESFailsClosed(t *testing.T) {
	root := t.TempDir()
	l := NewDirLock(filepath.Join(root, "checkout.lock.d"))
	// The flock file exists but is not openable read-write (mode 0444 as a
	// non-root user), while the lock directory's parent stays writable: the
	// unsafe mkdir-only fallback would succeed here, silently replacing the
	// kernel exclusion with nothing. The acquisition must fail instead.
	if err := os.WriteFile(l.flockPath(), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := l.Acquire(context.Background(), 0, "open-eacces-probe"); err == nil {
		t.Fatal("flock file open EACCES was silently degraded to mkdir-only fallback")
	}
	if _, statErr := os.Lstat(l.dir); !os.IsNotExist(statErr) {
		t.Fatalf("failed acquirer took ownership: lock dir exists, stat=%v", statErr)
	}
}

// A pre-existing lock directory with a LIVE holder must survive a failed
// acquisition untouched: no stale-break, no deletion, no replacement.
func TestFlockErrorLeavesLiveHolderLockUntouched(t *testing.T) {
	root := t.TempDir()
	lockRoot := filepath.Join(root, "lockroot")
	if err := os.MkdirAll(lockRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	l := NewDirLock(filepath.Join(lockRoot, "checkout.lock.d"))
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
	// The flock file cannot be opened for writing (read-only parent), so
	// the acquisition must fail closed without touching the live holder's
	// lock directory.
	if err := os.Chmod(lockRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockRoot, 0o755) })
	if err := l.Acquire(context.Background(), 0, "open-eacces-probe"); err == nil {
		t.Fatal("flock file open EACCES was silently degraded to mkdir-only fallback")
	}
	after, lerr := os.Lstat(l.dir)
	if lerr != nil || !os.SameFile(before, after) {
		t.Fatalf("pre-existing live-holder lock was deleted or replaced: stat=%v err=%v", after, lerr)
	}
}
