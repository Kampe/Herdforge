package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
)

func TestObserveVerifyLandedSquashPreservesCandidate(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	git := func(dir string, args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		b, e := c.CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git(root, "init", "--bare", "-q", "-b", "main", origin)
	git(root, "clone", "-q", origin, repo)
	for _, kv := range [][2]string{{"user.name", "test"}, {"user.email", "test@example.invalid"}, {"commit.gpgsign", "false"}} {
		git(repo, "config", kv[0], kv[1])
	}
	commit := func(body, msg string) string {
		t.Helper()
		if e := os.WriteFile(filepath.Join(repo, "a"), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		git(repo, "add", "a")
		git(repo, "commit", "-q", "-m", msg)
		return git(repo, "rev-parse", "HEAD")
	}
	base := commit("base\n", "base")
	git(repo, "push", "-q", "origin", "main")
	git(repo, "checkout", "-q", "-b", "work")
	commit("middle\n", "first")
	candidate := commit("final\n", "second")
	git(repo, "checkout", "-q", "main")
	git(repo, "merge", "--squash", "work")
	git(repo, "commit", "-q", "-m", "squash")
	landed := git(repo, "rev-parse", "HEAD")
	git(repo, "push", "-q", "origin", "main")
	git(repo, "checkout", "-q", "work")
	gate := &mergeadmit.Gate{RepoDir: repo}
	req := mergeadmit.Request{BaseSHA: base, CandidateSHA: candidate}
	proof, err := observeVerifyLanded(repo, gate, req)
	if err != nil {
		t.Fatalf("squash observation: %v", err)
	}
	if proof.MergeSHA != landed || proof.Mode != mergeadmit.ModeSquash {
		t.Fatalf("wrong proof: %+v", proof)
	}
	if head := git(repo, "rev-parse", "HEAD"); head != candidate {
		t.Fatalf("observer rewrote candidate to %s", head)
	}
	if e := os.WriteFile(filepath.Join(repo, "a"), []byte("dirty\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := observeVerifyLanded(repo, gate, req); e == nil {
		t.Fatal("dirty worktree admitted")
	}
	git(repo, "checkout", "--", "a")
	git(repo, "remote", "set-url", "origin", filepath.Join(root, "missing.git"))
	if _, e := observeVerifyLanded(repo, gate, req); e == nil {
		t.Fatal("failed fetch admitted stale origin/main")
	}
}
