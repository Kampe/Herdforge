package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// newTestWorktreeManager creates a manager with a permissive disk policy so
// tests are not blocked by the production 15GB reserve on machines with low
// free space. CI boxes have plenty of disk; this only matters locally.
func newTestWorktreeManager(repoRoot string) *WorktreeManager {
	wm := NewWorktreeManager(repoRoot)
	wm.DiskAdmission = resources.NewCapacityGate(resources.OSBackend{}, resources.DiskPolicy{
		ReserveBytes:   1,
		ReserveInodes:  1,
		RecoveryBytes:  2,
		RecoveryInodes: 1,
	})
	return wm
}

// advanceOriginMain creates a divergent commit on origin/main so the branch
// needs a rebase. It works directly in the repo by using commit-tree to
// create a new commit on top of origin/main, then pushing that SHA directly
// to the remote's main ref. This avoids touching the local main ref (which
// is checked out in the main worktree and cannot be force-updated).
func advanceOriginMain(t *testing.T, repoRoot string) {
	t.Helper()
	tree := gitOut(t, repoRoot, "rev-parse", "origin/main^{tree}")
	parent := gitOut(t, repoRoot, "rev-parse", "origin/main")
	if tree == "" || parent == "" {
		t.Fatalf("advanceOriginMain: cannot resolve origin/main in %s", repoRoot)
	}
	newCommit := gitOut(t, repoRoot, "commit-tree", tree, "-p", parent, "-m", "chore: advance origin/main")
	if newCommit == "" {
		t.Fatal("advanceOriginMain: commit-tree returned empty")
	}
	if err := runCmd(repoRoot, "git", "push", "--quiet", "origin", newCommit+":refs/heads/main"); err != nil {
		t.Fatalf("advanceOriginMain: push %s:main: %v", newCommit[:12], err)
	}
	if err := runCmd(repoRoot, "git", "fetch", "--quiet", "origin", "main"); err != nil {
		t.Fatalf("advanceOriginMain: fetch: %v", err)
	}
}

// gitWithHooks runs a git command with hooks ENABLED. It does NOT
// override core.hooksPath — git uses its default hook-discovery
// mechanism (or local core.hooksPath if set in the repo). Global and
// system config are disabled via the env below so the developer's
// global core.hooksPath cannot interfere.
//
// This exercises the real hook-discovery path that production uses:
// the hook must be installed where git actually looks, not where a
// test fixture redirects it. A test that overrides core.hooksPath to
// force git to find the hook in the wrong directory only proves the
// hook script works — not that git would discover and fire it in
// production, where no such override exists.
func gitWithHooks(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	config := []string{
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "gpg.program=",
		"-c", "gpg.ssh.program=",
		"-c", "credential.helper=",
		"-c", "core.askPass=",
		"-c", "user.signingKey=",
		"-c", "user.name=Herdforge Test",
		"-c", "user.email=herdforge-test@example.invalid",
	}
	cmd := exec.Command("git", append(config, args...)...)
	cmd.Dir = dir
	blocked := map[string]struct{}{
		"GIT_CONFIG_GLOBAL": {}, "GIT_CONFIG_SYSTEM": {}, "GIT_CONFIG_NOSYSTEM": {},
		"GNUPGHOME": {}, "GCM_INTERACTIVE": {}, "GCM_GUI_PROMPT": {},
		"GIT_ASKPASS": {}, "SSH_ASKPASS": {}, "GIT_TERMINAL_PROMPT": {},
		"GIT_EDITOR": {}, "GIT_SEQUENCE_EDITOR": {}, "GIT_PAGER": {}, "PAGER": {},
		"SSH_AUTH_SOCK": {}, "GPG_TTY": {}, "GPG_AGENT_INFO": {},
		"GIT_SSH_COMMAND": {}, "GIT_SSH": {}, "GIT_SSH_VARIANT": {},
		"GIT_CREDENTIAL_HELPER": {}, "SSH_ASKPASS_REQUIRE": {},
	}
	env := make([]string, 0, len(os.Environ())+12)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GIT_CONFIG_") {
			continue
		}
		if _, skip := blocked[key]; !skip {
			env = append(env, item)
		}
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GNUPGHOME=", "GCM_INTERACTIVE=0", "GCM_GUI_PROMPT=0",
		"GIT_ASKPASS=", "SSH_ASKPASS=", "GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=:", "GIT_SEQUENCE_EDITOR=:", "GIT_PAGER=cat", "PAGER=cat",
		"SSH_AUTH_SOCK=", "GPG_TTY=", "GPG_AGENT_INFO=",
		"GIT_SSH_COMMAND=", "GIT_SSH=", "GIT_SSH_VARIANT=",
		"GIT_CREDENTIAL_HELPER=", "SSH_ASKPASS_REQUIRE=never",
	)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), 1
	}
	return string(out), 0
}

// TestInstallPreRebaseHook_CreatesExecutableHook proves the hook file is
// created in the worktree's hooks directory and is executable.
func TestInstallPreRebaseHook_CreatesExecutableHook(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := newTestWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-HK")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	hooksDir, err := wm.worktreeHooksDir(context.Background(), wi.Path)
	if err != nil {
		t.Fatalf("resolve hooks dir: %v", err)
	}
	hookPath := filepath.Join(hooksDir, "pre-rebase")
	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("pre-rebase hook not installed: %v", err)
	}
	if info.Mode()&0100 == 0 {
		t.Fatalf("pre-rebase hook not executable: mode=%v", info.Mode())
	}
	content, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	if !strings.Contains(string(content), "refs/herd/safe/") {
		t.Fatal("hook script does not reference refs/herd/safe/")
	}
}

// TestInstallPreRebaseHook_Idempotent proves re-installation overwrites the
// existing hook without error. A regression that refuses to overwrite would
// break CreateTaskWorktree reattach.
func TestInstallPreRebaseHook_Idempotent(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := newTestWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-HI")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Re-install — must not error.
	if err := wm.InstallPreRebaseHook(context.Background(), wi.Path, "FAC-214-HI"); err != nil {
		t.Fatalf("re-install pre-rebase hook: %v", err)
	}

	// Verify the hook is still there and executable.
	hooksDir, err := wm.worktreeHooksDir(context.Background(), wi.Path)
	if err != nil {
		t.Fatalf("resolve hooks dir: %v", err)
	}
	hookPath := filepath.Join(hooksDir, "pre-rebase")
	if info, err := os.Stat(hookPath); err != nil || info.Mode()&0100 == 0 {
		t.Fatalf("hook missing or not executable after re-install: %v", err)
	}
}

// TestInstallPreRebaseHook_RejectsEmptyInputs proves the fail-closed guards.
func TestInstallPreRebaseHook_RejectsEmptyInputs(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wm := newTestWorktreeManager(tmpDir)

	if err := wm.InstallPreRebaseHook(context.Background(), "", "FAC-214"); err == nil {
		t.Fatal("empty worktree path must fail")
	}
	if err := wm.InstallPreRebaseHook(context.Background(), tmpDir, ""); err == nil {
		t.Fatal("empty task ref must fail")
	}
}

// TestPreRebaseHook_AutoWritesSafeRef is the core FAC-214 regression test
// for the hook. It proves the pre-rebase hook fires when `git rebase` runs
// in the worktree and auto-writes refs/herd/safe/<task> at the pre-rebase
// tip — without the coordinator calling WriteSafeRef.
//
// The test creates a divergent origin/main so the rebase is needed, commits
// real work on the branch, then runs `git rebase origin/main` with hooks
// enabled. The hook must fire BEFORE the rebase starts and capture the
// pre-rebase HEAD.
func TestPreRebaseHook_AutoWritesSafeRef(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := newTestWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-HR")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Lane commits real work.
	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: real work before rebase"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	preRebaseTip := gitOut(t, wi.Path, "rev-parse", "HEAD")
	if preRebaseTip == "" {
		t.Fatal("setup failed: no HEAD after commit")
	}

	// Advance origin/main with a conflicting commit so the rebase is needed.
	advanceOriginMain(t, tmpDir)

	// Delete the safe ref that CreateTaskWorktree wrote at the anchor commit
	// so we can prove the HOOK (not CreateTaskWorktree) wrote the safe ref.
	_ = runCmd(tmpDir, "git", "update-ref", "-d", SafeRefFor("FAC-214-HR"))
	if got := gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-HR")); got != "" {
		t.Fatalf("safe ref should have been deleted, got %s", got)
	}

	// Run git rebase with hooks ENABLED (not testgit.Command which disables
	// them). The rebase will conflict, but the pre-rebase hook fires BEFORE
	// the conflict and writes the safe ref.
	_, rc := gitWithHooks(t, wi.Path, "rebase", "origin/main")
	_ = rc
	// rc may be non-zero (conflict) — that's expected. The hook already ran.

	// The safe ref must now point at the pre-rebase tip, written by the hook.
	safeSHA := gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-HR"))
	if safeSHA == "" {
		t.Fatal("pre-rebase hook did not write the safe ref — hook must fire before rebase starts")
	}
	if safeSHA != preRebaseTip {
		t.Fatalf("safe ref = %s, want pre-rebase tip %s — hook must capture HEAD before rebase rewrites commits", safeSHA, preRebaseTip)
	}
}

// TestPreRebaseHook_SafeRefSurvivesAbortAndReset reproduces the FULL
// destructive sequence from the task packet: rebase conflicts, abort, then
// reset --hard origin/main. The pre-rebase hook must have captured the tip
// before the rebase, and the safe ref must survive the reset so the commits
// are recoverable.
func TestPreRebaseHook_SafeRefSurvivesAbortAndReset(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := newTestWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-HS")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Lane commits real work.
	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: work that would be lost"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	preRebaseTip := gitOut(t, wi.Path, "rev-parse", "HEAD")

	// Advance origin/main to create divergence.
	advanceOriginMain(t, tmpDir)

	// Delete the initial safe ref so we prove the hook wrote it.
	_ = runCmd(tmpDir, "git", "update-ref", "-d", SafeRefFor("FAC-214-HS"))

	// Run rebase with hooks enabled — hook fires and writes safe ref.
	_, _ = gitWithHooks(t, wi.Path, "rebase", "origin/main")

	safeSHA := gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-HS"))
	if safeSHA != preRebaseTip {
		t.Fatalf("safe ref = %s, want pre-rebase tip %s", safeSHA, preRebaseTip)
	}

	// Destructive sequence: rebase --abort then reset --hard origin/main.
	_, _ = gitWithHooks(t, wi.Path, "rebase", "--abort")
	originMain := gitOut(t, wi.Path, "rev-parse", "origin/main")
	_, _ = gitWithHooks(t, wi.Path, "reset", "--hard", "origin/main")

	headAfter := gitOut(t, wi.Path, "rev-parse", "HEAD")
	if headAfter != originMain {
		t.Fatalf("reset did not move HEAD to origin/main: HEAD=%s origin/main=%s", headAfter, originMain)
	}

	// The safe ref must still hold the pre-rebase tip.
	safeSHA = gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-HS"))
	if safeSHA != preRebaseTip {
		t.Fatalf("safe ref = %s after destructive reset, want pre-rebase tip %s — safe ref must survive", safeSHA, preRebaseTip)
	}

	// The real work must still be reachable from the safe ref.
	subject := gitOut(t, tmpDir, "log", "-1", "--format=%s", SafeRefFor("FAC-214-HS"))
	if !strings.Contains(subject, "work that would be lost") {
		t.Fatalf("safe ref tip subject = %q, expected the lost work commit", subject)
	}

	// DetectDroppedWork must alarm.
	report, err := wm.DetectDroppedWork(context.Background(), "FAC-214-HS", headAfter, originMain)
	if err != nil {
		t.Fatalf("DetectDroppedWork: %v", err)
	}
	if !report.Dropped {
		t.Fatal("DetectDroppedWork must report Dropped=true after the destructive sequence")
	}
	if !report.Recoverable {
		t.Fatal("DetectDroppedWork must report Recoverable=true — safe ref holds the commits")
	}
}

// TestPreRebaseHook_DoesNotBlockRebase proves the hook always exits 0 and
// never blocks a rebase. A regression that makes the hook exit non-zero
// would prevent lanes from rebasing at all.
func TestPreRebaseHook_DoesNotBlockRebase(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := newTestWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-HB")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	// Commit work on the branch.
	if err := runCmd(wi.Path, "git", "commit", "--allow-empty", "-q", "-m", "feat: work"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Advance origin/main with a non-conflicting commit (fast-forwardable
	// from the branch's perspective after rebase).
	advanceOriginMain(t, tmpDir)

	// Run rebase with hooks enabled. The hook must not block it.
	out, rc := gitWithHooks(t, wi.Path, "rebase", "origin/main")
	if rc != 0 {
		t.Fatalf("pre-rebase hook blocked the rebase (rc=%d): %s — hook must always exit 0", rc, out)
	}

	// Rebase succeeded: HEAD should be on top of origin/main.
	headSubject := gitOut(t, wi.Path, "log", "-1", "--format=%s")
	if !strings.Contains(headSubject, "feat: work") {
		t.Fatalf("after rebase, HEAD subject = %q, expected the lane's work commit", headSubject)
	}
}

// TestCreateTaskWorktree_InstallsPreRebaseHook proves CreateTaskWorktree
// installs the hook as part of worktree creation. A regression that removes
// the InstallPreRebaseHook call from CreateTaskWorktree would fail here.
func TestCreateTaskWorktree_InstallsPreRebaseHook(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	wm := newTestWorktreeManager(tmpDir)
	wi, err := wm.CreateTaskWorktree(context.Background(), "FAC-214-HC")
	if err != nil {
		t.Fatalf("CreateTaskWorktree: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(wi.Path) })

	hooksDir, err := wm.worktreeHooksDir(context.Background(), wi.Path)
	if err != nil {
		t.Fatalf("resolve hooks dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hooksDir, "pre-rebase")); err != nil {
		t.Fatalf("CreateTaskWorktree did not install pre-rebase hook: %v", err)
	}
}

// TestPreRebaseHook_HandlesNonHerdBranch proves the hook is a no-op (exits 0,
// writes no safe ref) when the current branch is not a herd/* branch. This
// prevents the hook from interfering with non-task worktrees.
func TestPreRebaseHook_HandlesNonHerdBranch(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)

	// Create a non-herd branch worktree manually.
	nonHerdWT := filepath.Join(tmpDir, "worktrees", "feature-x")
	if err := os.MkdirAll(filepath.Dir(nonHerdWT), 0755); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(tmpDir, "git", "worktree", "add", "-b", "feature-x", nonHerdWT, "HEAD"); err != nil {
		t.Fatalf("create non-herd worktree: %v", err)
	}
	t.Cleanup(func() {
		_ = runCmd(tmpDir, "git", "worktree", "remove", "--force", nonHerdWT)
	})

	// Install the hook in this worktree (it's the same script regardless of branch).
	wm := newTestWorktreeManager(tmpDir)
	if err := wm.InstallPreRebaseHook(context.Background(), nonHerdWT, "FAC-214-HN"); err != nil {
		t.Fatalf("InstallPreRebaseHook: %v", err)
	}

	// Commit work on the non-herd branch.
	if err := runCmd(nonHerdWT, "git", "commit", "--allow-empty", "-q", "-m", "feat: non-herd work"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Advance origin/main.
	advanceOriginMain(t, tmpDir)

	// Run rebase — hook fires but should NOT write a safe ref (non-herd branch).
	out, rc := gitWithHooks(t, nonHerdWT, "rebase", "origin/main")
	if rc != 0 {
		t.Fatalf("rebase failed on non-herd branch (rc=%d): %s", rc, out)
	}

	// No safe ref should exist for the task (the hook is a no-op on non-herd branches).
	safeSHA := gitOut(t, tmpDir, "rev-parse", "--verify", SafeRefFor("FAC-214-HN"))
	if safeSHA != "" {
		t.Fatalf("safe ref should not exist for non-herd branch, got %s", safeSHA)
	}
}
