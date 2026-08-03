package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/preflight"
)

// TestMain disables the disk-pressure floors for this package's existing
// worktree tests: they exercise real `git worktree add` fixtures and must
// stay hermetic even when the host itself is under pressure (the FAC-153
// incident host sat at 99%). Tests that assert the guard re-enable floors
// via t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(preflight.EnvDiskMinFreeGB, "0")
	os.Setenv(preflight.EnvDiskMinFreePct, "0")
	os.Setenv(preflight.EnvDiskMinInodePct, "0")
	// Isolate the cross-process reservation ledger: tests must never read
	// from or release into the host's real ledger.
	dir, err := os.MkdirTemp("", "herd-disk-ledger-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv(preflight.EnvDiskLedgerDir, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// An impossibly high floor makes any real volume read as critically low —
// hermetic regardless of actual host headroom.
func setImpossibleFloor(t *testing.T) {
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776") // 1 ZiB
}

func TestCreateTaskWorktreeFromRefusesUnderDiskPressure(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	pool := NewWorktreePool(tmpDir, filepath.Join(tmpDir, "worktrees"))

	setImpossibleFloor(t)
	_, err := pool.CreateTaskWorktree(context.Background(), "FAC-999")
	if err == nil {
		t.Fatal("expected fail-closed refusal under disk pressure")
	}
	if !strings.Contains(err.Error(), "disk_pressure") {
		t.Fatalf("expected disk_pressure evidence, got: %v", err)
	}
	// Refusal happened before ANY mutation: no worktree dir, no anchor ref.
	if _, statErr := os.Stat(filepath.Join(tmpDir, "worktrees", "fac-999")); !os.IsNotExist(statErr) {
		t.Fatalf("worktree path must not exist after refusal, stat err: %v", statErr)
	}
	refPath := filepath.Join(tmpDir, ".git", "refs", "herd", "anchors", "FAC-999")
	if _, statErr := os.Stat(refPath); !os.IsNotExist(statErr) {
		t.Fatalf("anchor ref must not exist after refusal")
	}
}

func TestCreateWorktreeRefusesUnderDiskPressure(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	pool := NewWorktreePool(tmpDir, filepath.Join(tmpDir, "worktrees"))

	setImpossibleFloor(t)
	target := filepath.Join(tmpDir, "worktrees", "wt-x")
	err := pool.CreateWorktree(context.Background(), "guard-branch", target)
	if err == nil || !strings.Contains(err.Error(), "disk_pressure") {
		t.Fatalf("expected disk_pressure refusal, got: %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatal("target must not exist after refusal")
	}
}

func TestCreateTaskWorktreeAllowedWithFloorsDisabled(t *testing.T) {
	// Floors are 0 via TestMain: creation must succeed and existing dirty
	// content must remain untouched by the guard (it never deletes).
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	pool := NewWorktreePool(tmpDir, filepath.Join(tmpDir, "worktrees"))

	wt, err := pool.CreateTaskWorktree(context.Background(), "FAC-998")
	if err != nil {
		t.Fatalf("creation with disabled floors failed: %v", err)
	}
	if wt.Path == "" || wt.Branch == "" {
		t.Fatalf("implausible worktree info: %+v", wt)
	}
}
