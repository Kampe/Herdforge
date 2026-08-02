package worktree

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// TestCreateTaskWorktree_AnchorCommit pins the FAC-106 reap-safety
// guarantee: a freshly created lane branch carries at least one commit, so
// `git worktree remove` (or a git clean of the gitignored worktree dir) can
// never destroy work that was never committable — the branch is always
// restorable with `git worktree add`.
func TestCreateTaskWorktree_AnchorCommit(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	runCmd(tmpDir, "git", "branch", "-m", "main")

	wm := NewWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-999")
	if err != nil {
		t.Fatalf("create task worktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	subj := gitOut(t, tmpDir, "log", "-1", "--format=%s", "herd/fac-999")
	if !strings.Contains(subj, "anchor") || !strings.Contains(subj, "FAC-999") {
		t.Fatalf("branch tip is not the anchor commit: %q", subj)
	}

	// Simulate the reap: remove the working directory (as git clean -fdx of a
	// gitignored worktree path would). The branch and its commit must survive.
	os.RemoveAll(wi.Path)
	if gitOut(t, tmpDir, "rev-parse", "--verify", "herd/fac-999") == "" {
		t.Fatal("branch did not survive working-dir removal")
	}
}
