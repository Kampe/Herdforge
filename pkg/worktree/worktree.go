package worktree

import (
	"context"
	"fmt"
	"os/exec"
)

type WorktreeManager struct {
	RepoRoot string
}

func NewWorktreeManager(repoRoot string) *WorktreeManager {
	return &WorktreeManager{RepoRoot: repoRoot}
}

func (w *WorktreeManager) CreateWorktree(ctx context.Context, branch string, targetDir string) error {
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "-b", branch, targetDir, "HEAD")
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create worktree: %v, output: %s", err, string(output))
	}
	return nil
}

func (w *WorktreeManager) RemoveWorktree(ctx context.Context, targetDir string) error {
	cmd := exec.CommandContext(ctx, "git", "worktree", "remove", "--force", targetDir)
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to remove worktree: %v, output: %s", err, string(output))
	}
	return nil
}
