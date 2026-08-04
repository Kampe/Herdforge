package gc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
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
		cmd := testgit.Command(dir, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v, %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0644)
	cmd := testgit.Command(dir, "add", "README.md")
	cmd.Run()
	cmd = testgit.Command(dir, "commit", "-m", "initial")
	cmd.Run()
}
