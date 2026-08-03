package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestWorktreePool_CreateAndList uses a temp-origin fixture (FAC-152):
// pointing a WorktreeManager at "." would run "git worktree list" with
// cmd.Dir="." — the developer's live repository — and is exactly the
// anti-pattern that let a prior test register a real worktree at
// pkg/dispatch/.herd/worktrees/fac-1. Every worktree op here must run
// against tmpDir so `git worktree list` in the live repo stays unchanged.
func TestWorktreePool_CreateAndList(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wtDir := filepath.Join(tmpDir, "worktrees")

	pool := NewWorktreePool(tmpDir, wtDir)
	if pool.RepoRoot != tmpDir || pool.WorktreeDir != wtDir {
		t.Errorf("unexpected pool fields: %+v", pool)
	}

	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("failed to create temp worktree dir: %v", err)
	}

	ctx := context.Background()
	wtList, err := pool.ListWorktrees(ctx)
	if err != nil {
		t.Fatalf("expected clean list worktrees execution, got err: %v", err)
	}

	if len(wtList) == 0 {
		t.Errorf("expected at least main checkout worktree, got 0")
	}
	for _, wt := range wtList {
		if !isContainedIn(wt.Path, tmpDir) {
			t.Fatalf("ListWorktrees leaked a path outside the temp fixture: %+v", wt)
		}
	}
}
