package verifier

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func vgit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// completionRepo builds a repo with origin/main plus a worktree branch whose
// commits are controllable per test.
func completionRepo(t *testing.T, subjects ...string) string {
	dir := t.TempDir()
	vgit(t, dir, "init", "-q", "-b", "main")
	vgit(t, dir, "config", "user.email", "t@h.local")
	vgit(t, dir, "config", "user.name", "t")
	vgit(t, dir, "commit", "--allow-empty", "-q", "-m", "base")
	vgit(t, dir, "update-ref", "refs/remotes/origin/main", mustHead(t, dir))
	for _, s := range subjects {
		vgit(t, dir, "commit", "--allow-empty", "-q", "-m", s)
	}
	return dir
}

func mustHead(t *testing.T, dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
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

func TestCheckCompletion_EmptyCmdsSkip(t *testing.T) {
	dir := completionRepo(t, "feat: work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "", "")
	if !c.Passed {
		t.Fatalf("empty build/test cmds skip as passed: %+v", c)
	}
}
