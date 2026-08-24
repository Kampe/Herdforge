package preflight

import (
	"os/exec"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

// A detached checkout with no local main branch -- exactly what CI produces for
// a pull request -- must not be reported as divergence. There is no local
// history to have drifted from origin/main.
func TestCheckMainOriginDivergence_NoLocalMainIsNotDivergence(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "feature")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "c1")

	got, err := CheckMainOriginDivergence(dir)
	if err != nil {
		t.Fatalf("missing local main must not be an error, got: %v", err)
	}
	if got.LocalAhead != 0 || got.RemoteAhead != 0 {
		t.Fatalf("want zero divergence, got %+v", got)
	}
}

// The real property must survive: a local main that has drifted from origin/main
// still fails, because that means commits landed outside the PR flow.
func TestCheckMainOriginDivergence_DivergedLocalMainStillFails(t *testing.T) {
	origin := t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "main")

	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	git(t, dir, "remote", "add", "origin", origin)
	git(t, dir, "push", "-q", "origin", "main")
	// Land a commit locally that never went through origin.
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "local-only")

	if _, err := CheckMainOriginDivergence(dir); err == nil {
		t.Fatal("a local main ahead of origin/main must still fail closed")
	}
}
