package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func cutRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := func(dir string, a ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, a...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	run(root, "init", "-q", "-b", "main", ".")
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(root, "other.md"), []byte("base\n"), 0o644)
	run(root, "add", ".")
	run(root, "commit", "-qm", "base")
	run(root, "branch", "origin/main")

	// A long-lived lane branch: touches docs AND an unrelated file.
	run(root, "checkout", "-q", "-b", "standing/lane")
	os.WriteFile(filepath.Join(root, "docs", "a.md"), []byte("base\nlane work\n"), 0o644)
	os.WriteFile(filepath.Join(root, "other.md"), []byte("base\nunrelated\n"), 0o644)
	run(root, "add", ".")
	run(root, "commit", "-qm", "lane work")
	run(root, "checkout", "-q", "main")
	return root
}

func runCut(t *testing.T, root string, args ...string) error {
	t.Helper()
	t.Setenv("HERD_ROOT", root)
	return runLaneCut(args)
}

// FAC-655: a standing lane had nowhere to put work except a long-lived branch,
// and roughly 1,100 commits were stranded on branches hundreds behind main --
// unmergeable because rebasing that history is a conflict marathon and the work
// is not scoped to any one task.
func TestLaneCutExtractsOnlyTheScopedWork(t *testing.T) {
	root := cutRepo(t)
	if err := runCut(t, root, "--branch", "standing/lane", "--scope", "docs", "--name", "cut/x"); err != nil {
		t.Fatalf("cut: %v", err)
	}
	dir := filepath.Join(root, ".herd", "worktrees", "cut-x")
	got, err := os.ReadFile(filepath.Join(dir, "docs", "a.md"))
	if err != nil {
		t.Fatalf("scoped file missing from candidate: %v", err)
	}
	if !strings.Contains(string(got), "lane work") {
		t.Errorf("scoped work was not carried across: %q", got)
	}
	// The unrelated file must NOT come along, or the cut is not bounded.
	out, err := os.ReadFile(filepath.Join(dir, "other.md"))
	if err != nil {
		t.Fatalf("read other.md: %v", err)
	}
	if strings.Contains(string(out), "unrelated") {
		t.Error("out-of-scope work leaked into the candidate; the cut is not bounded")
	}
}

// The lane keeps working, so extraction must never mutate its branch.
func TestLaneCutLeavesTheLaneBranchUntouched(t *testing.T) {
	root := cutRepo(t)
	before, _ := exec.Command("git", "-C", root, "rev-parse", "standing/lane").Output()
	if err := runCut(t, root, "--branch", "standing/lane", "--scope", "docs", "--name", "cut/y"); err != nil {
		t.Fatalf("cut: %v", err)
	}
	after, _ := exec.Command("git", "-C", root, "rev-parse", "standing/lane").Output()
	if string(before) != string(after) {
		t.Fatal("the lane branch was mutated; this is an extraction, not a move")
	}
}

// An unscoped cut would rebuild the same unreviewable blob under a new name,
// which is precisely what made the work unmergeable.
func TestLaneCutRefusesAnUnscopedCut(t *testing.T) {
	root := cutRepo(t)
	err := runCut(t, root, "--branch", "standing/lane")
	if err == nil || !strings.Contains(err.Error(), "--scope is required") {
		t.Fatalf("an unscoped cut must be refused: %v", err)
	}
}

// A scope with nothing to extract is refused here rather than producing a
// candidate that review ingest would refuse anyway for an empty diff.
func TestLaneCutRefusesAnEmptyScope(t *testing.T) {
	root := cutRepo(t)
	err := runCut(t, root, "--branch", "standing/lane", "--scope", "nonexistent")
	if err == nil || !strings.Contains(err.Error(), "EMPTY diff") {
		t.Fatalf("an empty scope must be refused with the reason: %v", err)
	}
}
