package gitroot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func grGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestCommonDirIsSharedAcrossWorktrees is the property that makes this the one
// definition worth having: every worktree of a repository must agree.
//
// FAC-565: this invocation existed in twelve places, two of them without
// --path-format=absolute and hand-rolling the absolutization afterwards. That is
// the same rule twelve times, and disagreement about "where is this repository"
// is what produced the handoff-mailbox and review-root divergences.
func TestCommonDirIsSharedAcrossWorktrees(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	grGit(t, repo, "init", "-q", "-b", "main")
	grGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	wt := filepath.Join(base, "wt")
	grGit(t, repo, "worktree", "add", "-q", "-b", "feature", wt)

	fromRepo, err := CommonDir(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	fromWorktree, err := CommonDir(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	if fromRepo != fromWorktree {
		t.Fatalf("every worktree must resolve one common dir:\n  repo: %s\n  wt:   %s", fromRepo, fromWorktree)
	}
	if !filepath.IsAbs(fromRepo) {
		t.Errorf("the common dir must always be absolute, got %q", fromRepo)
	}
}

// Toplevel is per-worktree by definition, which is exactly why it must not be
// confused with the common dir. Pinning both keeps a caller from reaching for
// the wrong one.
func TestToplevelIsPerWorktree(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	grGit(t, repo, "init", "-q", "-b", "main")
	grGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	wt := filepath.Join(base, "wt")
	grGit(t, repo, "worktree", "add", "-q", "-b", "feature", wt)

	topRepo, err := Toplevel(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	topWorktree, err := Toplevel(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	if topRepo == topWorktree {
		t.Fatal("a worktree has its own toplevel; if these match, the fixture is wrong")
	}
	for _, p := range []string{topRepo, topWorktree} {
		if !filepath.IsAbs(p) {
			t.Errorf("toplevel must be absolute, got %q", p)
		}
	}
}

// Outside a repository both must fail closed. Returning an empty path would let
// a caller join it against a cwd and silently address the wrong tree.
func TestOutsideARepositoryFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if _, err := CommonDir(context.Background(), dir); err == nil {
		t.Error("a non-repository must not yield a common dir")
	}
	if _, err := Toplevel(context.Background(), dir); err == nil {
		t.Error("a non-repository must not yield a toplevel")
	}
}
