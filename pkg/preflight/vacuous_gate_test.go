package preflight

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func vgGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func vgRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	vgGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "seed.go"), []byte("package seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vgGit(t, dir, "add", "-A")
	vgGit(t, dir, "commit", "-qm", "seed")
	// origin/main must exist for the gate's diff base.
	vgGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	return dir
}

// TestNewFileIsNotInvisibleToTheGate is the FAC-430 gate.
//
// changedPaths used --untracked-files=no, so a BRAND NEW file was invisible. The
// result was a vacuous pass exactly when the gate matters most: a new file with
// an absolute path leak passed preflight, then failed the identical check once
// committed. A pre-commit gate that cannot see the file being added is
// validating work that already shipped, not work about to ship.
func TestNewFileIsNotInvisibleToTheGate(t *testing.T) {
	dir := vgRepo(t)
	// A NEW, untracked file carrying a leak — the exact reported repro.
	if err := os.WriteFile(filepath.Join(dir, "leak.go"),
		[]byte("package p\n\nconst p = \"/Users/someone/leaked\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := CheckWorktreeBoundaryChanged(dir, nil)
	if err == nil {
		t.Fatal("a new untracked file with an absolute path leak must fail the gate, not pass vacuously")
	}
	if !strings.Contains(err.Error(), "leak.go") {
		t.Errorf("the failure must name the offending file, got: %v", err)
	}
}

// The gate must still honour .gitignore, or every build artifact and runtime
// file becomes a false positive and the gate gets switched off.
func TestIgnoredFilesStayExcluded(t *testing.T) {
	dir := vgRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("artifacts/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vgGit(t, dir, "add", ".gitignore")
	vgGit(t, dir, "commit", "-qm", "ignore artifacts")
	vgGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")

	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "artifacts", "out.go"),
		[]byte("package p\n\nconst p = \"/Users/someone/build\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckWorktreeBoundaryChanged(dir, nil); err != nil {
		t.Fatalf("an ignored artifact must not fail the gate: %v", err)
	}
}

// A committed leak must still fail: the fix widens what the gate sees, it does
// not narrow it.
func TestCommittedLeakStillFails(t *testing.T) {
	dir := vgRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "leak.go"),
		[]byte("package p\n\nconst p = \"/Users/someone/leaked\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vgGit(t, dir, "add", "-A")
	vgGit(t, dir, "commit", "-qm", "leak")
	if err := CheckWorktreeBoundaryChanged(dir, nil); err == nil {
		t.Fatal("a committed leak must still fail")
	}
}

// And a clean tree must pass, so the widened view has not made the gate
// unconditionally red.
func TestCleanTreePasses(t *testing.T) {
	dir := vgRepo(t)
	if err := CheckWorktreeBoundaryChanged(dir, nil); err != nil {
		t.Fatalf("a clean tree must pass: %v", err)
	}
}
