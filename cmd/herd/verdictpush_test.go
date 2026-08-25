package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	// Reproduce the condition that broke the packet recipe: .herd is ignored.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("/.herd/*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The whole reason this command exists: .gitignore covers /.herd/*, so a plain
// `git add` of a verdict silently no-ops and the reviewer sees "nothing to
// commit". Plumbing does not consult .gitignore, so the blob must still be
// written.
func TestVerdictPush_WritesBlobDespiteGitignore(t *testing.T) {
	dir := initRepo(t)
	inbox := filepath.Join(dir, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	art := filepath.Join(inbox, "v.md")
	if err := os.WriteFile(art, []byte("sha: x\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Prove the plain-add path really does no-op, so this test documents the bug.
	add := exec.Command("git", "-C", dir, "add", art)
	_ = add.Run()
	// Assert on the ARTIFACT, not on a clean status: .gitignore itself is
	// untracked here and shows as "?? .gitignore", which says nothing about
	// whether the verdict was staged.
	st := exec.Command("git", "-C", dir, "diff", "--cached", "--name-only")
	out, _ := st.CombinedOutput()
	if strings.Contains(string(out), "v.md") {
		t.Fatalf("precondition failed: .herd was expected to be ignored, but the verdict staged: %s", out)
	}

	hash := exec.Command("git", "-C", dir, "hash-object", "-w", art)
	blobOut, err := hash.CombinedOutput()
	if err != nil {
		t.Fatalf("plumbing must write past .gitignore: %v (%s)", err, blobOut)
	}
	if len(strings.TrimSpace(string(blobOut))) != 40 {
		t.Fatalf("expected a blob sha, got %q", blobOut)
	}
}

// An empty artifact must be refused: transporting a blank verdict wastes a ref
// and looks like a delivered review.
func TestVerdictPush_RefusesEmptyArtifact(t *testing.T) {
	dir := initRepo(t)
	art := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(art, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_ROOT", dir)
	os.Args = []string{"herd", "verdict-push", "--artifact", art, "--workspace", "w2", "--dry-run"}
	if err := runVerdictPush(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty verdict must be refused, got %v", err)
	}
}

// Without a resolvable workspace the ref would be verdicts/-<leaf>, which is the
// same class of bug as the $(herd config workspace) expansion that produced
// verdicts/ and always failed.
func TestVerdictPush_RefusesUnresolvableWorkspace(t *testing.T) {
	dir := initRepo(t)
	art := filepath.Join(dir, "v.md")
	if err := os.WriteFile(art, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_ROOT", dir)
	t.Setenv("HERD_WORKSPACE", "")
	t.Setenv("HERD_NO_LIVE_HERDR", "1")
	os.Args = []string{"herd", "verdict-push", "--artifact", art, "--dry-run"}
	if err := runVerdictPush(); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("an unresolvable workspace must be refused, got %v", err)
	}
}
