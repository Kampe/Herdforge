package lock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deadPID is a pid that cannot exist on any platform, so syscall.Kill(pid, 0)
// reliably returns ESRCH (never PID 1).
const deadPID = "999999999"

func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvHeld, "") // reset re-entrancy marker per test
	return dir
}

func newTestLock(dir string) *DirLock {
	l := NewDirLock(filepath.Join(dir, "lock.d"))
	l.SetMaxAge(300 * time.Second)
	return l
}

func TestDirLockAcquire(t *testing.T) {
	t.Run("fresh acquire creates dir and holder", func(t *testing.T) {
		dir := tempDir(t)
		l := newTestLock(dir)
		defer t.Setenv(EnvHeld, "")

		if err := l.Acquire(context.Background(), time.Second, "unit-test"); err != nil {
			t.Fatalf("fresh acquire: %v", err)
		}
		if _, err := os.Stat(l.Dir()); err != nil {
			t.Fatalf("lockdir missing after acquire: %v", err)
		}
		// touch the lock so it is never stale during the assertion window
		hot, _ := os.ReadFile(filepath.Join(l.Dir(), HolderFile))
		if !strings.Contains(string(hot), "pid=") || !strings.Contains(string(hot), "reason=unit-test") {
			t.Fatalf("holder content = %q", hot)
		}
	})

	t.Run("reentrant returns nil and does not recreate", func(t *testing.T) {
		dir := tempDir(t)
		lockDir := filepath.Join(dir, "h")
		l := NewDirLock(lockDir)
		if err := l.Acquire(context.Background(), time.Second, "outer"); err != nil {
			t.Fatalf("outer acquire: %v", err)
		}
		modBefore, _ := os.Stat(lockDir)
		t.Setenv(EnvHeld, lockDir)
		if err := l.Acquire(context.Background(), time.Second, "inner"); err != nil {
			t.Fatalf("reentrant acquire should succeed: %v", err)
		}
		modAfter, err := os.Stat(lockDir)
		if err != nil {
			t.Fatalf("lockdir vanished during reentrant acquire: %v", err)
		}
		if !modBefore.ModTime().Equal(modAfter.ModTime()) {
			t.Fatal("reentrant acquire modified an already-held lock")
		}
	})

	// FAC-138: the marker names ONE lockdir. Treating any non-empty value as
	// "held" granted every OTHER lock for free — so the forge coordinator
	// fence, taken underneath `herd lock with`, was never actually held.
	t.Run("marker for a different lockdir does not grant this lock", func(t *testing.T) {
		dir := tempDir(t)
		other := NewDirLock(filepath.Join(dir, "other.d"))
		fence := NewDirLock(filepath.Join(dir, "fence.d"))
		t.Setenv(EnvHeld, other.Dir())

		if err := fence.Acquire(context.Background(), 0, "first"); err != nil {
			t.Fatalf("first fence acquire: %v", err)
		}
		if _, err := os.Stat(fence.Dir()); err != nil {
			t.Fatalf("fence did not take the filesystem lock: %v", err)
		}
		// A second live coordinator must be refused, marker or not.
		if err := NewDirLock(fence.Dir()).Acquire(context.Background(), 0, "second"); err == nil {
			t.Fatal("second holder acquired a fence already held by a live process")
		}
	})

	t.Run("timeout on young live lock returns error with holder", func(t *testing.T) {
		dir := tempDir(t)
		lockDir := filepath.Join(dir, "held")
		// Create a young lockdir owned by a live pid (this test process).
		writeLockHold(t, lockDir)
		l := NewDirLock(lockDir)
		err := l.Acquire(context.Background(), 2*time.Second, "reporter")
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if !strings.Contains(err.Error(), "locked by") || !strings.Contains(err.Error(), "waited 2s") {
			t.Fatalf("timeout error = %v", err)
		}
		if _, statErr := os.Stat(lockDir); statErr != nil {
			t.Fatalf("locked dir removed during timeout: %v", statErr)
		}
	})

	t.Run("wait 0 errors immediately on young lock", func(t *testing.T) {
		dir := tempDir(t)
		lockDir := filepath.Join(dir, "l")
		writeLockHold(t, lockDir)
		start := time.Now()
		err := NewDirLock(lockDir).Acquire(context.Background(), 0, "r")
		if err == nil {
			t.Fatal("expected immediate timeout")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("wait=0 took %v", elapsed)
		}
	})
}

func TestDirLockDeadHolderRecovery(t *testing.T) {
	t.Run("dead pid breaks lock and acquire wins", func(t *testing.T) {
		dir := tempDir(t)
		lockDir := filepath.Join(dir, "dead")
		writeLockWithPID(t, lockDir, deadPID)
		l := NewDirLock(lockDir)
		if err := l.Acquire(context.Background(), time.Second, "after-crash"); err != nil {
			t.Fatalf("acquire after dead holder: %v", err)
		}
		b, _ := os.ReadFile(filepath.Join(lockDir, "holder"))
		// holder must be ours now (not the dead pid)
		if strings.Contains(string(b), "pid="+deadPID) {
			t.Fatalf("dead holder survived: %q", b)
		}
	})

	t.Run("age-bound lock is broken before acquire", func(t *testing.T) {
		dir := tempDir(t)
		lockDir := filepath.Join(dir, "aged")
		// Live pid (this test process) so ONLY the age rule can fire.
		writeLockWithPID(t, lockDir, "0")
		l := NewDirLock(lockDir)
		l.SetMaxAge(10 * time.Millisecond)
		old := time.Now().Add(-time.Minute)
		if err := os.Chtimes(lockDir, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if err := l.Acquire(context.Background(), time.Second, "after-age"); err != nil {
			t.Fatalf("acquire after aged lock: %v", err)
		}
	})

	t.Run("young lock with dead pid still breaks (dead beats mtime)", func(t *testing.T) {
		dir := tempDir(t)
		lockDir := filepath.Join(dir, "young-dead")
		writeLockWithPID(t, lockDir, deadPID)
		if err := os.Chtimes(lockDir, time.Now(), time.Now()); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		l := NewDirLock(lockDir)
		if err := l.Acquire(context.Background(), time.Second, "r"); err != nil {
			t.Fatalf("dead-holder young lock should break: %v", err)
		}
	})
}

func TestDirLockStaleAge(t *testing.T) {
	t.Run("dir older than maxAge is stale", func(t *testing.T) {
		lockDir := filepath.Join(t.TempDir(), "o")
		// Live pid so only the age rule can fire.
		writeLockWithPID(t, lockDir, "0")
		l := NewDirLock(lockDir)
		l.SetMaxAge(time.Minute)
		old := time.Now().Add(-2 * time.Minute)
		if err := os.Chtimes(lockDir, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if !l.breakIfStale() {
			t.Fatal("expected stale by age")
		}
		if _, err := os.Stat(lockDir); err == nil {
			t.Fatal("stale dir not removed")
		}
	})

	t.Run("dir younger than maxAge is not stale", func(t *testing.T) {
		dir := t.TempDir()
		lockDir := filepath.Join(dir, "y")
		// live pid, young dir
		writeLockWithPID(t, lockDir, "0")
		l := NewDirLock(lockDir)
		l.SetMaxAge(time.Hour)
		if l.breakIfStale() {
			t.Fatal("young live lock wrongly broken")
		}
		if _, err := os.Stat(lockDir); err != nil {
			t.Fatalf("young lock dir removed: %v", err)
		}
	})

	t.Run("missing lockdir not stale", func(t *testing.T) {
		l := NewDirLock(filepath.Join(t.TempDir(), "none"))
		if l.breakIfStale() {
			t.Fatal("missing lockdir reported stale")
		}
	})
}

func TestDirLockRelease(t *testing.T) {
	t.Run("release removes dir and dir can be re-made", func(t *testing.T) {
		dir := tempDir(t)
		lockDir := filepath.Join(dir, "r")
		l := NewDirLock(lockDir)
		if err := l.Acquire(context.Background(), time.Second, "r"); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		l.Release()
		if _, err := os.Stat(lockDir); err == nil {
			t.Fatal("dir still present after release")
		}
		if err := os.Mkdir(lockDir, 0o755); err != nil {
			t.Fatalf("raw mkdir after release failed (advisory leak): %v", err)
		}
	})
}

func TestDirLockStatus(t *testing.T) {
	t.Run("unlocked", func(t *testing.T) {
		held, holder := NewDirLock(filepath.Join(t.TempDir(), "u")).Status()
		if held {
			t.Fatalf("held on missing dir: %q", holder)
		}
		if holder != "" {
			t.Fatalf("holder = %q", holder)
		}
	})

	t.Run("locked reports holder", func(t *testing.T) {
		dir := t.TempDir()
		lockDir := filepath.Join(dir, "s")
		writeLockWithPID(t, lockDir, "424242")
		held, holder := NewDirLock(lockDir).Status()
		if !held {
			t.Fatal("expected held")
		}
		if !strings.Contains(holder, "pid=424242") {
			t.Fatalf("holder = %q", holder)
		}
	})
}

// writeLockWithPID writes a holder file with the given pid (fixture).
func writeLockWithPID(t *testing.T, lockDir, pid string) {
	t.Helper()
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("mk hinted: %v", err)
	}
	content := "pid=" + pid + "\nagent=test-fixture\nreason=fixture\n"
	if err := os.WriteFile(filepath.Join(lockDir, "holder"), []byte(content), 0o644); err != nil {
		t.Fatalf("write holder: %v", err)
	}
}

// writeLockHold writes a live-pid holder (this test process).
func writeLockHold(t *testing.T, lockDir string) {
	t.Helper()
	writeLockWithPID(t, lockDir, "0")
}
