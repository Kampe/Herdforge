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

func TestOpenAndNewRejectInvalidTargets(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "missing")
	if err := Open(missing); err == nil || err.Error() != "herd-reset-safe: "+missing+" does not exist" {
		t.Fatalf("missing directory error = %v", err)
	}
	nondir := filepath.Join(t.TempDir(), "file")
	if err := osWrite(nondir, "not a directory\n"); err != nil {
		t.Fatal(err)
	}
	if err := Open(nondir); err == nil || err.Error() != "herd-reset-safe: "+nondir+" does not exist" {
		t.Fatalf("non-directory error = %v", err)
	}
	nonrepo := t.TempDir()
	if _, err := New(ctx, nonrepo, nonrepo, Options{}); err == nil || !strings.Contains(err.Error(), "not a git worktree") {
		t.Fatalf("non-repository must fail closed, got %v", err)
	}
}

func TestNewRejectsMasterDetachedAndMismatchedRepositories(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	masterWT := filepath.Join(t.TempDir(), "master-wt")
	git(t, root, "worktree", "add", "-q", "-b", "master", masterWT)
	if _, err := New(ctx, root, masterWT, Options{}); err == nil || !strings.Contains(err.Error(), "refusing on 'master'") {
		t.Fatalf("master must be refused, got %v", err)
	}
	otherRoot, otherWT, _ := fixture(t)
	if _, err := New(ctx, otherRoot, wt, Options{}); err == nil || !strings.Contains(err.Error(), "not owned by repo root") {
		t.Fatalf("mismatched repository must be refused, got %v", err)
	}
	_ = otherWT
	git(t, wt, "checkout", "--detach", "-q")
	if _, err := New(ctx, root, wt, Options{}); err == nil || !strings.Contains(err.Error(), "refusing on detached HEAD") {
		t.Fatalf("detached HEAD must be refused, got %v", err)
	}
}

func TestNewDirtyFormattingAndPacketWhitelist(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	if err := osWrite(filepath.Join(wt, "tracked.txt"), "one\n"); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "tracked.txt")
	git(t, wt, "commit", "-q", "-m", "tracked")
	if err := osWrite(filepath.Join(wt, "tracked.txt"), "two\n"); err != nil {
		t.Fatal(err)
	}
	if err := osWrite(filepath.Join(wt, "TASK-PACKET.md"), "packet\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, root, wt, Options{}); err == nil || err.Error() != "herd-reset-safe: "+wt+" has uncommitted changes, refusing:\n   M tracked.txt\nherd-reset-safe: commit or stash first, then re-run" {
		t.Fatalf("dirty error formatting = %q", err)
	}
	if err := os.Remove(filepath.Join(wt, "tracked.txt")); err != nil {
		t.Fatal(err)
	}
	if err := osWrite(filepath.Join(wt, "tracked.txt"), "one\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, root, wt, Options{}); err != nil {
		t.Fatalf("sole TASK-PACKET.md must be allowed: %v", err)
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

func TestRunPropagatesResetFailure(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	var out, errOut bytes.Buffer
	plan, err := New(ctx, root, wt, Options{Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatal(err)
	}
	git(t, root, "update-ref", "-d", "refs/remotes/origin/main")
	if _, err := plan.Run(ctx); err == nil || !strings.Contains(err.Error(), "reset failed") {
		t.Fatalf("reset failure must propagate, got %v", err)
	}
	if !strings.Contains(out.String(), "safe to reset") || errOut.String() != "" {
		t.Fatalf("unexpected failure output: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestRunExactCleanOutput(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	var out, errOut bytes.Buffer
	plan, err := New(ctx, root, wt, Options{Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := "herd-reset-safe: " + wt + " (feature/cha-77) has no unmerged work, safe to reset\n" +
		"herd-reset-safe: " + wt + " reset to origin/main (" + plan.ResetSHA + ")\n"
	if out.String() != want || errOut.String() != "" {
		t.Fatalf("output mismatch:\nwant %q\nstdout %q\nstderr %q", want, out.String(), errOut.String())
	}
}

func TestRunPreservesBeforeResetAndLeavesSiblingUntouched(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	sibling := filepath.Join(t.TempDir(), "sibling-wt")
	git(t, root, "worktree", "add", "-q", "-b", "sibling", sibling)
	git(t, wt, "commit", "--allow-empty", "-q", "-m", "unique")
	uniqueSHA := git(t, wt, "rev-parse", "HEAD")
	siblingSHA := git(t, sibling, "rev-parse", "HEAD")
	rootSHA := git(t, root, "rev-parse", "HEAD")
	refsBefore := git(t, root, "for-each-ref", "--format=%(refname)=%(objectname)", "refs/heads/")
	plan, err := New(ctx, root, wt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt, "show-ref", "--hash", "refs/heads/"+plan.PreserveBranch); got != uniqueSHA {
		t.Fatalf("preserve branch was not created before reset: got %s want %s", got, uniqueSHA)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != git(t, root, "rev-parse", "origin/main") {
		t.Fatalf("target HEAD was not reset: %s", got)
	}
	if got := git(t, sibling, "rev-parse", "HEAD"); got != siblingSHA {
		t.Fatalf("sibling HEAD changed: got %s want %s", got, siblingSHA)
	}
	if got := git(t, root, "rev-parse", "refs/heads/main"); got != rootSHA {
		t.Fatalf("main ref changed: got %s want %s", got, rootSHA)
	}
	refsAfter := git(t, root, "for-each-ref", "--format=%(refname)=%(objectname)", "refs/heads/")
	if !conservedRefs(refsBefore, refsAfter, "refs/heads/"+plan.PreserveBranch, "refs/heads/feature/cha-77") {
		t.Fatalf("unexpected ref mutations:\nbefore:\n%s\nafter:\n%s", refsBefore, refsAfter)
	}
}

func conservedRefs(before, after, preserve, target string) bool {
	old := refMap(before)
	now := refMap(after)
	for ref, sha := range old {
		if ref == target {
			continue
		}
		if now[ref] != sha {
			return false
		}
	}
	for ref := range now {
		if _, ok := old[ref]; !ok && ref != preserve {
			return false
		}
	}
	return now[preserve] != ""
}

func refMap(text string) map[string]string {
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			refs[parts[0]] = parts[1]
		}
	}
	return refs
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
