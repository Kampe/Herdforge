package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExitError simulates exec.ExitError so CombinedOutput returns a non-nil error.
type fakeExitError struct{ msg string }

func (e *fakeExitError) Error() string { return e.msg }
func (e *fakeExitError) ExitCode() int { return 1 }

func TestCreateWorktree_Error_RepoRootNotGit(t *testing.T) {
	tmpDir := t.TempDir()
	wm := NewWorktreeManager(tmpDir)
	// No git init — git commands will fail
	err := wm.CreateWorktree(context.Background(), "test-branch", filepath.Join(tmpDir, "wt"))
	if err == nil {
		t.Fatal("expected error from non-repo dir")
	}
	if !strings.Contains(err.Error(), "failed to create worktree") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRemoveWorktree_Error_NonExistent(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)
	err := wm.RemoveWorktree(context.Background(), filepath.Join(tmpDir, "nonexistent"))
	if err == nil {
		t.Fatal("expected error removing non-existent worktree")
	}
	if !strings.Contains(err.Error(), "failed to remove worktree") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRemoveWorktree_Error_GitFailure(t *testing.T) {
	wm := NewWorktreeManager("/nonexistent/path")
	// Restore original after test
	defer func(old func(context.Context, string, ...string) *exec.Cmd) {
		execCommandContext = old
	}(execCommandContext)

	err := wm.RemoveWorktree(context.Background(), "/tmp/wt")
	if err == nil {
		t.Fatal("expected error when git fails")
	}
	if !strings.Contains(err.Error(), "failed to remove worktree") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestListWorktrees_Error_GitFailure(t *testing.T) {
	tmpDir := t.TempDir()
	wm := NewWorktreeManager(tmpDir)
	_, err := wm.ListWorktrees(context.Background())
	if err == nil {
		t.Fatal("expected error from non-repo dir")
	}
	if !strings.Contains(err.Error(), "failed to list worktrees") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestListWorktrees_Parsing_MalformedOutput(t *testing.T) {
	// We need to inject a fake execCommandContext that returns known output
	// so we can exercise edge cases in the parser.
	defer func(old func(context.Context, string, ...string) *exec.Cmd) {
		execCommandContext = old
	}(execCommandContext)

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Use a real exec.Cmd with a fake binary that echoes our test data
		cmd := exec.CommandContext(ctx, "echo", "worktree /path/a\nHEAD abc123\nbranch refs/heads/herd/task-1\n\nworktree /path/b\nHEAD def456\n")
		return cmd
	}

	wm := NewWorktreeManager(t.TempDir())
	wts, err := wm.ListWorktrees(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wts) != 2 {
		t.Fatalf("expected 2 worktrees, got %d", len(wts))
	}
	if wts[0].Path != "/path/a" || wts[0].Commit != "abc123" || wts[0].Branch != "herd/task-1" {
		t.Errorf("unexpected worktree[0]: %+v", wts[0])
	}
	if wts[1].Path != "/path/b" || wts[1].Commit != "def456" || wts[1].Branch != "" {
		t.Errorf("unexpected worktree[1]: %+v", wts[1])
	}
}

func TestListWorktrees_Parsing_NoTrailingNewline(t *testing.T) {
	defer func(old func(context.Context, string, ...string) *exec.Cmd) {
		execCommandContext = old
	}(execCommandContext)

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "echo", "-n", "worktree /path/c\nHEAD abc789\nbranch refs/heads/main")
		return cmd
	}

	wm := NewWorktreeManager(t.TempDir())
	wts, err := wm.ListWorktrees(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(wts) != 1 {
		t.Fatalf("expected 1 worktree, got %d", len(wts))
	}
	if wts[0].Path != "/path/c" || wts[0].Commit != "abc789" || wts[0].Branch != "main" {
		t.Errorf("unexpected worktree: %+v", wts[0])
	}
}

func TestCreateTaskWorktree_Error_NonRepo(t *testing.T) {
	tmpDir := t.TempDir()
	wm := NewWorktreeManager(tmpDir)
	_, err := wm.CreateTaskWorktree(context.Background(), "TASK-1")
	if err == nil {
		t.Fatal("expected error from non-repo dir")
	}
	// FAC-121: failure may surface at immutable-base resolution before worktree add.
	if !strings.Contains(err.Error(), "failed to create git worktree") &&
		!strings.Contains(err.Error(), "immutable base") &&
		!strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCreateTaskWorktree_Error_DiskCapacityTargetNotDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	wm := NewWorktreeManager(tmpDir)
	herdDir := filepath.Join(tmpDir, ".herd")
	if err := os.MkdirAll(herdDir, 0755); err != nil {
		t.Fatalf("mkdir .herd: %v", err)
	}
	if err := os.WriteFile(wm.WorktreeDir, []byte("not a dir"), 0644); err != nil {
		t.Fatalf("write file for worktreeDir: %v", err)
	}
	// Initialize the repository before taking the post-admission mutation baseline.
	initRepo(t, tmpDir)

	gitCalls := 0
	oldExec := execCommandContext
	defer func() { execCommandContext = oldExec }()
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gitCalls++
		return exec.CommandContext(ctx, "false")
	}

	_, err := wm.CreateTaskWorktree(context.Background(), "TASK-1")
	if err == nil {
		t.Fatal("expected disk capacity denial when worktree dir is a file")
	}
	errText := err.Error()
	if !strings.Contains(errText, "disk capacity gate: resolve target volume") ||
		!strings.Contains(errText, "not a directory") {
		t.Fatalf("unexpected error: %v", err)
	}
	if gitCalls != 0 {
		t.Fatalf("git callback count = %d, want zero after admission denial", gitCalls)
	}
	if info, statErr := os.Stat(wm.WorktreeDir); statErr != nil || info.IsDir() {
		t.Fatalf("worktree root fixture changed after admission denial: info=%v err=%v", info, statErr)
	}
	if _, statErr := os.Stat(filepath.Join(wm.WorktreeDir, "task-1")); statErr == nil {
		t.Fatal("task worktree path was mutated after admission denial")
	}
	anchorCheck := exec.Command("git", "show-ref", "--verify", "--quiet", AnchorRefFor("TASK-1"))
	anchorCheck.Dir = tmpDir
	if anchorCheck.Run() == nil {
		t.Fatal("durable anchor was created after admission denial")
	}
}

func TestPruneMergedWorktrees_Error_ListWorktrees(t *testing.T) {
	tmpDir := t.TempDir()
	wm := NewWorktreeManager(tmpDir)
	_, err := wm.PruneMergedWorktrees(context.Background(), "main")
	if err == nil {
		t.Fatal("expected error when ListWorktrees fails")
	}
}

func TestPruneMergedWorktrees_WithHerbBranchMerged(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	runCmd(tmpDir, "git", "branch", "-m", "main")

	wm := NewWorktreeManager(tmpDir)
	// Use CreateTaskWorktree which creates herd/<ref> branch and worktree
	wi, err := wm.CreateTaskWorktree(context.Background(), "FTR-42")
	if err != nil {
		t.Fatalf("create task worktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Merge into main and publish to origin so git cherry sees content-merged.
	runCmd(tmpDir, "git", "checkout", "main")
	runCmd(tmpDir, "git", "merge", "--no-ff", "herd/ftr-42")
	runCmd(tmpDir, "git", "push", "origin", "main")

	// The historical wrapper cannot prove exact target and lease evidence.
	if _, err := wm.PruneMergedWorktrees(context.Background(), "main"); err == nil {
		t.Fatal("expected global auto-reap refusal")
	}
	if _, err := os.Stat(filepath.Join(wi.Path, ".git")); err != nil {
		t.Fatalf("fail-closed wrapper removed worktree: %v", err)
	}
	_ = wi
}

func TestPruneMergedWorktrees_RemoveFailureNotCounted(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	runCmd(tmpDir, "git", "branch", "-m", "main")

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "STALE-99")
	if err != nil {
		t.Fatalf("create task worktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Content-merge onto origin/main so classification is eligible.
	runCmd(tmpDir, "git", "checkout", "main")
	runCmd(tmpDir, "git", "merge", "--no-ff", "herd/stale-99")
	runCmd(tmpDir, "git", "push", "origin", "main")

	// Mock git worktree remove to fail (FAC-117: do not count as reaped).
	defer func(old func(context.Context, string, ...string) *exec.Cmd) {
		execCommandContext = old
	}(execCommandContext)

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Match both `git worktree remove` and remove via RemoveWorktree.
		if name == "git" {
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "worktree" && args[i+1] == "remove" {
					return exec.CommandContext(ctx, "false")
				}
			}
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = tmpDir
		return cmd
	}

	if _, err := wm.PruneMergedWorktrees(context.Background(), "main"); err == nil {
		t.Fatal("expected global auto-reap refusal before remove seam")
	}

	// Clean up the worktree manually since prune's remove was mocked away
	// Restore real exec for cleanup.
	execCommandContext = exec.CommandContext
	runCmd(tmpDir, "git", "worktree", "remove", "--force", wi.Path)
}

func TestCreateWorktree_Error_InvalidBranch(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)
	// A branch with a space should cause git to fail
	err := wm.CreateWorktree(context.Background(), "invalid branch name", filepath.Join(tmpDir, "wt"))
	if err == nil {
		t.Fatal("expected error for invalid branch name")
	}
	if !strings.Contains(err.Error(), "failed to create worktree") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestRemoveWorktree_Error_WithDirtyWorktree(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := NewWorktreeManager(tmpDir)
	wtDir := filepath.Join(tmpDir, "wt-dirty")

	// Create worktree
	if err := wm.CreateWorktree(context.Background(), "branch-dirty", wtDir); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wtDir) })

	// Write a dirty file in the worktree
	if err := os.WriteFile(filepath.Join(wtDir, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	// Remove with --force should still succeed (the code uses --force)
	err := wm.RemoveWorktree(context.Background(), wtDir)
	if err != nil {
		t.Fatalf("expected no error with --force, got: %v", err)
	}
}

// TestCreateWorktree_MockedGitError uses execCommandContext mock to force a git error
func TestCreateWorktree_MockedGitError(t *testing.T) {
	defer func(old func(context.Context, string, ...string) *exec.Cmd) {
		execCommandContext = old
	}(execCommandContext)

	mockErr := errors.New("git exploded")

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// Return a real Cmd that will fail because the binary doesn't produce valid output
		// but we can't make exec.CommandContext itself return a non-nil error.
		// Instead, set the command to something that fails immediately.
		cmd := exec.CommandContext(ctx, "false")
		cmd.Dir = "/tmp"
		return cmd
	}

	wm := NewWorktreeManager("/tmp")
	err := wm.CreateWorktree(context.Background(), "b", "/tmp/wt")
	if err == nil {
		t.Fatal("expected error from mocked git command")
	}
	_ = mockErr
}

func TestCreateTaskWorktree_MockedGitError(t *testing.T) {
	defer func(old func(context.Context, string, ...string) *exec.Cmd) {
		execCommandContext = old
	}(execCommandContext)

	tmpDir := t.TempDir()
	wm := NewWorktreeManager(tmpDir)
	if err := os.MkdirAll(wm.WorktreeDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "false")
		return cmd
	}

	_, err := wm.CreateTaskWorktree(context.Background(), "TASK-X")
	if err == nil {
		t.Fatal("expected error from mocked git command in CreateTaskWorktree")
	}
	// FAC-121 resolves origin base first; mocked git fails there before worktree add.
	if !strings.Contains(err.Error(), "failed to create git worktree") &&
		!strings.Contains(err.Error(), "immutable base") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestListWorktrees_MockedError(t *testing.T) {
	defer func(old func(context.Context, string, ...string) *exec.Cmd) {
		execCommandContext = old
	}(execCommandContext)

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "false")
		return cmd
	}

	wm := NewWorktreeManager(t.TempDir())
	_, err := wm.ListWorktrees(context.Background())
	if err == nil {
		t.Fatal("expected error from mocked git command in ListWorktrees")
	}
	if !strings.Contains(err.Error(), "failed to list worktrees") {
		t.Errorf("unexpected error message: %v", err)
	}
}
