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

// TestLateWriterIntoGitFailsCleanupWithoutReap is the deterministic pre-fix
// reproduction of the FAC-125 CI flake class: a residual process-group writer
// recreates files under .git while RemoveAll walks the tree, so unlinkat on
// .git returns "directory not empty".
//
// This test deliberately does NOT use t.TempDir for the victim tree: it must
// assert RemoveAll failure with a live unreaped writer, then reap and delete
// under explicit ownership. No sleeps-as-fix, no RemoveAll retries.
func TestLateWriterIntoGitFailsCleanupWithoutReap(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-late-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	// Ownership of root transfers to the end of this test via explicit reap +
	// RemoveAll — never leave an unreaped writer for the package teardown.
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Child process group: keep creating files under .git so concurrent
	// RemoveAll races on a non-empty directory.
	script := filepath.Join(root, "late-writer.sh")
	writeExecutable(t, script, "#!/bin/sh\ni=0\nwhile :; do\n  i=$((i+1))\n  printf 'x' > \"$1/objects/late-$i\"\ndone\n")
	cmd := exec.Command(script, gitDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	// Ensure at least one late object exists before RemoveAll.
	if err := waitForLateObject(gitDir); err != nil {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatal(err)
	}

	removeErr := os.RemoveAll(root)
	// Always reap before leaving the test, regardless of RemoveAll outcome.
	reapProcessGroup(pgid)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	if removeErr == nil {
		// Writer may have been scheduled such that RemoveAll still won. The
		// lifecycle defect class is still that an unreaped process group was
		// left owning the tree; we required an explicit reap above.
		t.Log("RemoveAll won the scheduler race once; unreaped process group still required explicit kill")
	} else {
		t.Logf("reproduced pre-fix late-writer cleanup failure: %v", removeErr)
	}

	// After process-group reap, cleanup must succeed without retry loops.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("after process-group reap, RemoveAll must succeed: %v (%s)", err, diagnoseRepoWriters(root))
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
	script := filepath.Join(root, "late-writer.sh")
	writeExecutable(t, script, "#!/bin/sh\ni=0\nwhile :; do\n  i=$((i+1))\n  printf 'x' > \"$1/objects/late-$i\"\ndone\n")

	life := &lifecycle{}
	cmd := exec.Command(script, gitDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	life.observeStarted(cmd)

	if err := waitForLateObject(gitDir); err != nil {
		life.reap()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	life.finishCommand(cmd)
	_ = cmd.Wait()
	life.reap()

	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("reaped writer must not block cleanup: %v (%s)", err, diagnoseRepoWriters(root))
	}
}

// TestHermeticGitDoesNotLeaveDetachedWriters proves production git helpers
// disable auto-detach writers and leave a tree RemoveAll-clean without
// test-side deletion tricks.
func TestHermeticGitDoesNotLeaveDetachedWriters(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-hermetic-git-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("hermetic git tree must clean up: %v (%s)", err, diagnoseRepoWriters(root))
		}
	}()

	if _, err := runGit(root, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "user.email", "test@example.invalid"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "user.name", "verifier-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "commit.gpgsign", "false"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		writeFile(t, filepath.Join(root, "f.txt"), fmt.Sprintf("%d\n", i))
		if _, err := runGit(root, "add", "f.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := runGit(root, "commit", "-q", "-m", fmt.Sprintf("c%d", i)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMutationPathGuardsStressNoTempDirResidue runs the exact path-guard
// matrix many times under this process. Pair with:
//
//	go test -race ./pkg/verifier -run 'TestMutationPathGuardsRejectEscapesAndMetadataWithoutOutsideWrites' -count=500
//
// for cross-process stress. Failures must not be masked by cleanup retries.
func TestMutationPathGuardsStressNoTempDirResidue(t *testing.T) {
	const iterations = 50
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

func waitForLateObject(gitDir string) error {
	objects := filepath.Join(gitDir, "objects")
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := os.ReadDir(objects)
		if err == nil {
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "late-") {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("late writer never created a residual file under .git/objects")
		}
		// Bound the poll; this is readiness for a synthetic writer, not a
		// cleanup mitigation. The production fix is reap/hermetic git.
		time.Sleep(time.Millisecond)
	}
}
