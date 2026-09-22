package main

import (
	"os/exec"
	"testing"
)

func gitInitIssuanceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-q", "-m", "base")
	return dir
}

func gitSHA(t *testing.T, dir string, rev string) string {
	t.Helper()
	out, err := gitC(dir, "rev-parse", rev)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return out
}

func TestVerifierIssuanceBaseUsesMergeBaseWhenMainAdvanced(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	base := gitSHA(t, dir, "HEAD")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("checkout", "-q", "-b", "feature")
	run("commit", "--allow-empty", "-q", "-m", "candidate")
	candidate := gitSHA(t, dir, "HEAD")
	run("checkout", "-q", "main")
	run("commit", "--allow-empty", "-q", "-m", "later-main")
	originMain := gitSHA(t, dir, "HEAD")
	run("update-ref", "refs/remotes/origin/main", originMain)
	if gitIsAncestor(dir, originMain, candidate) {
		t.Fatal("fixture did not advance origin/main past the candidate")
	}
	got, err := verifierIssuanceBase(dir, originMain, candidate, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf("base=%s want merge-base %s (not advanced main %s)", got, base, originMain)
	}
	if !gitIsAncestor(dir, got, candidate) {
		t.Fatal("selected base is not an ancestor of the candidate")
	}
}

func TestVerifierIssuanceBaseRefusesExplicitNonAncestor(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	base := gitSHA(t, dir, "HEAD")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("commit", "--allow-empty", "-q", "-m", "later-main")
	later := gitSHA(t, dir, "HEAD")
	run("update-ref", "HEAD", base)
	candidate := gitSHA(t, dir, "HEAD")
	if _, err := verifierIssuanceBase(dir, later, candidate, "", later); err == nil {
		t.Fatal("explicit advanced main was accepted as verifier base")
	}
}

func TestVerifierIssuanceBasePreservesAuthenticatedAncestralBase(t *testing.T) {
	dir := gitInitIssuanceRepo(t)
	base := gitSHA(t, dir, "HEAD")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("checkout", "-q", "-b", "feature")
	run("commit", "--allow-empty", "-q", "-m", "candidate")
	candidate := gitSHA(t, dir, "HEAD")
	run("checkout", "-q", "main")
	run("commit", "--allow-empty", "-q", "-m", "later-main")
	originMain := gitSHA(t, dir, "HEAD")
	got, err := verifierIssuanceBase(dir, originMain, candidate, base, "")
	if err != nil || got != base {
		t.Fatalf("authenticated base: got %s err=%v want %s", got, err, base)
	}
}
