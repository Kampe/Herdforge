package harvest

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestUnmergedFor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "config", "user.email", "t@h.local")
	gitIn(t, root, "config", "user.name", "t")
	gitIn(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	gitIn(t, root, "update-ref", "refs/remotes/origin/main", gitIn(t, root, "rev-parse", "HEAD"))

	wt := root + "/wt-lane"
	gitIn(t, root, "worktree", "add", "-q", "-b", "lane", wt)
	gitIn(t, wt, "commit", "--allow-empty", "-q", "-m", "feat: unique work")

	h := NewHarvester(root)

	t.Run("branch with unique commit reports it", func(t *testing.T) {
		u, err := h.UnmergedFor(ctx, wt)
		if err != nil || u == nil {
			t.Fatalf("want unmerged work, got %+v err %v", u, err)
		}
		if u.Branch != "lane" || len(u.Unmerged) != 1 {
			t.Fatalf("unexpected result %+v", u)
		}
	})

	t.Run("patch-equivalent upstream means clean", func(t *testing.T) {
		// Rebase-merge simulation: move origin/main to carry the same patch.
		gitIn(t, root, "update-ref", "refs/remotes/origin/main", gitIn(t, wt, "rev-parse", "HEAD"))
		u, err := h.UnmergedFor(ctx, wt)
		if err != nil || u != nil {
			t.Fatalf("merged branch must be clean, got %+v err %v", u, err)
		}
	})

	t.Run("main-branch worktree is never paneled", func(t *testing.T) {
		u, err := h.UnmergedFor(ctx, root)
		if err != nil || u != nil {
			t.Fatalf("main checkout must be nil,nil got %+v err %v", u, err)
		}
	})

	t.Run("non-worktree errors", func(t *testing.T) {
		if _, err := h.UnmergedFor(ctx, t.TempDir()); err == nil {
			t.Fatal("non-worktree must error")
		}
	})
}
