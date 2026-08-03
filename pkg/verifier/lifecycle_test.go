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

// TestLateWriterIntoGitRequiresExplicitReap proves the ownership defect class:
// an unreaped process-group writer remains live (and may recreate files under
// .git) until processGroupKiller runs. RemoveAll is attempted while unreaped
// only to exercise the concurrent-writer path; the hard assertions are live
// before reap and gone after reap — never a soft t.Log when reproduction is
// weak.
func TestLateWriterIntoGitRequiresExplicitReap(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-late-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", `i=0; while :; do i=$((i+1)); printf x > "$1/objects/late-$i"; done`, "late-writer", gitDir)
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
	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatalf("unreaped writer must be live after creating residue: %v", err)
	}

	// Concurrent RemoveAll while unreaped — may fail with directory-not-empty.
	_ = os.RemoveAll(root)

	// Ownership defect: without explicit reap the group is still live.
	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatalf("unreaped writer must survive RemoveAll until explicit reap: %v", err)
	}

	reapProcessGroup(pgid)
	_ = cmd.Wait()
	if err := waitForPIDGone(pgid, 2*time.Second); err != nil {
		t.Fatalf("after process-group reap, leader must be gone: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("after explicit reap, RemoveAll must succeed: %v", err)
	}
}

// TestProcessGroupReapAllowsTempDirCleanup is the post-fix barrier: the same
// late-writer process group is reaped before cleanup, so RemoveAll succeeds
// without sleeps or retrying deletion.
func TestProcessGroupReapAllowsTempDirCleanup(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-reaped-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `i=0; while :; do i=$((i+1)); printf x > "$1/objects/late-$i"; done`, "late-writer", gitDir)
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
	reapProcessGroup(pgid)
	_ = cmd.Wait()
	if err := waitForPIDGone(pgid, 2*time.Second); err != nil {
		t.Fatalf("reaped writer still live: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("reaped writer must not block cleanup: %v", err)
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
	// Intentional ambient-style repo values that must be overridden by -c.
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
// matrix several times in-process. Fleet acceptance stress is:
//
//	go test -race ./pkg/verifier -run 'TestMutationPathGuardsRejectEscapesAndMetadataWithoutOutsideWrites$' -count=500 -parallel=2
//	go test -race ./pkg/verifier/... -count=100
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
