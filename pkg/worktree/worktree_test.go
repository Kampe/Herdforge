package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWorktreePool_CreateAndList(t *testing.T) {
	tmpDir := t.TempDir()
	wtDir := filepath.Join(tmpDir, "worktrees")

	pool := NewWorktreePool(".", wtDir)
	if pool.RepoRoot != "." || pool.WorktreeDir != wtDir {
		t.Errorf("unexpected pool fields: %+v", pool)
	}

	// Verify directory creation logic
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
}
