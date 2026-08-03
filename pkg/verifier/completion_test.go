package verifier

import (
	"context"
	"strings"
	"testing"
)

func vgit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := runGit(dir, args...); err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
}

// completionRepo builds a repo with origin/main plus a worktree branch whose
// commits are controllable per test.
func completionRepo(t *testing.T, subjects ...string) string {
	dir := t.TempDir()
	registerTempDirLifecycleBarrier(t, dir)
	vgit(t, dir, "init", "-q", "-b", "main")
	vgit(t, dir, "config", "user.email", "t@h.local")
	vgit(t, dir, "config", "user.name", "t")
	vgit(t, dir, "config", "commit.gpgsign", "false")
	vgit(t, dir, "commit", "--allow-empty", "-q", "-m", "base")
	vgit(t, dir, "update-ref", "refs/remotes/origin/main", mustHead(t, dir))
	for _, s := range subjects {
		vgit(t, dir, "commit", "--allow-empty", "-q", "-m", s)
	}
	return dir
}

func mustHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := runGit(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestCheckCompletion_RealWorkPasses(t *testing.T) {
	dir := completionRepo(t, "feat: real work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "true", "true")
	if !c.Passed || !c.HasCommits {
		t.Fatalf("real work must pass: %+v", c)
	}
}

func TestCheckCompletion_OnlyAnchorFails(t *testing.T) {
	dir := completionRepo(t, "chore: anchor FAC-1 worktree (FAC-106 reap-safe)", "wip: partial")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "true", "true")
	if c.Passed || c.HasCommits {
		t.Fatalf("anchor+wip only must fail the gate: %+v", c)
	}
	if len(c.Reasons) == 0 || !strings.Contains(c.Reasons[0], "no real commits") {
		t.Fatalf("must explain the whiff: %+v", c.Reasons)
	}
}

func TestCheckCompletion_BuildFailFails(t *testing.T) {
	dir := completionRepo(t, "feat: work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "false", "true")
	if c.Passed || c.Builds {
		t.Fatalf("build failure must fail the gate: %+v", c)
	}
}

func TestCheckCompletion_TestFailFails(t *testing.T) {
	dir := completionRepo(t, "feat: work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "true", "false")
	if c.Passed || c.TestsPass {
		t.Fatalf("test failure must fail the gate: %+v", c)
	}
}

func TestCheckCompletion_EmptyCmdsFailClosed(t *testing.T) {
	dir := completionRepo(t, "feat: work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "", "")
	if c.Passed || c.Builds || c.TestsPass {
		t.Fatalf("empty build/test commands must fail closed: %+v", c)
	}
}
