package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRepoWithLandedHead builds a repo whose branch HEAD is already contained in
// origin/main -- the shape of work rebase-merged outside the fleet.
func gitRepoWithLandedHead(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	if err := exec.Command("sh", "-c", "echo one > "+filepath.Join(dir, "f")).Run(); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "one")
	// origin/main points at the same commit HEAD is on.
	run("update-ref", "refs/remotes/origin/main", "HEAD")
	return dir
}

// TestCandidateResolutionRefusesLandedHead is the FAC-566 regression.
//
// The resolver fell back to worktree HEAD, which is correct only while work is
// unmerged. For work already rebase-merged, HEAD is the LANDED head, so the
// recorded candidate became the merge commit -- and Route B then refused the
// legitimate verdict it existed to authorize.
func TestCandidateResolutionRefusesLandedHead(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	_, err := resolveVerifyLandedCandidate(dir, "reconstruct/cha-2183", verifyLandedBinding{})
	if err == nil {
		t.Fatal("a HEAD already contained in origin/main must not be used as the candidate")
	}
	for _, want := range []string{"already contained", "--candidate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal must explain and name the remedy (%q), got %v", want, err)
		}
	}
}

// An explicit candidate always wins: the operator named the object.
func TestExplicitCandidateWins(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	want := "fc3bd108309667d67493a09efc9725f47b15452f"
	got, err := resolveVerifyLandedCandidate(dir, "b", verifyLandedBinding{Candidate: want})
	if err != nil || got != want {
		t.Fatalf("explicit candidate must win, got %q %v", got, err)
	}
}

// An unmerged HEAD is still a valid candidate; this guard must not break the
// ordinary pre-merge harvest.
func TestUnmergedHeadStillResolves(t *testing.T) {
	dir := gitRepoWithLandedHead(t)
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Advance HEAD past origin/main so it is genuinely unmerged.
	if err := exec.Command("sh", "-c", "echo two >> "+filepath.Join(dir, "f")).Run(); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "two")

	got, err := resolveVerifyLandedCandidate(dir, "b", verifyLandedBinding{})
	if err != nil || got == "" {
		t.Fatalf("an unmerged HEAD must still resolve as the candidate: %q %v", got, err)
	}
}
