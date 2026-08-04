package resetsafe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testgit.Command(dir, args...)
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
	if _, err := New(ctx, nonrepo, nonrepo, Options{}); err == nil || !strings.Contains(err.Error(), "not a git repository") {
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

func TestCanonicalCommonDirFailsClosedAndResolvesSymlink(t *testing.T) {
	if _, err := canonicalCommonDir("", "fixture"); err == nil || !strings.Contains(err.Error(), "common dir is empty") {
		t.Fatalf("empty common dir must fail closed, got %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing-common-dir")
	if _, err := canonicalCommonDir(missing, "fixture"); err == nil || !strings.Contains(err.Error(), "cannot resolve fixture git common dir") {
		t.Fatalf("unresolvable common dir must fail closed, got %v", err)
	}
	realDir := filepath.Join(t.TempDir(), "real-common-dir")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "common-dir-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalCommonDir(link, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != want {
		t.Fatalf("symlink common dir = %q, want %q", canonical, want)
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
	var out, errOut bytes.Buffer
	plan, err := New(ctx, root, wt, Options{Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatal(err)
	}
	oldRun := gitRunFn
	gitRunFn = func(ctx context.Context, dir string, args ...string) error {
		err := oldRun(ctx, dir, args...)
		if err == nil && len(args) >= 2 && args[0] == "push" {
			return errors.New("injected push failure")
		}
		return err
	}
	t.Cleanup(func() { gitRunFn = oldRun })
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

func TestRunRefMutationAfterPushBlocksReset(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retarget  bool
		pushError bool
	}{
		{name: "delete after push succeeds"},
		{name: "retarget after push succeeds", retarget: true},
		{name: "delete after push fails", pushError: true},
		{name: "retarget after push fails", retarget: true, pushError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root, wt, _ := fixture(t)
			git(t, wt, "commit", "--allow-empty", "-q", "-m", "unique")
			plan, err := New(ctx, root, wt, Options{})
			if err != nil {
				t.Fatal(err)
			}
			oldRun := gitRunFn
			gitRunFn = func(ctx context.Context, dir string, args ...string) error {
				err := oldRun(ctx, dir, args...)
				if err == nil && len(args) >= 2 && args[0] == "push" {
					ref := "refs/heads/" + plan.authority.preserveBranch
					if tc.retarget {
						git(t, dir, "update-ref", ref, "origin/main")
					} else {
						git(t, dir, "update-ref", "-d", ref)
					}
					if tc.pushError {
						return errors.New("injected push failure")
					}
				}
				return err
			}
			t.Cleanup(func() { gitRunFn = oldRun })
			if _, err := plan.Run(ctx); err == nil || !strings.Contains(err.Error(), "preserve ref changed") {
				t.Fatalf("preserve ref mutation must block reset, got %v", err)
			}
			if got := git(t, wt, "rev-parse", "HEAD"); got != plan.authority.head {
				t.Fatalf("reset ran after preserve ref mutation: got %s want %s", got, plan.authority.head)
			}
		})
	}
}

func TestNewStrictCherryFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	if err := osWrite(filepath.Join(wt, "unique.txt"), "unique\n"); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", "unique.txt")
	git(t, wt, "commit", "-q", "-m", "unique")
	missingAncestor := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "commit", "--allow-empty", "-q", "-m", "tip")
	objects := git(t, wt, "rev-parse", "--git-path", "objects")
	if !filepath.IsAbs(objects) {
		objects = filepath.Join(wt, objects)
	}
	objectPath := filepath.Join(objects, missingAncestor[:2], missingAncestor[2:])
	if err := os.Remove(objectPath); err != nil {
		t.Fatalf("remove fixture object: %v", err)
	}
	if _, err := New(ctx, root, wt, Options{}); err == nil || !strings.Contains(err.Error(), "cannot verify unmerged work") || !strings.Contains(err.Error(), "git cherry") {
		t.Fatalf("strict cherry failure must propagate, got %v", err)
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
	oldRun := gitRunFn
	gitRunFn = func(ctx context.Context, dir string, args ...string) error {
		if len(args) >= 2 && args[0] == "reset" && args[1] == "--hard" {
			return errors.New("injected reset failure")
		}
		return oldRun(ctx, dir, args...)
	}
	t.Cleanup(func() { gitRunFn = oldRun })
	if _, err := plan.Run(ctx); err == nil || !strings.Contains(err.Error(), "reset failed") {
		t.Fatalf("reset failure must propagate, got %v", err)
	}
	if !strings.Contains(out.String(), "safe to reset") || errOut.String() != "" {
		t.Fatalf("unexpected failure output: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestRunIgnoresExportedAuthorityTamperingAndSiblingRetarget(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	sibling := filepath.Join(t.TempDir(), "sibling-wt")
	git(t, root, "worktree", "add", "-q", "-b", "sibling", sibling)
	git(t, wt, "commit", "--allow-empty", "-q", "-m", "unique")
	plan, err := New(ctx, root, wt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	originalPreserve := plan.authority.preserveBranch
	plan.Worktree = sibling
	plan.Branch = "main"
	plan.ShortSHA = "tampered"
	plan.Unique = nil
	plan.PreserveBranch = "harvest/evil"
	if _, err := plan.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != git(t, root, "rev-parse", "origin/main") {
		t.Fatalf("bound target was not reset: %s", got)
	}
	if got := git(t, sibling, "rev-parse", "HEAD"); got != git(t, root, "rev-parse", "origin/main") {
		t.Fatalf("sibling was retargeted or changed: %s", got)
	}
	if git(t, wt, "show-ref", "--verify", "refs/heads/"+originalPreserve) == "" {
		t.Fatal("bound preserve branch was not used")
	}
	if hasRef(wt, "refs/heads/harvest/evil") {
		t.Fatal("tampered preserve branch was used")
	}
}

func TestRunRejectsPostPlanDirtyChange(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	plan, err := New(ctx, root, wt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := osWrite(filepath.Join(wt, "late.txt"), "late\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Run(ctx); err == nil || !strings.Contains(err.Error(), "planned worktree has uncommitted changes") {
		t.Fatalf("post-plan dirty state must fail closed, got %v", err)
	}
	if git(t, wt, "rev-parse", "HEAD") != git(t, root, "rev-parse", "origin/main") {
		t.Fatal("post-plan dirty rejection must not reset")
	}
}

func TestRunRejectsPostPlanHeadBranchAndOriginDrift(t *testing.T) {
	ctx := context.Background()
	t.Run("head drift", func(t *testing.T) {
		root, wt, _ := fixture(t)
		plan, err := New(ctx, root, wt, Options{})
		if err != nil {
			t.Fatal(err)
		}
		git(t, wt, "commit", "--allow-empty", "-q", "-m", "late commit")
		if _, err := plan.Run(ctx); err == nil || !strings.Contains(err.Error(), "planned HEAD changed") {
			t.Fatalf("HEAD drift must fail closed, got %v", err)
		}
	})
	t.Run("branch drift", func(t *testing.T) {
		root, wt, _ := fixture(t)
		plan, err := New(ctx, root, wt, Options{})
		if err != nil {
			t.Fatal(err)
		}
		git(t, wt, "checkout", "-q", "-b", "late-branch")
		if _, err := plan.Run(ctx); err == nil || !strings.Contains(err.Error(), "planned branch changed") {
			t.Fatalf("branch drift must fail closed, got %v", err)
		}
	})
	t.Run("origin main drift", func(t *testing.T) {
		root, wt, _ := fixture(t)
		plan, err := New(ctx, root, wt, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := osWrite(filepath.Join(root, "base-change.txt"), "changed\n"); err != nil {
			t.Fatal(err)
		}
		git(t, root, "add", "base-change.txt")
		git(t, root, "commit", "-q", "-m", "base changed")
		git(t, root, "push", "-q", "origin", "main")
		if _, err := plan.Run(ctx); err == nil || !strings.Contains(err.Error(), "planned origin/main changed") {
			t.Fatalf("origin/main drift must fail closed, got %v", err)
		}
	})
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
	siblingHeadBefore := git(t, sibling, "rev-parse", "HEAD")
	siblingRefBefore := git(t, root, "rev-parse", "refs/heads/sibling")
	siblingStatusBefore := git(t, sibling, "status", "--porcelain")
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
	if got := git(t, sibling, "rev-parse", "HEAD"); got != siblingHeadBefore {
		t.Fatalf("sibling HEAD changed: got %s want %s", got, siblingHeadBefore)
	}
	if got := git(t, root, "rev-parse", "refs/heads/sibling"); got != siblingRefBefore {
		t.Fatalf("sibling ref changed: got %s want %s", got, siblingRefBefore)
	}
	if got := git(t, sibling, "status", "--porcelain"); got != siblingStatusBefore {
		t.Fatalf("sibling status changed: got %q want %q", got, siblingStatusBefore)
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

func TestRunSkipsResetWhenPushIntroducesPreResetDrift(t *testing.T) {
	ctx := context.Background()
	root, wt, _ := fixture(t)
	git(t, wt, "commit", "--allow-empty", "-q", "-m", "unique")
	plan, err := New(ctx, root, wt, Options{})
	if err != nil {
		t.Fatal(err)
	}
	oldRun := gitRunFn
	var driftSHA string
	gitRunFn = func(ctx context.Context, dir string, args ...string) error {
		err := oldRun(ctx, dir, args...)
		if err == nil && len(args) >= 2 && args[0] == "push" {
			git(t, dir, "commit", "--allow-empty", "-q", "-m", "injected pre-reset drift")
			driftSHA = git(t, dir, "rev-parse", "HEAD")
		}
		return err
	}
	t.Cleanup(func() { gitRunFn = oldRun })
	if _, err := plan.Run(ctx); err == nil || !strings.Contains(err.Error(), "planned HEAD changed") {
		t.Fatalf("post-push drift must fail closed, got %v", err)
	}
	if driftSHA == "" {
		t.Fatal("injected drift did not execute")
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != driftSHA {
		t.Fatalf("reset ran after post-push drift: got %s want %s", got, driftSHA)
	}
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

func hasRef(dir, ref string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
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
