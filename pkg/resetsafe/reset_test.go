package resetsafe

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.email=test@herdforge.local", "-c", "user.name=Test Runner", "-c", "commit.gpgSign=false"}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func fixture(t *testing.T) (root, wt, remote string) {
	t.Helper()
	root = t.TempDir()
	remote = filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, remote, "init", "--bare", "-q")
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	git(t, root, "remote", "add", "origin", remote)
	git(t, root, "push", "-q", "-u", "origin", "main")
	wt = filepath.Join(t.TempDir(), "feature-wt")
	git(t, root, "worktree", "add", "-q", "-b", "feature/cha-77", wt)
	return root, wt, remote
}

func TestNewRejectsMainAndDirtyWorktrees(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	if _, err := New(ctx, root, root, Options{}); err == nil || !strings.Contains(err.Error(), "refusing on 'main'") {
		t.Fatalf("main must be refused, got %v", err)
	}

	if err := osWrite(filepath.Join(wt, "tracked.txt"), "dirty\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, root, wt, Options{}); err == nil || !strings.Contains(err.Error(), "commit or stash first, then re-run") {
		t.Fatalf("dirty worktree must be refused, got %v", err)
	}
}

func TestNewAllowsPacketOnlyAndUsesHarvestUnmergedFor(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	if err := osWrite(filepath.Join(wt, "TASK-PACKET.md"), "local packet\n"); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "commit", "--allow-empty", "-q", "-m", "unique")
	plan, err := New(ctx, root, wt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Unique) != 1 || plan.PreserveBranch != "harvest/feature-cha-77-"+plan.ShortSHA {
		t.Fatalf("unexpected preserve plan: %+v", plan)
	}
}

func TestRunPreservesPushesAndResetsOnlyTarget(t *testing.T) {
	ctx := context.Background()
	root, wt, remote := fixture(t)
	git(t, wt, "commit", "--allow-empty", "-q", "-m", "unique")
	var out, errOut bytes.Buffer
	plan, err := New(ctx, root, wt, Options{Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatal(err)
	}
	got, err := plan.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Pushed || got.ResetSHA == "" || !strings.Contains(out.String(), "reset to origin/main") {
		t.Fatalf("unexpected run result/output: %+v\n%s", got, out.String())
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != git(t, root, "rev-parse", "origin/main") {
		t.Fatalf("target was not reset to origin/main: %s", got)
	}
	if got := git(t, wt, "show-ref", "--verify", "refs/heads/"+plan.PreserveBranch); got == "" {
		t.Fatal("preserve branch missing locally")
	}
	if got := git(t, remote, "show-ref", "refs/heads/"+plan.PreserveBranch); got == "" {
		t.Fatal("preserve branch was not pushed")
	}
}

func TestRunPushFailureStillResetsAndLeavesLocalBranch(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	git(t, wt, "commit", "--allow-empty", "-q", "-m", "unique")
	git(t, root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	var out, errOut bytes.Buffer
	plan, err := New(ctx, root, wt, Options{Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatal(err)
	}
	got, err := plan.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pushed || !strings.Contains(errOut.String(), "WARN could not push "+plan.PreserveBranch) {
		t.Fatalf("push failure was not downgraded: pushed=%v stderr=%s", got.Pushed, errOut.String())
	}
	if git(t, wt, "show-ref", "--verify", "refs/heads/"+plan.PreserveBranch) == "" {
		t.Fatal("local preserve branch missing after push failure")
	}
	if git(t, wt, "rev-parse", "HEAD") != git(t, root, "rev-parse", "origin/main") {
		t.Fatal("reset did not execute after push failure")
	}
}

func TestNewReportsPatchEquivalentCommitAsClean(t *testing.T) {
	ctx := context.Background()
	root, wt, remote := fixture(t)
	if err := osWrite(filepath.Join(wt, "same-patch.txt"), "same patch\n"); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "same-patch.txt")
	git(t, wt, "commit", "-q", "-m", "same patch")
	if err := osWrite(filepath.Join(root, "same-patch.txt"), "same patch\n"); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "same-patch.txt")
	git(t, root, "commit", "-q", "-m", "replayed same patch")
	git(t, root, "push", "-q", "origin", "main")
	git(t, wt, "fetch", "-q", "origin", "main")
	_ = remote
	plan, err := New(ctx, root, wt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || len(plan.Unique) != 0 {
		t.Fatalf("patch-equivalent commit must be clean: %+v", plan)
	}
}

func osWrite(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o644)
}
