package gitroot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeTreeWriteFlag(t *testing.T) {
	want := strings.Join([]string{"", "", "write", "tree"}, "-")
	if MergeTreeWriteFlag != want {
		t.Fatalf("merge-tree write flag = %q, want %q", MergeTreeWriteFlag, want)
	}
	wantBase := strings.Join([]string{"", "", "merge", "base=HEAD"}, "-")
	if MergeTreeHeadBaseFlag != wantBase {
		t.Fatalf("merge-tree HEAD base flag = %q, want %q", MergeTreeHeadBaseFlag, wantBase)
	}
}

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

// TestLaneRootDoesNotHijackTheProjectRoot is the FAC-573 gate.
//
// HERD_ROOT was overloaded. The launch environment sets it to the LANE root, so
// a supervisor launched into its own worktree inherited it — and the mailbox and
// review-root resolvers, both reading HERD_ROOT as the PROJECT root, agreed with
// each other and were both wrong. The live supervisor resolved a lane-local
// mailbox, reported no pending handoffs, and five real ones sat unread.
//
// An explicit cd could not repair it: an inherited override outranks the working
// directory by design. Correct for a lane root, wrong for a project root — which
// is the tell that these were two values sharing one name.
func TestLaneRootDoesNotHijackTheProjectRoot(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	grGit(t, repo, "init", "-q", "-b", "main")
	grGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	lane := filepath.Join(base, "lane")
	grGit(t, repo, "worktree", "add", "-q", "-b", "feature", lane)

	if err := os.Unsetenv(EnvProjectRoot); err != nil {
		t.Fatal(err)
	}
	// Exactly the live condition: HERD_ROOT points at the lane.
	t.Setenv(EnvLaneRoot, lane)

	root, laneOverride, err := ProjectRoot(context.Background(), lane)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		wantRoot = repo
	}
	gotRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		gotRoot = root
	}
	if gotRoot != wantRoot {
		t.Fatalf("the lane override must not become the project root:\n  got:  %s\n  want: %s", gotRoot, wantRoot)
	}
	if laneOverride == "" {
		t.Error("a divergent lane override must be surfaced, not silently ignored")
	}
	// And resolving from the project itself gives the same answer, because a
	// control root must be worktree-invariant.
	fromRepo, _, err := ProjectRoot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := evalOrSelf(fromRepo), gotRoot; a != b {
		t.Errorf("the project root must be worktree-invariant: %s vs %s", a, b)
	}
}

// The dedicated variable is honoured, since a launcher or operator must be able
// to state the project root explicitly.
func TestExplicitProjectRootWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvProjectRoot, dir)
	t.Setenv(EnvLaneRoot, filepath.Join(dir, "lane"))
	root, laneOverride, err := ProjectRoot(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if evalOrSelf(root) != evalOrSelf(dir) {
		t.Errorf("%s must win, got %s", EnvProjectRoot, root)
	}
	if laneOverride == "" {
		t.Error("a lane root that disagrees must still be surfaced")
	}
}

// An agreeing HERD_ROOT is not a divergence and must not produce noise.
func TestAgreeingLaneRootIsNotReported(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	grGit(t, repo, "init", "-q", "-b", "main")
	grGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	if err := os.Unsetenv(EnvProjectRoot); err != nil {
		t.Fatal(err)
	}
	root, _, err := ProjectRoot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvLaneRoot, root)
	_, laneOverride, err := ProjectRoot(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if laneOverride != "" {
		t.Errorf("an agreeing lane root is not a divergence, got %q", laneOverride)
	}
}

func evalOrSelf(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}
