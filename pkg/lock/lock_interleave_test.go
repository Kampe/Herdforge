package lock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Deterministic interleaving reproduction for the check-then-remove race the
// OpenAI evidence review identified (bundle-interleaving-repair-2306): the
// successor is installed INSIDE the interval between the ownership check and
// the directory removal. Production leaves interleaveHook nil, so these
// seams are inert outside tests.

func TestReleaseSkipsSuccessorLockInstalledInTheCheckRemoveInterval(t *testing.T) {
	dir := tempDir(t)
	lockDir := filepath.Join(dir, "r")
	l := NewDirLock(lockDir)
	if err := l.Acquire(context.Background(), time.Second, "owner"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	saved := interleaveHook
	defer func() { interleaveHook = saved }()
	interleaveHook = func(stage string) {
		if stage != "release-pre-remove" {
			return
		}
		// A successor replaces the lock directory after the owner's token
		// check and before the removal: fresh directory, fresh token, live
		// holder pid.
		if err := os.RemoveAll(lockDir); err != nil {
			t.Errorf("successor takeover removeAll: %v", err)
		}
		if err := os.Mkdir(lockDir, 0o755); err != nil {
			t.Errorf("successor takeover mkdir: %v", err)
		}
		successor := NewDirLock(lockDir)
		successor.token = "successor-token"
		successor.writeHolder("successor")
	}
	l.Release()
	if _, err := os.Stat(lockDir); err != nil {
		t.Fatalf("successor lock directory was removed by the stale owner's release: %v", err)
	}
	if got := holderToken(filepath.Join(lockDir, HolderFile)); got != "successor-token" {
		t.Fatalf("successor holder token destroyed by the stale owner's release: got %q", got)
	}
}

func TestStaleBreakSkipsFreshLockInstalledInTheStatRemoveInterval(t *testing.T) {
	dir := tempDir(t)
	lockDir := filepath.Join(dir, "r")
	l := NewDirLock(lockDir)
	if err := os.Mkdir(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	deadPIDHolder := filepath.Join(lockDir, HolderFile)
	if err := os.WriteFile(deadPIDHolder, []byte("pid=999999998\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved := interleaveHook
	defer func() { interleaveHook = saved }()
	interleaveHook = func(stage string) {
		if stage != "stale-pre-remove" {
			return
		}
		// A fresh, live lock replaces the stale one in the interval between
		// the staleness decision and the removal.
		if err := os.RemoveAll(lockDir); err != nil {
			t.Errorf("fresh takeover removeAll: %v", err)
		}
		if err := os.Mkdir(lockDir, 0o755); err != nil {
			t.Errorf("fresh takeover mkdir: %v", err)
		}
		fresh := NewDirLock(lockDir)
		fresh.token = "fresh-token"
		fresh.writeHolder("fresh")
	}
	if removed := l.breakIfStale(); removed {
		t.Fatalf("fresh lock installed during the stale-break interval was removed (removed=%v)", removed)
	}
	if _, err := os.Stat(lockDir); err != nil {
		t.Fatalf("fresh lock directory was removed by the stale break: %v", err)
	}
	if got := holderToken(filepath.Join(lockDir, HolderFile)); got != "fresh-token" {
		t.Fatalf("fresh holder token destroyed by the stale break: got %q", got)
	}
}
