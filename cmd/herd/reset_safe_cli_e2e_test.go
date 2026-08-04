package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type resetSafeRepo struct {
	root     string
	worktree string
	sibling  string
	remote   string
}

func newResetSafeRepo(t *testing.T) resetSafeRepo {
	t.Helper()
	f := resetSafeRepo{
		root:   t.TempDir(),
		remote: filepath.Join(t.TempDir(), "origin.git"),
	}
	if err := os.MkdirAll(f.remote, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitT(t, f.remote, "init", "--bare", "-q")
	runGitT(t, f.root, "init", "-q", "-b", "main")
	runGitT(t, f.root, "config", "user.email", "reset-safe@test.invalid")
	runGitT(t, f.root, "config", "user.name", "Reset Safe Test")
	runGitT(t, f.root, "config", "commit.gpgSign", "false")
	runGitT(t, f.root, "commit", "--allow-empty", "-q", "-m", "base")
	runGitT(t, f.root, "remote", "add", "origin", f.remote)
	runGitT(t, f.root, "push", "-q", "-u", "origin", "main")
	f.worktree = filepath.Join(t.TempDir(), "feature-wt")
	f.sibling = filepath.Join(t.TempDir(), "sibling-wt")
	runGitT(t, f.root, "worktree", "add", "-q", "-b", "feature/cli", f.worktree)
	runGitT(t, f.root, "worktree", "add", "-q", "-b", "sibling", f.sibling)
	return f
}

func resetSafeCommand(t *testing.T, binary, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("herd reset-safe %v: %v", args, err)
	return "", -1
}

func resetSafeShape(t *testing.T, root string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(runGitT(t, root, "worktree", "list", "--porcelain")), "\n")
	var shape []string
	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") || strings.HasPrefix(line, "branch ") {
			shape = append(shape, line)
		}
	}
	return strings.Join(shape, "\n")
}

func resetSafeRefs(t *testing.T, root string) string {
	t.Helper()
	return runGitT(t, root, "for-each-ref", "--format=%(refname)=%(objectname)", "refs/heads")
}

func resetSafeRef(t *testing.T, dir, branch string) string {
	t.Helper()
	return strings.TrimSpace(runGitT(t, dir, "rev-parse", "refs/heads/"+branch))
}

func resetSafeRevision(t *testing.T, dir, ref string) string {
	t.Helper()
	return strings.TrimSpace(runGitT(t, dir, "rev-parse", ref))
}

func resetSafeCanonical(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestResetSafeCompiledNonRepoAndBranchRefusals(t *testing.T) {
	binary := buildHerd(t)

	nonRepo := t.TempDir()
	output, exit := resetSafeCommand(t, binary, nonRepo, "reset-safe", nonRepo)
	if exit != 1 || output != "herd-reset-safe: repo root is not a git repository\n" {
		t.Fatalf("non-repo result = exit %d, bytes %q", exit, output)
	}

	for _, branch := range []string{"main", "master"} {
		t.Run(branch, func(t *testing.T) {
			f := newResetSafeRepo(t)
			f.worktree = f.root
			if branch == "master" {
				master := filepath.Join(t.TempDir(), "master-wt")
				runGitT(t, f.root, "worktree", "add", "-q", "-b", "master", master)
				f.worktree = master
			}
			beforeShape, beforeRefs := resetSafeShape(t, f.root), resetSafeRefs(t, f.root)
			output, exit := resetSafeCommand(t, binary, f.root, "reset-safe", f.worktree)
			want := "herd-reset-safe: refusing on '" + branch + "' — this is for feature-branch worktrees, never the shared main checkout\n"
			if exit != 1 || output != want {
				t.Fatalf("%s result = exit %d, bytes %q, want %q", branch, exit, output, want)
			}
			if got := resetSafeShape(t, f.root); got != beforeShape {
				t.Fatalf("%s changed worktree shape: before %q after %q", branch, beforeShape, got)
			}
			if got := resetSafeRefs(t, f.root); got != beforeRefs {
				t.Fatalf("%s changed refs: before %q after %q", branch, beforeRefs, got)
			}
		})
	}
}

func TestResetSafeCompiledDirtyTrackedWorktree(t *testing.T) {
	binary := buildHerd(t)
	f := newResetSafeRepo(t)
	writeRepoFile(t, f.worktree, "tracked.txt", "clean\n")
	if err := os.WriteFile(filepath.Join(f.worktree, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeShape, beforeRefs := resetSafeShape(t, f.root), resetSafeRefs(t, f.root)
	output, exit := resetSafeCommand(t, binary, f.root, "reset-safe", f.worktree)
	want := "herd-reset-safe: " + f.worktree + " has uncommitted changes, refusing:\n   M tracked.txt\nherd-reset-safe: commit or stash first, then re-run\n"
	if exit != 1 || output != want {
		t.Fatalf("dirty result = exit %d, bytes %q, want %q", exit, output, want)
	}
	if got := resetSafeShape(t, f.root); got != beforeShape {
		t.Fatalf("dirty command changed worktree shape: before %q after %q", beforeShape, got)
	}
	if got := resetSafeRefs(t, f.root); got != beforeRefs {
		t.Fatalf("dirty command changed refs: before %q after %q", beforeRefs, got)
	}
}

func TestResetSafeCompiledPacketOnlySuccessIsDisposable(t *testing.T) {
	binary := buildHerd(t)
	f := newResetSafeRepo(t)
	if err := os.WriteFile(filepath.Join(f.worktree, "TASK-PACKET.md"), []byte("packet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeShape := resetSafeShape(t, f.root)
	output, exit := resetSafeCommand(t, binary, f.root, "reset-safe", f.worktree)
	want := "herd-reset-safe: " + f.worktree + " (feature/cli) has no unmerged work, safe to reset\n" +
		"herd-reset-safe: " + f.worktree + " reset to origin/main (" + strings.TrimSpace(runGitT(t, f.root, "rev-parse", "--short", "origin/main")) + ")\n"
	if exit != 0 || output != want {
		t.Fatalf("packet-only result = exit %d, bytes %q, want %q", exit, output, want)
	}
	if got := resetSafeShape(t, f.root); got != beforeShape {
		t.Fatalf("packet-only command changed worktree shape: before %q after %q", beforeShape, got)
	}
	if got := resetSafeRef(t, f.worktree, "feature/cli"); got != resetSafeRef(t, f.root, "main") {
		t.Fatalf("packet-only target did not reset to origin/main: %s", got)
	}
	if got := strings.TrimSpace(runGitT(t, f.sibling, "status", "--porcelain")); got != "" {
		t.Fatalf("packet-only command dirtied sibling: %q", got)
	}
}

func TestResetSafeCompiledPushFailurePreservesLocalRefAndSibling(t *testing.T) {
	binary := buildHerd(t)
	f := newResetSafeRepo(t)
	runGitT(t, f.worktree, "commit", "--allow-empty", "-q", "-m", "unique")
	uniqueSHA := strings.TrimSpace(runGitT(t, f.worktree, "rev-parse", "HEAD"))
	shortSHA := strings.TrimSpace(runGitT(t, f.worktree, "rev-parse", "--short", "HEAD"))
	preserve := "harvest/feature-cli-" + shortSHA
	siblingHead := resetSafeRef(t, f.sibling, "sibling")
	badRemote := filepath.Join(t.TempDir(), "missing-origin.git")
	runGitT(t, f.root, "config", "remote.origin.pushurl", badRemote)

	output, exit := resetSafeCommand(t, binary, f.root, "reset-safe", f.worktree)
	want := "herd-reset-safe: " + f.worktree + " has 1 unmerged commit(s), preserving to " + preserve + " before reset:\n" +
		"  " + uniqueSHA + "\n" +
		"herd-reset-safe: WARN could not push " + preserve + " — it still exists locally at " + f.worktree + " as a branch ref; do not delete this worktree until it's recovered\n" +
		"herd-reset-safe: " + f.worktree + " reset to origin/main (" + strings.TrimSpace(runGitT(t, f.root, "rev-parse", "--short", "origin/main")) + ")\n"
	if exit != 0 || output != want {
		t.Fatalf("push-failure result = exit %d, bytes %q, want %q", exit, output, want)
	}
	if got := resetSafeRef(t, f.worktree, preserve); got != uniqueSHA {
		t.Fatalf("local preserve ref = %s, want %s", got, uniqueSHA)
	}
	if got := resetSafeRef(t, f.sibling, "sibling"); got != siblingHead {
		t.Fatalf("push-failure changed sibling ref: %s", got)
	}
	if got := strings.TrimSpace(runGitT(t, f.sibling, "status", "--porcelain")); got != "" {
		t.Fatalf("push-failure dirtied sibling: %q", got)
	}
}

func TestResetSafeCompiledSuccessPushesAndDoesNotAddWorktrees(t *testing.T) {
	binary := buildHerd(t)
	f := newResetSafeRepo(t)
	runGitT(t, f.worktree, "commit", "--allow-empty", "-q", "-m", "unique")
	uniqueSHA := strings.TrimSpace(runGitT(t, f.worktree, "rev-parse", "HEAD"))
	shortSHA := strings.TrimSpace(runGitT(t, f.worktree, "rev-parse", "--short", "HEAD"))
	preserve := "harvest/feature-cli-" + shortSHA
	siblingHead := resetSafeRef(t, f.sibling, "sibling")
	beforeShape := resetSafeShape(t, f.root)

	output, exit := resetSafeCommand(t, binary, f.root, "reset-safe", f.worktree)
	want := "herd-reset-safe: " + f.worktree + " has 1 unmerged commit(s), preserving to " + preserve + " before reset:\n" +
		"  " + uniqueSHA + "\n" +
		"herd-reset-safe: pushed " + preserve + ". Recover with: git cherry-pick <sha>  OR  git merge " + preserve + "\n" +
		"herd-reset-safe: " + f.worktree + " reset to origin/main (" + strings.TrimSpace(runGitT(t, f.root, "rev-parse", "--short", "origin/main")) + ")\n"
	if exit != 0 || output != want {
		t.Fatalf("success result = exit %d, bytes %q, want %q", exit, output, want)
	}
	if got := resetSafeRef(t, f.worktree, preserve); got != uniqueSHA {
		t.Fatalf("local preserve ref = %s, want %s", got, uniqueSHA)
	}
	if got := strings.TrimSpace(runGitT(t, f.remote, "show-ref", "refs/heads/"+preserve)); got == "" {
		t.Fatal("successful command did not push preserve ref")
	}
	if got := resetSafeRef(t, f.worktree, "feature/cli"); got != resetSafeRef(t, f.root, "main") {
		t.Fatalf("successful target did not reset to origin/main: %s", got)
	}
	if got := resetSafeShape(t, f.root); got != beforeShape {
		t.Fatalf("successful command added/removed worktrees: before %q after %q", beforeShape, got)
	}
	if got := resetSafeRef(t, f.sibling, "sibling"); got != siblingHead {
		t.Fatalf("successful command changed sibling ref: %s", got)
	}
	if got := strings.TrimSpace(runGitT(t, f.sibling, "status", "--porcelain")); got != "" {
		t.Fatalf("successful command dirtied sibling: %q", got)
	}
}

func TestResetSafeCompiledSymlinkInputUsesCanonicalAuthorityButExactOutput(t *testing.T) {
	binary := buildHerd(t)
	f := newResetSafeRepo(t)
	link := filepath.Join(t.TempDir(), "worktree-link")
	if err := os.Symlink(f.worktree, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "TASK-PACKET.md"), []byte("packet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if resetSafeCanonical(t, link) == link {
		t.Fatal("fixture did not create a distinct canonical path")
	}
	beforeShape := resetSafeShape(t, f.root)
	newShort := strings.TrimSpace(runGitT(t, f.root, "rev-parse", "--short", "origin/main"))
	output, exit := resetSafeCommand(t, binary, f.root, "reset-safe", link)
	want := "herd-reset-safe: " + link + " (feature/cli) has no unmerged work, safe to reset\n" +
		"herd-reset-safe: " + link + " reset to origin/main (" + newShort + ")\n"
	if exit != 0 || output != want {
		t.Fatalf("symlink result = exit %d, bytes %q, want %q", exit, output, want)
	}
	if got := resetSafeShape(t, f.root); got != beforeShape {
		t.Fatalf("symlink command changed worktree shape: before %q after %q", beforeShape, got)
	}
	if got := resetSafeRef(t, f.worktree, "feature/cli"); got != resetSafeRef(t, f.root, "main") {
		t.Fatalf("symlink target did not reset through canonical worktree: %s", got)
	}
}

func TestResetSafeCompiledFetchesFreshOriginMainBeforePlanning(t *testing.T) {
	binary := buildHerd(t)
	f := newResetSafeRepo(t)
	oldOrigin := resetSafeRevision(t, f.worktree, "origin/main")
	updater := filepath.Join(t.TempDir(), "updater")
	runGitT(t, filepath.Dir(updater), "clone", "-q", f.remote, updater)
	runGitT(t, updater, "checkout", "-q", "main")
	if got := strings.TrimSpace(runGitT(t, updater, "symbolic-ref", "--short", "HEAD")); got != "main" {
		t.Fatalf("updater HEAD branch = %q, want main", got)
	}
	if got, want := resetSafeRevision(t, updater, "HEAD"), resetSafeRevision(t, updater, "refs/heads/main"); got != want {
		t.Fatalf("updater HEAD = %s, want refs/heads/main %s", got, want)
	}
	runGitT(t, updater, "config", "user.email", "reset-safe-updater@test.invalid")
	runGitT(t, updater, "config", "user.name", "Reset Safe Updater")
	if err := os.WriteFile(filepath.Join(updater, "remote.txt"), []byte("new remote main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, updater, "add", "remote.txt")
	runGitT(t, updater, "commit", "-q", "-m", "advance remote main")
	newOrigin := strings.TrimSpace(runGitT(t, updater, "rev-parse", "HEAD"))
	if newOrigin == oldOrigin {
		t.Fatalf("fixture did not advance updater main: still at %s", newOrigin)
	}
	runGitT(t, updater, "push", "-q", "origin", "main")
	if got := resetSafeRevision(t, f.worktree, "origin/main"); got != oldOrigin {
		t.Fatalf("fixture origin/main was not stale before command: %s", got)
	}
	if err := os.WriteFile(filepath.Join(f.worktree, "TASK-PACKET.md"), []byte("packet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newShort := strings.TrimSpace(runGitT(t, updater, "rev-parse", "--short", "HEAD"))
	output, exit := resetSafeCommand(t, binary, f.root, "reset-safe", f.worktree)
	want := "herd-reset-safe: " + f.worktree + " (feature/cli) has no unmerged work, safe to reset\n" +
		"herd-reset-safe: " + f.worktree + " reset to origin/main (" + newShort + ")\n"
	if exit != 0 || output != want {
		t.Fatalf("fresh-origin result = exit %d, bytes %q, want %q", exit, output, want)
	}
	if got := resetSafeRevision(t, f.worktree, "origin/main"); got != newOrigin {
		t.Fatalf("target origin/main was not refreshed: %s, want %s", got, newOrigin)
	}
	if got := resetSafeRef(t, f.worktree, "feature/cli"); got != newOrigin {
		t.Fatalf("target did not reset to freshly fetched origin/main: %s", got)
	}
}

func TestResetSafeCompiledFetchFailureUsesExistingLocalOriginMain(t *testing.T) {
	binary := buildHerd(t)
	f := newResetSafeRepo(t)
	localOrigin := resetSafeRevision(t, f.worktree, "origin/main")
	badRemote := filepath.Join(t.TempDir(), "no-network-origin")
	runGitT(t, f.root, "remote", "set-url", "origin", badRemote)
	if err := os.WriteFile(filepath.Join(f.worktree, "TASK-PACKET.md"), []byte("packet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	localShort := strings.TrimSpace(runGitT(t, f.worktree, "rev-parse", "--short", "origin/main"))
	output, exit := resetSafeCommand(t, binary, f.root, "reset-safe", f.worktree)
	want := "herd-reset-safe: " + f.worktree + " (feature/cli) has no unmerged work, safe to reset\n" +
		"herd-reset-safe: " + f.worktree + " reset to origin/main (" + localShort + ")\n"
	if exit != 0 || output != want {
		t.Fatalf("fetch-failure result = exit %d, bytes %q, want %q", exit, output, want)
	}
	if got := resetSafeRevision(t, f.worktree, "origin/main"); got != localOrigin {
		t.Fatalf("fetch failure changed local origin/main: %s", got)
	}
}
