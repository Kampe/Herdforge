package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewWorktreeManager(t *testing.T) {
	repoRoot := "/tmp/test-repo"
	wm := NewWorktreeManager(repoRoot)
	if wm.RepoRoot != repoRoot {
		t.Errorf("expected RepoRoot %s, got %s", repoRoot, wm.RepoRoot)
	}
	expectedDir := filepath.Join(repoRoot, ".herd", "worktrees")
	if wm.WorktreeDir != expectedDir {
		t.Errorf("expected WorktreeDir %s, got %s", expectedDir, wm.WorktreeDir)
	}
}

func TestNewWorktreePool(t *testing.T) {
	wm := NewWorktreePool("/repo", "/custom/wt")
	if wm.RepoRoot != "/repo" || wm.WorktreeDir != "/custom/wt" {
		t.Errorf("unexpected pool fields: %+v", wm)
	}
}

func TestCreateWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wtDir := filepath.Join(tmpDir, "wt-test")
	defer os.RemoveAll(wtDir)

	err := wm.CreateWorktree(context.Background(), "test-branch", wtDir)
	if err != nil {
		t.Fatalf("expected worktree creation, got err: %v", err)
	}

	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		t.Fatal("expected worktree directory to exist")
	}
}

func TestRemoveWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wtDir := filepath.Join(tmpDir, "wt-to-remove")

	err := wm.CreateWorktree(context.Background(), "branch-to-remove", wtDir)
	if err != nil {
		t.Fatalf("create worktree: %v", err)
	}

	err = wm.RemoveWorktree(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("expected clean remove, got err: %v", err)
	}
}

func TestCreateTaskWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-42")
	if err != nil {
		t.Fatalf("expected task worktree creation, got err: %v", err)
	}

	expectedPath := filepath.Join(tmpDir, ".herd", "worktrees", "fac-42")
	if wi.Path != expectedPath {
		t.Errorf("expected path %s, got %s", expectedPath, wi.Path)
	}
	if wi.Branch != "herd/fac-42" {
		t.Errorf("expected branch herd/fac-42, got %s", wi.Branch)
	}
}

func TestPruneMergedWorktrees(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := NewWorktreeManager(tmpDir)
	// Historical global pruning is permanently fail-closed.
	if _, err := wm.PruneMergedWorktrees(context.Background(), "main"); err == nil {
		t.Fatal("expected global auto-reap refusal")
	}
}

// initRepo creates a minimal git repo in tmpDir for worktree operations.
// FAC-121: also publishes origin/main so CreateTaskWorktree can pin an
// immutable remote base rather than local HEAD.
func initRepo(t *testing.T, tmpDir string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		// Hermetic: disable signing so host 1Password/gpg hooks cannot break tests.
		{"git", "config", "commit.gpgsign", "false"},
		{"git", "config", "tag.gpgsign", "false"},
		{"git", "config", "gpg.format", "openpgp"},
	}
	for _, args := range cmds {
		if err := runCmd(tmpDir, args[0], args[1:]...); err != nil {
			t.Fatalf("git init setup failed: %v", err)
		}
	}
	// Create an initial commit so HEAD resolves
	if err := os.WriteFile(filepath.Join(tmpDir, "README.md"), []byte("# test"), 0644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	if err := runCmd(tmpDir, "git", "add", "README.md"); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if err := runCmd(tmpDir, "git", "commit", "-m", "initial"); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	_ = runCmd(tmpDir, "git", "branch", "-M", "main")
	setupOriginMain(t, tmpDir)
}

// setupOriginMain creates a bare origin remote and pushes main so
// origin/main is a resolvable immutable base for FAC-121 tests.
func setupOriginMain(t *testing.T, tmpDir string) {
	t.Helper()
	bare := filepath.Join(tmpDir, ".origin.git")
	if err := runCmd(tmpDir, "git", "init", "--bare", bare); err != nil {
		t.Fatalf("init bare origin: %v", err)
	}
	if err := runCmd(tmpDir, "git", "remote", "add", "origin", bare); err != nil {
		// remote may already exist when re-initing; force URL
		_ = runCmd(tmpDir, "git", "remote", "set-url", "origin", bare)
	}
	if err := runCmd(tmpDir, "git", "push", "-u", "origin", "main"); err != nil {
		t.Fatalf("push origin/main: %v", err)
	}
}

func runCmd(dir, name string, args ...string) error {
	c := execCommandContext(context.Background(), name, args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
