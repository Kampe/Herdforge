package resources

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// Repository-wide path contracts used by resource census, dispatch receipts,
// and operator commands. Their literal definition belongs in one place.
const (
	ReviewPoolPathFragment      = "/.herd/pool/"
	ManagedWorktreePathFragment = "/.herd/worktrees/"
	LegacyWorktreePathFragment  = "/.worktrees/"
	TaskContextFile             = "TASK-CONTEXT.json"
)

// GitCommitIsAncestor reports Git reachability while preserving the
// distinction between a proven negative (exit 1) and an unanswerable query.
func GitCommitIsAncestor(ctx context.Context, root, sha, ref string) (bool, error) {
	sha = strings.TrimSpace(sha)
	ref = strings.TrimSpace(ref)
	if sha == "" || ref == "" {
		return false, errors.New("Git ancestry requires a commit and ref")
	}
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", sha, ref)
	if strings.TrimSpace(root) != "" {
		cmd.Dir = root
	}
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
