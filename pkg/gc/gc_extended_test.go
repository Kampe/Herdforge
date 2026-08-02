package gc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

func TestScanOverlap_EmptyRepo(t *testing.T) {
	tmpDir := t.TempDir()
	initGitRepo(t, tmpDir)
	wm := worktree.NewWorktreeManager(tmpDir)
	gcm := NewGCManager(tmpDir, wm)

	report, err := gcm.ScanOverlap(context.Background(), 1)
	if err != nil {
		t.Fatalf("expected clean scan, got err: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil report")
	}
}

func TestScanOverlap_NonGitDir(t *testing.T) {
	tmpDir := t.TempDir()
	wm := worktree.NewWorktreeManager(tmpDir)
	gcm := NewGCManager(tmpDir, wm)

	_, err := gcm.ScanOverlap(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error for non-git directory")
	}
}

func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0644)
	cmd := exec.Command("git", "add", "README.md")
	cmd.Dir = dir
	cmd.Run()
	cmd = exec.Command("git", "commit", "-m", "initial")
	cmd.Dir = dir
	cmd.Run()
}
