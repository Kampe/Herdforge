package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManagedPath returns the state-root location for one owner checkout. It is
// deliberately derived from the record identity, never from a branch name.
func (w *WorktreeManager) ManagedPath(id string) (string, error) {
	if w == nil || strings.TrimSpace(w.StateRoot) == "" {
		return "", fmt.Errorf("managed state root is required")
	}
	id = safeLifecyclePart(id)
	if id == "" {
		return "", fmt.Errorf("managed worktree id is required")
	}
	return filepath.Join(w.StateRoot, safeLifecyclePart(w.Kind), id), nil
}

// CreateManagedDetached creates an inert checkout and publishes its ownership
// record before returning it to the caller. A record failure removes the new
// checkout, so no successful call can leave an unowned worktree.
func (w *WorktreeManager) CreateManagedDetached(ctx context.Context, id, revision string) (string, error) {
	path, err := w.ManagedPath(id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(revision) == "" {
		return "", fmt.Errorf("managed detached worktree revision is required")
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("managed worktree %q already exists", id)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat managed worktree: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create managed worktree parent: %w", err)
	}
	cmd := execCommandContext(ctx, "git", "worktree", "add", "--detach", path, revision)
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("create managed detached worktree: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if err := w.recordActiveWorktree(id, revision, path); err != nil {
		_ = w.removeManagedBestEffort(ctx, path)
		return "", err
	}
	return path, nil
}

// CreateManagedBranch creates a branch-attached checkout under the state root
// and records it as one atomic lifecycle operation.
func (w *WorktreeManager) CreateManagedBranch(ctx context.Context, id, branch, base string) (string, error) {
	path, err := w.ManagedPath(id)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(branch) == "" || strings.TrimSpace(base) == "" {
		return "", fmt.Errorf("managed branch worktree requires branch and base")
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("managed worktree %q already exists", id)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat managed worktree: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create managed worktree parent: %w", err)
	}
	cmd := execCommandContext(ctx, "git", "worktree", "add", "-b", branch, path, base)
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("create managed branch worktree: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if err := w.recordActiveWorktree(id, base, path); err != nil {
		_ = w.removeManagedBestEffort(ctx, path)
		return "", err
	}
	return path, nil
}

// RetireManaged removes an owned, clean checkout and marks the exact record
// retired. It never uses --force: in-band callers must archive dirty work via
// expiry rather than discarding it.
func (w *WorktreeManager) RetireManaged(ctx context.Context, id string) error {
	path, err := w.ManagedPath(id)
	if err != nil {
		return err
	}
	if err := w.RemoveWorktreeSafely(ctx, path); err != nil {
		return err
	}
	return w.recordRetiredWorktreeID(id, path)
}

// RegisterManaged records an externally-created checkout only when it is at
// the exact path derived from its lifecycle id. It supports mature creators
// whose Git invocation carries additional safety flags while still preventing
// unowned state-root checkouts.
func (w *WorktreeManager) RegisterManaged(id, base, path string) error {
	want, err := w.ManagedPath(id)
	if err != nil {
		return err
	}
	if filepath.Clean(want) != filepath.Clean(path) {
		return fmt.Errorf("managed worktree path does not match lifecycle id")
	}
	return w.recordActiveWorktree(id, base, path)
}

// MarkManagedRetired updates an exact lifecycle record after a creator's own
// successful removal operation.
func (w *WorktreeManager) MarkManagedRetired(id string) error {
	path, err := w.ManagedPath(id)
	if err != nil {
		return err
	}
	return w.recordRetiredWorktreeID(id, path)
}

func (w *WorktreeManager) removeManagedBestEffort(ctx context.Context, path string) error {
	cmd := execCommandContext(ctx, "git", "worktree", "remove", "--force", path)
	cmd.Dir = w.RepoRoot
	return cmd.Run()
}
