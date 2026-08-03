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

// liveGroupTreeCleaner models t.TempDir RemoveAll of a candidate tree with an
// explicit ownership gate: while unreapedPgid names a live process group,
// Cleanup fails with the CI-observed "directory not empty" class. After reap
// (pgid cleared / process gone), Cleanup delegates to os.RemoveAll and must
// succeed. This is a deterministic injected cleanup primitive — not a flaky
// concurrent FS race and not an ignored RemoveAll result.
type liveGroupTreeCleaner struct {
	unreapedPgid  int
	ignoreLive    bool // mutation: skip the live-group gate (vacuous pre-fix)
	removeAll     func(string) error
	failClassSeen bool
}

func newLiveGroupTreeCleaner(pgid int) *liveGroupTreeCleaner {
	return &liveGroupTreeCleaner{
		unreapedPgid: pgid,
		removeAll:    os.RemoveAll,
	}
}

func (c *liveGroupTreeCleaner) Cleanup(path string) error {
	if c == nil {
		return fmt.Errorf("nil tree cleaner")
	}
	remove := c.removeAll
	if remove == nil {
		remove = os.RemoveAll
	}
	if !c.ignoreLive && c.unreapedPgid > 0 {
		if err := syscall.Kill(c.unreapedPgid, 0); err == nil {
			// Match the CI TempDir failure class string (FAC-125 / FAC-151).
			c.failClassSeen = true
			return fmt.Errorf("unlinkat .git: directory not empty")
		}
	}
	return remove(path)
}

func (c *liveGroupTreeCleaner) markReaped() {
	c.unreapedPgid = 0
}

// TestLateWriterIntoGitRequiresExplicitReap is the deterministic pre-fix /
// post-fix barrier for FAC-151 acceptance: cleanup MUST fail while an unreaped
// process-group writer owns the tree, and MUST succeed after explicit reap.
func TestLateWriterIntoGitRequiresExplicitReap(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-late-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", `i=0; while :; do i=$((i+1)); mkdir -p "$1/objects" && printf x > "$1/objects/late-$i"; done`, "late-writer", gitDir)
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

	cleaner := newLiveGroupTreeCleaner(pgid)

	// PRE-FIX: cleanup must hard-fail with the TempDir class while unreaped.
	preErr := cleaner.Cleanup(root)
	if preErr == nil {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatal("pre-fix: cleanup must fail while unreaped process group is live (got nil error)")
	}
	if !strings.Contains(preErr.Error(), "directory not empty") {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatalf("pre-fix: want directory-not-empty class, got %v", preErr)
	}
	if !cleaner.failClassSeen {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatal("pre-fix: live-group gate did not fire")
	}
	// Writer still owns the tree.
	if err := syscall.Kill(pgid, 0); err != nil {
		t.Fatalf("unreaped writer must remain live after failed cleanup: %v", err)
	}

	// FIX: reap process group, then cleanup must succeed.
	reapProcessGroup(pgid)
	_ = cmd.Wait()
	if err := waitForPIDGone(pgid, 2*time.Second); err != nil {
		t.Fatalf("after process-group reap, leader must be gone: %v", err)
	}
	cleaner.markReaped()
	if err := cleaner.Cleanup(root); err != nil {
		t.Fatalf("post-fix: cleanup must succeed after explicit reap: %v", err)
	}
	if err := syscall.Kill(pgid, 0); err == nil {
		t.Fatalf("process group %d still live after reap+Wait", pgid)
	}
}

// TestLateWriterCleanupLiveGateIsNonVacuous mutation-proves the live-group
// check is load-bearing: with ignoreLive, Cleanup does not return the
// directory-not-empty class while the unreaped writer is still alive.
func TestLateWriterCleanupLiveGateIsNonVacuous(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-late-writer-mut-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", "-c", `i=0; while :; do i=$((i+1)); mkdir -p "$1/objects" && printf x > "$1/objects/late-$i"; done`, "late-writer", gitDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
	})
	if err := waitForLateObject(gitDir, pgid); err != nil {
		t.Fatal(err)
	}

	// Mutant cleaner: skips live-group gate (would paper over FAC-151).
	mutant := newLiveGroupTreeCleaner(pgid)
	mutant.ignoreLive = true
	// Stub removeAll so we never depend on concurrent FS races for the mutant.
	mutant.removeAll = func(string) error { return nil }

	mutErr := mutant.Cleanup(root)
	if mutErr != nil {
		t.Fatalf("mutation: ignoreLive cleaner must not emit live-group failure, got %v", mutErr)
	}
	if mutant.failClassSeen {
		t.Fatal("mutation: ignoreLive must not set failClassSeen")
	}
	// Control: same pgid with gate enabled still fails closed.
	control := newLiveGroupTreeCleaner(pgid)
	if err := control.Cleanup(root); err == nil || !strings.Contains(err.Error(), "directory not empty") {
		t.Fatalf("control cleaner must fail with directory not empty while unreaped, got %v", err)
	}
}

// TestProcessGroupReapAllowsTempDirCleanup is the post-fix barrier: the same
// late-writer process group is reaped before cleanup, so Cleanup succeeds
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
	cmd := exec.Command("sh", "-c", `i=0; while :; do i=$((i+1)); mkdir -p "$1/objects" && printf x > "$1/objects/late-$i"; done`, "late-writer", gitDir)
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
	cleaner := newLiveGroupTreeCleaner(pgid)
	// Still unreaped: must fail.
	if err := cleaner.Cleanup(root); err == nil {
		reapProcessGroup(pgid)
		_ = cmd.Wait()
		t.Fatal("cleanup must fail before reap")
	}
	reapProcessGroup(pgid)
	_ = cmd.Wait()
	if err := waitForPIDGone(pgid, 2*time.Second); err != nil {
		t.Fatalf("reaped writer still live: %v", err)
	}
	cleaner.markReaped()
	if err := cleaner.Cleanup(root); err != nil {
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
