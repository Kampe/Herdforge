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
// by clearing all mode bits on .git (permission denied while walking). This is
// the pre-fix TempDir cleanup barrier — not a flaky concurrent-walk race and
// not an ignored RemoveAll result.
func sealGitDirForCleanupFailure(gitDir string) error {
	return os.Chmod(gitDir, 0)
}

func unsealGitDirAfterReap(gitDir string) error {
	return os.Chmod(gitDir, 0o755)
}

// lateWriterParkScript creates one residue file under .git/objects then parks
// in the process group. It does NOT rely on recreating removed parents after
// RemoveAll (that path is vacuous when RemoveAll wins). Parking keeps the
// unreaped group live through the sealed RemoveAll failure.
const lateWriterParkScript = `mkdir -p "$1/objects" && printf x > "$1/objects/late-0" && exec sleep 3600`

// TestLateWriterIntoGitRequiresExplicitReap is the FAC-151 deterministic
// pre-fix / post-fix cleanup barrier:
//
//  1. Unreaped process-group writer creates .git residue and parks.
//  2. Seal .git so real os.RemoveAll hard-fails (assert err != nil — never ignored).
//  3. After process-group reap + wait + unseal, os.RemoveAll must succeed.
func TestLateWriterIntoGitRequiresExplicitReap(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-late-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", lateWriterParkScript, "late-writer", gitDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var writerErr strings.Builder
	cmd.Stderr = &writerErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	if err := waitForLateObject(gitDir, pgid); err != nil {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatalf("writer failed to create residue: %v (stderr=%q)", err, writerErr.String())
	}
	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatalf("unreaped writer must be live after creating residue: %v", err)
	}

	if err := sealGitDirForCleanupFailure(gitDir); err != nil {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatalf("seal tree: %v", err)
	}

	// PRE-FIX: real os.RemoveAll must fail. Never `_ = os.RemoveAll`.
	preErr := os.RemoveAll(root)
	if preErr == nil {
		_ = unsealGitDirAfterReap(gitDir)
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatal("pre-fix: os.RemoveAll must return an error while the tree is sealed under unreaped ownership")
	}
	if err := syscall.Kill(pgid, 0); err != nil {
		_ = unsealGitDirAfterReap(gitDir)
		t.Fatalf("unreaped writer must remain live after failed RemoveAll: %v (stderr=%q)", err, writerErr.String())
	}

	// FIX: reap + wait + unseal, then RemoveAll succeeds.
	reapProcessGroup(pgid)
	_ = cmd.Wait()
	if err := waitForPIDGone(pgid, 2*time.Second); err != nil {
		_ = unsealGitDirAfterReap(gitDir)
		t.Fatalf("after process-group reap, leader must be gone: %v", err)
	}
	if err := unsealGitDirAfterReap(gitDir); err != nil {
		t.Fatalf("post-fix: unseal after reap must succeed: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("post-fix: os.RemoveAll must succeed after reap+wait+unseal: %v", err)
	}
	if err := syscall.Kill(pgid, 0); err == nil {
		t.Fatalf("process group %d still live after reap+Wait", pgid)
	}
}

// TestLateWriterCleanupMutationOmittingReapStillFails proves that deleting
// reap/wait/unseal from the fix path leaves os.RemoveAll failing — the
// negative guard is non-vacuous.
func TestLateWriterCleanupMutationOmittingReapStillFails(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-late-writer-mut-*")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", lateWriterParkScript, "late-writer", gitDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		_ = unsealGitDirAfterReap(gitDir)
		_ = os.RemoveAll(root)
	})

	if err := waitForLateObject(gitDir, pgid); err != nil {
		t.Fatal(err)
	}
	if err := sealGitDirForCleanupFailure(gitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err == nil {
		t.Fatal("control: sealed unreaped tree must make os.RemoveAll fail")
	}

	// MUTATION: omit reapProcessGroup, Wait, and unseal.
	mutErr := os.RemoveAll(root)
	if mutErr == nil {
		t.Fatal("mutation: removing reap/wait/unseal must leave os.RemoveAll failing; got nil")
	}
	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatalf("mutation path: writer must still be live without reap: %v", err)
	}
}

// TestProcessGroupReapAllowsTempDirCleanup: seal → RemoveAll fails; reap+unseal → succeeds.
func TestProcessGroupReapAllowsTempDirCleanup(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-reaped-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", lateWriterParkScript, "late-writer", gitDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	if err := waitForLateObject(gitDir, pgid); err != nil {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := sealGitDirForCleanupFailure(gitDir); err != nil {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err == nil {
		_ = unsealGitDirAfterReap(gitDir)
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatal("pre-reap: os.RemoveAll must fail while sealed")
	}
	reapProcessGroup(pgid)
	_ = cmd.Wait()
	if err := waitForPIDGone(pgid, 2*time.Second); err != nil {
		_ = unsealGitDirAfterReap(gitDir)
		t.Fatalf("reaped writer still live: %v", err)
	}
	if err := unsealGitDirAfterReap(gitDir); err != nil {
		t.Fatalf("unseal after reap: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("post-reap: os.RemoveAll must succeed: %v", err)
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
	t.Cleanup(func() { _ = os.RemoveAll(root) })

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

func waitForLateObject(gitDir string, pgid int) error {
	objects := filepath.Join(gitDir, "objects")
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(objects)
		if err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "late-") {
					return nil
				}
			}
		}
		if pgid > 0 {
			if err := syscall.Kill(pgid, 0); err != nil {
				return fmt.Errorf("late writer process group %d exited before creating residue: %w", pgid, err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("late writer never created a residual file under .git/objects")
		}
		time.Sleep(time.Millisecond)
	}
}
