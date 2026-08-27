package harvest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHarvestSkipsNonWorktreeBeforeAnyScan drives the REAL HarvestReadOnly path
// (FAC-604). A stale directory without .git must appear in Skipped and must not
// receive a per-path rev-parse. Revert proof: if harvest() calls soft
// ClassifyHarvestInput instead of Strict, this test goes red (dead path is
// scanned, not skipped).
func TestHarvestSkipsNonWorktreeBeforeAnyScan(t *testing.T) {
	root := t.TempDir()
	capacityGit(t, root, "init", "-q", "-b", "main")
	capacityGit(t, root, "config", "user.email", "fac604@test")
	capacityGit(t, root, "config", "user.name", "fac604")
	capacityGit(t, root, "commit", "--allow-empty", "-q", "-m", "base")

	live := filepath.Join(root, "live-wt")
	capacityGit(t, root, "worktree", "add", "-q", "-b", "live", live)

	dead := filepath.Join(root, "stale-no-git")
	if err := os.MkdirAll(dead, 0o755); err != nil {
		t.Fatal(err)
	}

	old := execCommandContext
	t.Cleanup(func() { execCommandContext = old })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "list" {
			out := "worktree " + root + "\nworktree " + live + "\nworktree " + dead + "\n"
			return exec.CommandContext(ctx, "printf", "%s", out)
		}
		// Inventory of live tips only: dead must already have been skipped.
		if len(args) >= 1 && args[0] == "rev-parse" {
			return exec.CommandContext(ctx, "printf", "%s", "live\n")
		}
		if len(args) >= 1 && (args[0] == "cherry" || args[0] == "fetch" || args[0] == "log") {
			return exec.CommandContext(ctx, "printf", "%s", "")
		}
		return old(ctx, name, args...)
	}

	h := NewHarvester(root)
	result, err := h.HarvestReadOnly(context.Background())
	if err != nil {
		t.Fatalf("HarvestReadOnly: %v", err)
	}
	found := false
	for _, skip := range result.Skipped {
		if skip.Path == dead {
			found = true
			if !strings.Contains(skip.Reason, "not a git worktree") {
				t.Fatalf("skip reason=%q want not a git worktree", skip.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("dead non-worktree must be in Skipped before scan; skipped=%+v errors=%v", result.Skipped, result.Errors)
	}
	for _, u := range result.UnmergedWorktrees {
		if u.WorktreePath == dead {
			t.Fatal("dead path must not appear in UnmergedWorktrees")
		}
	}
}

// TestHarvestScratchExcludedThroughHarvest pins scratch exclusion on the
// operator path, not only ClassifyHarvestInput directly.
func TestHarvestScratchExcludedThroughHarvest(t *testing.T) {
	root := t.TempDir()
	capacityGit(t, root, "init", "-q", "-b", "main")
	capacityGit(t, root, "config", "user.email", "fac604@test")
	capacityGit(t, root, "config", "user.name", "fac604")
	capacityGit(t, root, "commit", "--allow-empty", "-q", "-m", "base")

	scratch := filepath.Join(root, "scratchpad", "mcp-nonvacuity-check")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}

	old := execCommandContext
	t.Cleanup(func() { execCommandContext = old })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) >= 2 && args[0] == "worktree" && args[1] == "list" {
			out := "worktree " + root + "\nworktree " + scratch + "\n"
			return exec.CommandContext(ctx, "printf", "%s", out)
		}
		if len(args) >= 1 && args[0] == "rev-parse" {
			return exec.CommandContext(ctx, "printf", "%s", "main\n")
		}
		if len(args) >= 1 && (args[0] == "cherry" || args[0] == "fetch" || args[0] == "log") {
			return exec.CommandContext(ctx, "printf", "%s", "")
		}
		return old(ctx, name, args...)
	}

	h := NewHarvester(root)
	result, err := h.HarvestReadOnly(context.Background())
	if err != nil {
		t.Fatalf("HarvestReadOnly: %v", err)
	}
	found := false
	for _, skip := range result.Skipped {
		if skip.Path == scratch {
			found = true
			if !strings.Contains(skip.Reason, "scratch") {
				t.Fatalf("reason=%q", skip.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("scratch must be skipped via HarvestReadOnly; skipped=%+v", result.Skipped)
	}
}
