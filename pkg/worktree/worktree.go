package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// execCommandContext is a variable so tests can mock; defaults to exec.CommandContext
var execCommandContext = exec.CommandContext

type WorktreeManager struct {
	RepoRoot    string
	WorktreeDir string
}

func NewWorktreeManager(repoRoot string) *WorktreeManager {
	return &WorktreeManager{
		RepoRoot:    repoRoot,
		WorktreeDir: filepath.Join(repoRoot, ".herd", "worktrees"),
	}
}

func NewWorktreePool(repoRoot string, worktreeDir string) *WorktreeManager {
	return &WorktreeManager{
		RepoRoot:    repoRoot,
		WorktreeDir: worktreeDir,
	}
}

type WorktreeInfo struct {
	Path   string
	Branch string
	Commit string
}

func (w *WorktreeManager) CreateWorktree(ctx context.Context, branch string, targetDir string) error {
	cmd := execCommandContext(ctx, "git", "worktree", "add", "-b", branch, targetDir, "HEAD")
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create worktree: %v, output: %s", err, string(output))
	}
	return nil
}

func (w *WorktreeManager) RemoveWorktree(ctx context.Context, targetDir string) error {
	cmd := execCommandContext(ctx, "git", "worktree", "remove", "--force", targetDir)
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to remove worktree: %v, output: %s", err, string(output))
	}
	return nil
}

// ListWorktrees runs git worktree list and returns structured worktree information
func (w *WorktreeManager) ListWorktrees(ctx context.Context) ([]*WorktreeInfo, error) {
	cmd := execCommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = w.RepoRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w, output: %s", err, string(output))
	}

	var res []*WorktreeInfo
	lines := strings.Split(string(output), "\n")
	var current *WorktreeInfo

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if current != nil {
				res = append(res, current)
				current = nil
			}
			continue
		}

		if strings.HasPrefix(line, "worktree ") {
			current = &WorktreeInfo{Path: strings.TrimPrefix(line, "worktree ")}
		} else if strings.HasPrefix(line, "HEAD ") && current != nil {
			current.Commit = strings.TrimPrefix(line, "HEAD ")
		} else if strings.HasPrefix(line, "branch ") && current != nil {
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}

	if current != nil {
		res = append(res, current)
	}

	return res, nil
}

// CreateTaskWorktree spins up an isolated ephemeral worktree for a task ref
func (w *WorktreeManager) CreateTaskWorktree(ctx context.Context, taskRef string) (*WorktreeInfo, error) {
	branch := fmt.Sprintf("herd/%s", strings.ToLower(taskRef))
	targetPath := filepath.Join(w.WorktreeDir, strings.ToLower(taskRef))

	if err := os.MkdirAll(w.WorktreeDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create worktree root directory: %w", err)
	}

	cmd := execCommandContext(ctx, "git", "worktree", "add", "-b", branch, targetPath, "HEAD")
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("failed to create git worktree: %v, output: %s", err, string(output))
	}

	return &WorktreeInfo{
		Path:   targetPath,
		Branch: branch,
	}, nil
}

// PruneMergedWorktrees automatically removes worktree paths whose branches have landed on default branch
func (w *WorktreeManager) PruneMergedWorktrees(ctx context.Context, defaultBranch string) (int, error) {
	wtList, err := w.ListWorktrees(ctx)
	if err != nil {
		return 0, err
	}

	prunedCount := 0
	for _, wt := range wtList {
		if strings.HasPrefix(wt.Branch, "herd/") {
			cmd := execCommandContext(ctx, "git", "branch", "--merged", defaultBranch)
			cmd.Dir = w.RepoRoot
			output, err := cmd.CombinedOutput()
			if err == nil && strings.Contains(string(output), wt.Branch) {
				rmCmd := execCommandContext(ctx, "git", "worktree", "remove", "--force", wt.Path)
				rmCmd.Dir = w.RepoRoot
				if err := rmCmd.Run(); err == nil {
					prunedCount++
				}
			}
		}
	}

	return prunedCount, nil
}
