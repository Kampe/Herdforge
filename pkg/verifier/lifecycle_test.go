package verifier

import (
	"context"
	"errors"
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
	if f.started && !f.reaped && f.pgid > 0 {
		if err := processGroupKiller(f.pgid); err != nil && !isESRCH(err) {
			f.t.Errorf("shutdown reap pgid %d: %v", f.pgid, err)
		}
		if f.cmd != nil && f.cmd.Process != nil {
			if err := f.cmd.Wait(); err != nil && !isExpectedKillWait(err) {
				f.t.Errorf("shutdown wait: %v (stderr=%q)", err, f.stderr.String())
			}
		}
		f.reaped = true
	}
	if f.root != "" {
		if err := os.RemoveAll(f.root); err != nil && !os.IsNotExist(err) {
			// Unseal once more if still sealed permission issues.
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

func (f *lateWriterFixture) reapAndWait() {
	f.t.Helper()
	if f.reaped {
		return
	}
	if err := processGroupKiller(f.pgid); err != nil && !isESRCH(err) {
		f.t.Fatalf("reap process group %d: %v", f.pgid, err)
	}
	if f.cmd == nil || f.cmd.Process == nil {
		f.t.Fatal("reap: nil process")
	}
	waitErr := f.cmd.Wait()
	if waitErr != nil && !isExpectedKillWait(waitErr) {
		f.t.Fatalf("wait after reap: %v (stderr=%q)", waitErr, f.stderr.String())
	}
	if f.cmd.ProcessState == nil {
		f.t.Fatal("wait after reap: missing ProcessState")
	}
	if err := waitForPIDGone(f.pgid, 2*time.Second); err != nil {
		f.t.Fatalf("pgid %d still present after reap+wait: %v", f.pgid, err)
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

func isESRCH(err error) bool {
	return err != nil && errors.Is(err, syscall.ESRCH)
}

func isExpectedKillWait(err error) bool {
	if err == nil {
		return true
	}
	// SIGKILL / signal: killed are expected after process-group reap.
	msg := err.Error()
	return strings.Contains(msg, "signal") || strings.Contains(msg, "kill") || strings.Contains(msg, "killed")
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
// post-reap success, explicit ready handshake, no ignored kill/Wait/RemoveAll.
func TestLateWriterIntoGitRequiresExplicitReap(t *testing.T) {
	f := startLateWriterFixture(t)

	f.seal()

	// PRE-FIX: real os.RemoveAll must fail (never ignored).
	preErr := os.RemoveAll(f.root)
	if preErr == nil {
		t.Fatal("pre-fix: os.RemoveAll must return an error while sealed under unreaped ownership")
	}
	if err := syscall.Kill(f.pgid, 0); err != nil {
		t.Fatalf("unreaped writer must remain live after failed RemoveAll: %v (stderr=%q)", err, f.stderr.String())
	}

	// FIX: reap + wait + unseal, then RemoveAll must succeed.
	f.reapAndWait()
	f.unseal()
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatalf("post-fix: os.RemoveAll must succeed after reap+wait+unseal: %v", err)
	}
	if err := syscall.Kill(f.pgid, 0); err == nil {
		t.Fatalf("process group %d still live after reap+Wait", f.pgid)
	}
	// Root is gone; prevent shutdown RemoveAll noise.
	f.root = ""
}

// TestLateWriterCleanupMutationOmittingReapStillFails: omit reap/wait/unseal
// and assert RemoveAll still fails (non-vacuous negative guard).
func TestLateWriterCleanupMutationOmittingReapStillFails(t *testing.T) {
	f := startLateWriterFixture(t)
	f.seal()

	if err := os.RemoveAll(f.root); err == nil {
		t.Fatal("control: sealed unreaped tree must make os.RemoveAll fail")
	}

	// MUTATION of the fix path: no reapAndWait, no unseal.
	mutErr := os.RemoveAll(f.root)
	if mutErr == nil {
		t.Fatal("mutation: omitting reap/wait/unseal must leave os.RemoveAll failing; got nil")
	}
	if err := syscall.Kill(f.pgid, 0); err != nil {
		t.Fatalf("mutation: writer must still be live without reap: %v", err)
	}
	// Fixture cleanup via t.Cleanup reaps/unseals/removes — no leak.
}

// TestProcessGroupReapAllowsTempDirCleanup: seal fail → reap+unseal success.
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
