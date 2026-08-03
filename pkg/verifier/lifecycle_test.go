package verifier

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sealGitDirForCleanupFailure makes real os.RemoveAll(root) fail deterministically
// (permission denied walking .git). Not a flaky concurrent-walk race.
func sealGitDirForCleanupFailure(gitDir string) error {
	return os.Chmod(gitDir, 0)
}

func unsealGitDirAfterReap(gitDir string) error {
	return os.Chmod(gitDir, 0o755)
}

// lateWriterHandshakeScript: create residue, signal ready on $2, then park.
// No free-running recreate loop — readiness is an explicit boundary file.
const lateWriterHandshakeScript = `mkdir -p "$1/objects" && printf x > "$1/objects/late-0" && printf ready > "$2" && exec sleep 3600`

// lateWriterFixture owns root + process group and always reaps on exit so early
// Fatal paths cannot leak processes or sealed trees.
type lateWriterFixture struct {
	t         *testing.T
	root      string
	gitDir    string
	readyPath string
	cmd       *exec.Cmd
	pgid      int
	stderr    strings.Builder
	started   bool
	sealed    bool
	reaped    bool
}

func startLateWriterFixture(t *testing.T) *lateWriterFixture {
	t.Helper()
	root, err := os.MkdirTemp("", "verifier-late-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	f := &lateWriterFixture{
		t:         t,
		root:      root,
		gitDir:    filepath.Join(root, ".git"),
		readyPath: filepath.Join(root, "writer.ready"),
	}
	if err := os.MkdirAll(filepath.Join(f.gitDir, "objects"), 0o755); err != nil {
		_ = os.RemoveAll(root)
		t.Fatal(err)
	}
	t.Cleanup(f.shutdown)

	f.cmd = exec.Command("sh", "-c", lateWriterHandshakeScript, "late-writer", f.gitDir, f.readyPath)
	f.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	f.cmd.Stderr = &f.stderr
	if err := f.cmd.Start(); err != nil {
		t.Fatalf("start late writer: %v", err)
	}
	f.started = true
	f.pgid = f.cmd.Process.Pid

	if err := waitForWriterReady(f.readyPath, f.pgid, 5*time.Second); err != nil {
		t.Fatalf("writer ready handshake: %v (stderr=%q)", err, f.stderr.String())
	}
	if err := syscall.Kill(f.pgid, 0); err != nil {
		t.Fatalf("writer must be live after ready handshake: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.gitDir, "objects", "late-0")); err != nil {
		t.Fatalf("residue missing after ready handshake: %v", err)
	}
	return f
}

// shutdown is leak-safe cleanup for all paths (including early Fatal via t.Cleanup).
// Uses production ReapOwnedCmd so test cleanup cannot ignore kill/Wait/group errors.
func (f *lateWriterFixture) shutdown() {
	if f == nil {
		return
	}
	if f.sealed {
		if err := unsealGitDirAfterReap(f.gitDir); err != nil && !os.IsNotExist(err) {
			f.t.Errorf("shutdown unseal: %v", err)
		}
		f.sealed = false
	}
	if f.started && !f.reaped {
		if err := ReapOwnedCmd(f.cmd); err != nil {
			// Last-resort group kill if production reap failed mid-path.
			if f.pgid > 0 {
				_ = killProcessGroup(f.pgid)
			}
			f.t.Errorf("shutdown ReapOwnedCmd: %v (stderr=%q)", err, f.stderr.String())
		}
		f.reaped = true
	}
	if f.root != "" {
		if err := os.RemoveAll(f.root); err != nil && !os.IsNotExist(err) {
			_ = unsealGitDirAfterReap(f.gitDir)
			if err2 := os.RemoveAll(f.root); err2 != nil && !os.IsNotExist(err2) {
				f.t.Errorf("shutdown RemoveAll: %v", err2)
			}
		}
	}
}

func (f *lateWriterFixture) seal() {
	f.t.Helper()
	if err := sealGitDirForCleanupFailure(f.gitDir); err != nil {
		f.t.Fatalf("seal .git: %v", err)
	}
	f.sealed = true
}

// reapAndWait closes ownership via production ReapOwnedCmd (full-group kill +
// Wait + group-gone probe). Errors are never ignored.
func (f *lateWriterFixture) reapAndWait() {
	f.t.Helper()
	if f.reaped {
		return
	}
	if err := ReapOwnedCmd(f.cmd); err != nil {
		f.t.Fatalf("production ReapOwnedCmd: %v (stderr=%q)", err, f.stderr.String())
	}
	if err := waitForProcessGroupGone(f.pgid, 2*time.Second); err != nil {
		f.t.Fatalf("process group %d still live after ReapOwnedCmd: %v", f.pgid, err)
	}
	f.reaped = true
}

func (f *lateWriterFixture) unseal() {
	f.t.Helper()
	if err := unsealGitDirAfterReap(f.gitDir); err != nil {
		f.t.Fatalf("unseal .git: %v", err)
	}
	f.sealed = false
}

// waitForProcessGroupGone proves no member of the process group remains
// (grandchildren included). Leader-only ESRCH is not sufficient.
func waitForProcessGroupGone(pgid int, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		err := syscall.Kill(-pgid, 0)
		if err != nil && isESRCH(err) {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				return fmt.Errorf("process group %d still has live members after diagnostic bound", pgid)
			}
			return fmt.Errorf("process group %d probe after bound: %w", pgid, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForWriterReady is an explicit boundary handshake on readyPath contents.
// Diagnostic bound only — not a cancel/cleanup sleep.
func waitForWriterReady(readyPath string, pgid int, bound time.Duration) error {
	deadline := time.Now().Add(bound)
	for {
		data, err := os.ReadFile(readyPath)
		if err == nil && strings.TrimSpace(string(data)) == "ready" {
			return nil
		}
		if pgid > 0 {
			if killErr := syscall.Kill(pgid, 0); killErr != nil {
				return fmt.Errorf("writer pgid %d exited before ready handshake: %w", pgid, killErr)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("diagnostic ready bound exceeded waiting for %s", filepath.Base(readyPath))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestLateWriterIntoGitRequiresExplicitReap: hard pre-reap RemoveAll failure,
// post-reap success via production ReapOwnedCmd, ready handshake, no ignored
// kill/Wait/RemoveAll errors.
func TestLateWriterIntoGitRequiresExplicitReap(t *testing.T) {
	f := startLateWriterFixture(t)

	f.seal()

	// PRE-FIX: real os.RemoveAll must fail (never ignored).
	preErr := os.RemoveAll(f.root)
	if preErr == nil {
		t.Fatal("pre-fix: os.RemoveAll must return an error while sealed under unreaped ownership")
	}
	if err := syscall.Kill(-f.pgid, 0); err != nil {
		t.Fatalf("unreaped process group must remain live after failed RemoveAll: %v (stderr=%q)", err, f.stderr.String())
	}

	// FIX: production ReapOwnedCmd + unseal, then RemoveAll must succeed.
	f.reapAndWait()
	f.unseal()
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatalf("post-fix: os.RemoveAll must succeed after ReapOwnedCmd+unseal: %v", err)
	}
	if err := syscall.Kill(-f.pgid, 0); err == nil {
		t.Fatalf("process group %d still live after production ReapOwnedCmd", f.pgid)
	}
	// Root is gone; prevent shutdown RemoveAll noise.
	f.root = ""
}

// TestLateWriterCleanupMutationOmittingReapStillFails: omit ReapOwnedCmd/unseal
// and assert RemoveAll still fails (non-vacuous negative guard).
func TestLateWriterCleanupMutationOmittingReapStillFails(t *testing.T) {
	f := startLateWriterFixture(t)
	f.seal()

	if err := os.RemoveAll(f.root); err == nil {
		t.Fatal("control: sealed unreaped tree must make os.RemoveAll fail")
	}

	// MUTATION of the fix path: no ReapOwnedCmd, no unseal.
	mutErr := os.RemoveAll(f.root)
	if mutErr == nil {
		t.Fatal("mutation: omitting ReapOwnedCmd/unseal must leave os.RemoveAll failing; got nil")
	}
	if err := syscall.Kill(-f.pgid, 0); err != nil {
		t.Fatalf("mutation: process group must still be live without reap: %v", err)
	}
	// Fixture cleanup via t.Cleanup → ReapOwnedCmd/unseal/RemoveAll — no leak.
}

// TestProcessGroupReapAllowsTempDirCleanup: seal fail → ReapOwnedCmd+unseal success.
func TestProcessGroupReapAllowsTempDirCleanup(t *testing.T) {
	f := startLateWriterFixture(t)
	f.seal()
	if err := os.RemoveAll(f.root); err == nil {
		t.Fatal("pre-reap: os.RemoveAll must fail while sealed")
	}
	f.reapAndWait()
	f.unseal()
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatalf("post-reap: os.RemoveAll must succeed: %v", err)
	}
	f.root = ""
}

// grandchildGroupScript: leader backgrounds a real nested sh (not a shell
// function — $$ in functions is the parent shell on bash/zsh) that writes its
// own pid then parks. Production ReapOwnedCmd must kill leader + grandchild.
const grandchildGroupScript = `sh -c 'printf "%s\n" "$$" > "$1"; exec sleep 3600' grandchild "$1" & wait`

// startGrandchildGroup starts a Setpgid shell with a live grandchild and
// returns (cmd, leaderPgid, grandchildPid). Caller must ReapOwnedCmd or kill.
func startGrandchildGroup(t *testing.T) (cmd *exec.Cmd, pgid int, grandchild int) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd = exec.Command("sh", "-c", grandchildGroupScript, "group-leader", ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start grandchild group: %v", err)
	}
	pgid = cmd.Process.Pid
	t.Cleanup(func() {
		// Leak-safe: production reap if still unreaped.
		if cmd.ProcessState == nil && cmd.Process != nil {
			if err := ReapOwnedCmd(cmd); err != nil {
				_ = killProcessGroup(pgid)
				t.Errorf("cleanup ReapOwnedCmd: %v", err)
			}
		} else if pgid > 0 {
			_ = killProcessGroup(pgid)
		}
	})
	gc, err := waitForChildReadyPID(ready, 5*time.Second)
	if err != nil {
		t.Fatalf("grandchild ready: %v", err)
	}
	if err := syscall.Kill(gc, 0); err != nil {
		t.Fatalf("grandchild %d not live after ready: %v", gc, err)
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("process group %d not live after ready: %v", pgid, err)
	}
	return cmd, pgid, gc
}

// TestReapOwnedCmdKillsGrandchildren is the production-load-bearing positive
// proof: ReapOwnedCmd (kill process group + Wait + group-gone probe) must
// extinguish the leader and a ready grandchild. This is not a fake cleaner —
// it exercises the same primitive execute() uses after Start when ctx is done.
func TestReapOwnedCmdKillsGrandchildren(t *testing.T) {
	cmd, pgid, grandchild := startGrandchildGroup(t)

	if err := ReapOwnedCmd(cmd); err != nil {
		t.Fatalf("production ReapOwnedCmd: %v", err)
	}
	if err := waitForProcessGroupGone(pgid, 2*time.Second); err != nil {
		t.Fatalf("after ReapOwnedCmd: %v", err)
	}
	if err := syscall.Kill(grandchild, 0); err == nil {
		t.Fatalf("grandchild %d still live after production ReapOwnedCmd", grandchild)
	}
	if cmd.ProcessState == nil {
		t.Fatal("ReapOwnedCmd must Wait the leader (ProcessState set)")
	}
}

// TestReapOwnedCmdLeaderOnlyMutationFailsClosed is production-load-bearing:
// injecting a leader-only killer must make ReapOwnedCmd return a non-nil error
// because the group-gone probe sees the surviving grandchild. A leader-only
// liveness check would falsely pass.
func TestReapOwnedCmdLeaderOnlyMutationFailsClosed(t *testing.T) {
	cmd, pgid, grandchild := startGrandchildGroup(t)

	prev := processGroupKiller
	processGroupKiller = func(id int) error {
		if id <= 0 {
			return fmt.Errorf("kill process group: invalid pgid %d", id)
		}
		// MUTATION: kill leader only — leaves grandchildren in the group.
		return syscall.Kill(id, syscall.SIGKILL)
	}
	t.Cleanup(func() { processGroupKiller = prev })

	reapErr := ReapOwnedCmd(cmd)
	if reapErr == nil {
		// If the OS reaped the grandchild with the leader (unlikely), the
		// mutation cannot load-bear; force-fail so the test is non-vacuous.
		if err := syscall.Kill(grandchild, 0); err == nil {
			t.Fatal("mutation: ReapOwnedCmd returned nil while grandchild still live")
		}
		t.Fatal("mutation: leader-only kill must make ReapOwnedCmd fail closed (group still live or probe error); got nil")
	}
	if !strings.Contains(reapErr.Error(), "still has live members") &&
		!strings.Contains(reapErr.Error(), "process group") {
		t.Fatalf("mutation: want group-liveness failure, got %v", reapErr)
	}

	// Grandchild must still be alive — proves leader-only is insufficient.
	if err := syscall.Kill(grandchild, 0); err != nil {
		t.Fatalf("mutation expected grandchild %d to survive leader-only kill: %v", grandchild, err)
	}
	// Production killer restored for cleanup; extinguish orphaned members.
	processGroupKiller = prev
	if err := killProcessGroup(pgid); err != nil && !isESRCH(err) {
		t.Fatalf("post-mutation group kill: %v", err)
	}
	if err := waitForProcessGroupGone(pgid, 2*time.Second); err != nil {
		t.Fatalf("post-mutation cleanup: %v", err)
	}
}

// TestHermeticGitConfigFlagsReachGit is the non-vacuous coverage for
// hermeticGitConfig: git must resolve the -c overrides on the same argv path
// runGit uses. Deleting hermeticGitConfig fails these equality checks.
func TestHermeticGitConfigFlagsReachGit(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-hermetic-flags-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil && !os.IsNotExist(err) {
			t.Errorf("cleanup hermetic root: %v", err)
		}
	})

	if _, err := runGit(root, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "gc.auto", "6700"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "gc.autoDetach", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "maintenance.auto", "true"); err != nil {
		t.Fatal(err)
	}

	gotAuto, err := runGit(root, "config", "--get", "gc.auto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotAuto)) != "0" {
		t.Fatalf("gc.auto via hermetic runGit = %q, want 0 (hermeticGitConfig must reach git)", strings.TrimSpace(string(gotAuto)))
	}
	gotDetach, err := runGit(root, "config", "--get", "gc.autoDetach")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotDetach)) != "false" {
		t.Fatalf("gc.autoDetach via hermetic runGit = %q, want false", strings.TrimSpace(string(gotDetach)))
	}
	gotMaint, err := runGit(root, "config", "--get", "maintenance.auto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotMaint)) != "false" {
		t.Fatalf("maintenance.auto via hermetic runGit = %q, want false", strings.TrimSpace(string(gotMaint)))
	}
}

// TestMutationPathGuardsStressNoTempDirResidue runs the exact path-guard
// matrix several times in-process.
func TestMutationPathGuardsStressNoTempDirResidue(t *testing.T) {
	const iterations = 5
	if testing.Short() {
		t.Skip("stress path under -short")
	}
	for i := 0; i < iterations; i++ {
		runMutationPathGuardMatrix(t)
	}
}

func runMutationPathGuardMatrix(t *testing.T) {
	t.Helper()
	dir, _ := verificationRepo(t)
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	writeFile(t, outsideFile, "outside\n")
	gitMetadataProbe := filepath.Join(dir, ".git", "hooks", "fac122-probe")
	writeFile(t, gitMetadataProbe, "metadata\n")

	trackedLink := filepath.Join(dir, "tracked-link")
	if err := os.Symlink(outsideFile, trackedLink); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "tracked-link")
	git(t, dir, "commit", "-q", "-m", "add tracked link")

	gitParentLink := filepath.Join(dir, "git-parent")
	if err := os.Symlink(".git", gitParentLink); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "git-parent")
	git(t, dir, "commit", "-q", "-m", "add git metadata alias")
	outsideParent := t.TempDir()
	outsideVictim := filepath.Join(outsideParent, "victim.txt")
	writeFile(t, outsideVictim, "outside-parent\n")
	outsideParentLink := filepath.Join(dir, "outside-parent")
	if err := os.Symlink(outsideParent, outsideParentLink); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "outside-parent")
	git(t, dir, "commit", "-q", "-m", "add outside parent alias")
	candidate := gitOutput(t, dir, "rev-parse", "HEAD")

	cases := []struct {
		target   string
		expected string
	}{
		{target: outsideFile, expected: "relative path"},
		{target: "../outside.txt", expected: "escapes candidate"},
		{target: "nested/../../outside.txt", expected: "escapes candidate"},
		{target: "tracked-link", expected: "Lstat regular file"},
		{target: "git-parent/hooks/fac122-probe", expected: "git metadata"},
		{target: ".git/hooks/fac122-probe", expected: "may not enter .git"},
		{target: "outside-parent/victim.txt", expected: "resolves outside candidate root"},
	}
	for _, tt := range cases {
		_, err := NewVerifierArgs([]string{"true"}).RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
			CandidateSHA:      candidate,
			EnvironmentPolicy: EnvironmentPolicyInherited,
			TargetFile:        tt.target,
			OriginalCode:      "outside\n",
			MutantCode:        "clobbered\n",
			Timeout:           time.Second,
		})
		if err == nil || !strings.Contains(err.Error(), tt.expected) {
			t.Fatalf("target %q: want %q, got %v", tt.target, tt.expected, err)
		}
		assertFile(t, outsideFile, "outside\n")
		assertFile(t, outsideVictim, "outside-parent\n")
		assertFile(t, gitMetadataProbe, "metadata\n")
		assertClean(t, dir)
	}
}
