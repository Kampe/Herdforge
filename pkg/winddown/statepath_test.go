package winddown

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// gitRepoWithLinkedWorktree builds a repository plus one linked worktree and
// returns the symlink-resolved root and the linked worktree path. Every lane in
// this fleet runs from a linked worktree, so that is the only shape this rule
// has to be correct in.
func gitRepoWithLinkedWorktree(t *testing.T) (root, linked string) {
	t.Helper()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=winddown-test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=winddown-test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	root = t.TempDir()
	run(root, "init", "-q", "-b", "main")
	run(root, "commit", "--allow-empty", "-qm", "base")
	linked = filepath.Join(t.TempDir(), "lane")
	run(root, "worktree", "add", "-q", "-b", "lane", linked)
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved, linked
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// A cwd-relative default silently resolves a lane-local path that cannot exist:
// .herd/winddown.json is gitignored, so it lives exactly once, at the project
// root. Every lane-launched caller then fails closed against a file the
// coordinator did in fact provision.
func TestDefaultStatePathResolvesProjectRootFromLinkedWorktree(t *testing.T) {
	t.Setenv(envStatePath, "")
	t.Setenv(gitroot.EnvProjectRoot, "")
	root, linked := gitRepoWithLinkedWorktree(t)
	chdir(t, linked)

	want := filepath.Join(root, relativeStatePath)
	if got := DefaultStatePath(); got != want {
		t.Fatalf("DefaultStatePath from linked worktree = %q, want %q", got, want)
	}
}

func TestDefaultStatePathHonorsExplicitOverride(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "elsewhere.json")
	t.Setenv(envStatePath, explicit)
	_, linked := gitRepoWithLinkedWorktree(t)
	chdir(t, linked)

	if got := DefaultStatePath(); got != explicit {
		t.Fatalf("DefaultStatePath = %q, want the explicit override %q", got, explicit)
	}
}

// Outside a repository there is no project root to resolve, so the historical
// relative path stands rather than the call failing.
func TestDefaultStatePathFallsBackOutsideRepository(t *testing.T) {
	t.Setenv(envStatePath, "")
	t.Setenv(gitroot.EnvProjectRoot, "")
	chdir(t, t.TempDir())

	if got := DefaultStatePath(); got != relativeStatePath {
		t.Fatalf("DefaultStatePath outside a repository = %q, want %q", got, relativeStatePath)
	}
}

// The consumer that found this: the feedback census admission gate passes an
// empty path, so RequireAdmission must reach the root's state from a lane.
func TestRequireAdmissionFromLinkedWorktreeReadsProjectRootState(t *testing.T) {
	t.Setenv(envStatePath, "")
	t.Setenv(gitroot.EnvProjectRoot, "")
	root, linked := gitRepoWithLinkedWorktree(t)
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o700); err != nil {
		t.Fatal(err)
	}
	state := []byte(`{"enabled":false,"actor":"coordinator","reason":"initialized","timestamp":"2026-09-02T00:00:00Z","generation":1}`)
	if err := os.WriteFile(filepath.Join(root, relativeStatePath), state, 0o600); err != nil {
		t.Fatal(err)
	}
	chdir(t, linked)

	if err := RequireAdmission(context.Background(), ""); err != nil {
		t.Fatalf("RequireAdmission from linked worktree = %v, want admission against the project root's state", err)
	}
}
