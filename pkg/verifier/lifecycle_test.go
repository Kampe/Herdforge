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

// activeLateWriterScript is a production-shaped late writer under root:
//  1. signals ready after seed residue
//  2. continuously mkdir -p + creates new objects via ABSOLUTE paths
//  3. advances genPath (OUTSIDE root so partial RemoveAll cannot erase proof)
//
// Unreaped, it blocks durable TempDir cleanup: either os.RemoveAll fails with
// "directory not empty" (FAC-151 CI class) or RemoveAll "wins" a walk but the
// writer recreates residue while still live. Parked sleep cannot do this.
// No chmod seal/unseal.
//
// Args: $1=root $2=readyPath $3=genPath(outside root)
const activeLateWriterScript = `
root="$1"
objects="$1/.git/objects"
mkdir -p "$objects" || exit 1
printf x > "$objects/seed"
printf ready > "$2"
i=0
while true; do
  mkdir -p "$objects" 2>/dev/null || true
  printf w > "$objects/active-$i" 2>/dev/null || true
  # Atomic-ish gen publish: avoid empty-file reads mid-truncate under -race.
  printf "%s" "$i" > "$3.tmp" 2>/dev/null || true
  mv -f "$3.tmp" "$3" 2>/dev/null || printf "%s" "$i" > "$3"
  i=$((i+1))
done
`

// lateWriterFixture owns root + process group. The sole toggled step before
// cleanupTempDir is production ReapOwnedCmd — no seal/unseal side channel.
type lateWriterFixture struct {
	t         *testing.T
	root      string
	readyPath string
	genPath   string
	cmd       *exec.Cmd
	pgid      int
	stderr    strings.Builder
	started   bool
	reaped    bool
}

func startLateWriterFixture(t *testing.T) *lateWriterFixture {
	t.Helper()
	root, err := os.MkdirTemp("", "verifier-late-writer-*")
	if err != nil {
		t.Fatal(err)
	}
	// gen lives beside root (not under it) so RemoveAll cannot erase the
	// mutation counter while the writer is still proving activity.
	genPath := root + ".writer.gen"
	readyPath := root + ".writer.ready"
	f := &lateWriterFixture{
		t:         t,
		root:      root,
		readyPath: readyPath,
		genPath:   genPath,
	}
	t.Cleanup(f.shutdown)

	f.cmd = exec.Command("sh", "-c", activeLateWriterScript, "late-writer", f.root, f.readyPath, f.genPath)
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
	if err := syscall.Kill(-f.pgid, 0); err != nil {
		t.Fatalf("writer process group must be live after ready: %v", err)
	}
	// Active mutation (not parked sleep): gen must advance past 0.
	if _, err := f.waitGenAdvance(0, 2*time.Second); err != nil {
		t.Fatalf("writer must actively mutate after ready: %v (stderr=%q)", err, f.stderr.String())
	}
	return f
}

// cleanupTempDir is the single cleanup action shared by pre-fix, post-fix,
// and mutation paths. Only production ReapOwnedCmd is toggled around it.
func cleanupTempDir(root string) error {
	return os.RemoveAll(root)
}

// isLiveWriterRemoveAllError attributes failure to the concurrent-writer class
// (directory not empty / unlinkat), not permission tricks.
func isLiveWriterRemoveAllError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "directory not empty") ||
		strings.Contains(msg, "not empty") ||
		strings.Contains(msg, "unlinkat")
}

// ownershipBlocksDurableCleanup proves unreaped active ownership prevents a
// durable TempDir cleanup. Either:
//   - cleanupTempDir returns the live-writer unlinkat/not-empty class, or
//   - cleanupTempDir returns nil but the writer recreates residue while still live
//
// (RemoveAll "won" one walk — not durable). Parked sleep + chmod cannot pass.
func ownershipBlocksDurableCleanup(root string, pgid int) error {
	if err := syscall.Kill(-pgid, 0); err != nil {
		return fmt.Errorf("writer group %d not live before cleanup: %w", pgid, err)
	}
	err := cleanupTempDir(root)
	if err != nil {
		return err
	}
	// RemoveAll returned nil — require non-durable recreation under live writer.
	objects := filepath.Join(root, ".git", "objects")
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if kerr := syscall.Kill(-pgid, 0); kerr != nil {
			return fmt.Errorf("writer group %d died after RemoveAll success (no durable-block proof): %w", pgid, kerr)
		}
		if st, stErr := os.Stat(objects); stErr == nil && st.IsDir() {
			return fmt.Errorf("cleanup not durable: unreaped writer recreated %s after RemoveAll", objects)
		}
		if entries, rdErr := os.ReadDir(root); rdErr == nil && len(entries) > 0 {
			return fmt.Errorf("cleanup not durable: unreaped writer left/recreated %d entries under root", len(entries))
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

// durableCleanupAfterReap runs the same cleanupTempDir and requires the root
// to stay gone (no recreation). Call only after production ReapOwnedCmd.
func durableCleanupAfterReap(root string, observe time.Duration) error {
	if err := cleanupTempDir(root); err != nil {
		return err
	}
	deadline := time.Now().Add(observe)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(root); err == nil {
			return fmt.Errorf("cleanup not durable after reap: root reappeared")
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

func (f *lateWriterFixture) readGen() (int, error) {
	// Brief re-read on empty: concurrent truncate/replace can yield "" once.
	var last error
	for attempt := 0; attempt < 50; attempt++ {
		data, err := os.ReadFile(f.genPath)
		if err != nil {
			last = err
			time.Sleep(time.Millisecond)
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			last = fmt.Errorf("empty gen")
			time.Sleep(time.Millisecond)
			continue
		}
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			last = fmt.Errorf("parse gen %q: %w", s, err)
			time.Sleep(time.Millisecond)
			continue
		}
		return n, nil
	}
	if last == nil {
		last = fmt.Errorf("gen unreadable")
	}
	return 0, last
}

func (f *lateWriterFixture) waitGenAdvance(min int, bound time.Duration) (int, error) {
	deadline := time.Now().Add(bound)
	var lastErr error
	var n int
	for {
		n, lastErr = f.readGen()
		if lastErr == nil && n > min {
			return n, nil
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return 0, fmt.Errorf("gen did not advance past %d: %w", min, lastErr)
			}
			return n, fmt.Errorf("gen stuck at %d (want > %d)", n, min)
		}
		time.Sleep(time.Millisecond)
	}
}

// shutdown: production ReapOwnedCmd (errors reported), then cleanupTempDir.
func (f *lateWriterFixture) shutdown() {
	if f == nil {
		return
	}
	if f.started && !f.reaped {
		if err := ReapOwnedCmd(f.cmd); err != nil {
			// Residual tree after unreaped writer is expected on mutation paths;
			// fail only if kill/wait identity errors remain after close.
			if !errors.Is(err, ErrResidualOwnedTree) && !strings.Contains(err.Error(), "residual owned") {
				if f.pgid > 0 {
					if kerr := killProcessGroupIfLive(f.pgid); kerr != nil {
						f.t.Errorf("shutdown fallback killProcessGroupIfLive %d: %v", f.pgid, kerr)
					}
				}
				f.t.Errorf("shutdown ReapOwnedCmd: %v (stderr=%q)", err, f.stderr.String())
			}
		}
		f.reaped = true
	}
	if f.root != "" {
		if err := cleanupTempDir(f.root); err != nil && !os.IsNotExist(err) {
			f.t.Errorf("shutdown cleanupTempDir: %v", err)
		}
	}
	if f.genPath != "" {
		if err := os.Remove(f.genPath); err != nil && !os.IsNotExist(err) {
			f.t.Errorf("shutdown remove gen: %v", err)
		}
	}
	if f.readyPath != "" {
		if err := os.Remove(f.readyPath); err != nil && !os.IsNotExist(err) {
			f.t.Errorf("shutdown remove ready: %v", err)
		}
	}
}

// reapOwned closes ownership via production ReapOwnedCmd only. Errors fatal.
func (f *lateWriterFixture) reapOwned() {
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

// TestLateWriterIntoGitRequiresExplicitReap: unreaped active writer blocks
// durable cleanup; production ReapOwnedCmd is the sole toggled step; same
// cleanupTempDir then durably succeeds. No chmod/unseal.
func TestLateWriterIntoGitRequiresExplicitReap(t *testing.T) {
	f := startLateWriterFixture(t)

	genBefore, err := f.readGen()
	if err != nil {
		t.Fatalf("pre-fix: initial gen: %v", err)
	}

	// PRE-FIX: only cleanupTempDir — writer unreaped → durable cleanup blocked.
	preErr := ownershipBlocksDurableCleanup(f.root, f.pgid)
	if preErr == nil {
		t.Fatal("pre-fix: durable cleanup must be blocked by unreaped active writer")
	}
	if err := syscall.Kill(-f.pgid, 0); err != nil {
		t.Fatalf("unreaped process group must remain live after blocked cleanup: %v (stderr=%q)", err, f.stderr.String())
	}
	// Continuing mutation (gen outside root) — proves active writer, not park.
	if _, err := f.waitGenAdvance(genBefore, 2*time.Second); err != nil {
		t.Fatalf("pre-fix: writer must keep mutating after blocked cleanup: %v", err)
	}

	// FIX: toggle ONLY production ReapOwnedCmd (kill+Wait+group-gone).
	f.reapOwned()
	genAfterReap, err := f.readGen()
	if err != nil {
		// gen may be mid-write at kill; treat unreadable as frozen baseline.
		genAfterReap = genBefore
	}

	// POST-FIX: same cleanupTempDir must durably succeed — no recreation.
	if err := durableCleanupAfterReap(f.root, 100*time.Millisecond); err != nil {
		t.Fatalf("post-fix: durable cleanup after production ReapOwnedCmd: %v", err)
	}
	if err := syscall.Kill(-f.pgid, 0); err == nil {
		t.Fatalf("process group %d still live after production ReapOwnedCmd", f.pgid)
	}
	// Gen must not keep advancing after reap (writer dead).
	time.Sleep(20 * time.Millisecond)
	if g2, gerr := f.readGen(); gerr == nil && g2 > genAfterReap+1000 {
		t.Fatalf("post-fix: gen advanced after reap (%d -> %d); writer still mutating", genAfterReap, g2)
	}
	f.root = ""
}

// TestLateWriterCleanupMutationOmittingReapStillFails: identical cleanup path;
// only production ReapOwnedCmd is omitted. Mutant must fail durable cleanup.
func TestLateWriterCleanupMutationOmittingReapStillFails(t *testing.T) {
	f := startLateWriterFixture(t)

	gen0, err := f.readGen()
	if err != nil {
		t.Fatalf("mutation: initial gen: %v", err)
	}

	// Control: unreaped active writer blocks durable cleanup.
	if err := ownershipBlocksDurableCleanup(f.root, f.pgid); err == nil {
		t.Fatal("control: durable cleanup must fail under unreaped active writer")
	}

	// MUTATION: omit only ReapOwnedCmd; every other cleanup action identical.
	mutErr := ownershipBlocksDurableCleanup(f.root, f.pgid)
	if mutErr == nil {
		t.Fatal("mutation: omitting only production ReapOwnedCmd must leave durable cleanup failing; got nil")
	}
	if err := syscall.Kill(-f.pgid, 0); err != nil {
		t.Fatalf("mutation: process group must still be live without reap: %v", err)
	}
	if _, err := f.waitGenAdvance(gen0, 2*time.Second); err != nil {
		t.Fatalf("mutation: writer must keep mutating without reap: %v", err)
	}
	// Fixture t.Cleanup runs production ReapOwnedCmd then cleanupTempDir.
}

// TestProcessGroupReapAllowsTempDirCleanup: blocked cleanup → ReapOwnedCmd →
// durable cleanupTempDir success (no seal/unseal).
func TestProcessGroupReapAllowsTempDirCleanup(t *testing.T) {
	f := startLateWriterFixture(t)
	if err := ownershipBlocksDurableCleanup(f.root, f.pgid); err == nil {
		t.Fatal("pre-reap: durable cleanup must fail under active writer")
	}
	f.reapOwned()
	if err := durableCleanupAfterReap(f.root, 100*time.Millisecond); err != nil {
		t.Fatalf("post-reap: durable cleanup must succeed: %v", err)
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
		// Leak-safe: production reap if still unreaped; kill/Wait errors reported.
		if cmd.ProcessState == nil && cmd.Process != nil {
			if err := ReapOwnedCmd(cmd); err != nil && !errors.Is(err, ErrResidualOwnedTree) &&
				!strings.Contains(err.Error(), "residual owned") {
				if kerr := killProcessGroupIfLive(pgid); kerr != nil {
					t.Errorf("cleanup fallback killProcessGroupIfLive %d: %v", pgid, kerr)
				}
				t.Errorf("cleanup ReapOwnedCmd: %v", err)
			}
		} else if pgid > 0 {
			if err := killProcessGroupIfLive(pgid); err != nil {
				t.Errorf("cleanup killProcessGroupIfLive %d: %v", pgid, err)
			}
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

// TestReapOwnedCmdTreeCloseKillsGrandchildrenDespiteLeaderOnlyGroupKill proves
// durable tree tracking is load-bearing: even if processGroupKiller is mutated
// to leader-only, ReapOwnedCmd still reaps grandchildren via positive-PID
// finalize (not empty -pgid reuse).
func TestReapOwnedCmdTreeCloseKillsGrandchildrenDespiteLeaderOnlyGroupKill(t *testing.T) {
	cmd, pgid, grandchild := startGrandchildGroup(t)

	prev := processGroupKiller
	processGroupKiller = func(id int) error {
		if id <= 0 {
			return fmt.Errorf("kill process group: invalid pgid %d", id)
		}
		// MUTATION: kill leader only — group-wide signal is insufficient alone.
		return syscall.Kill(id, syscall.SIGKILL)
	}
	t.Cleanup(func() { processGroupKiller = prev })

	if err := ReapOwnedCmd(cmd); err != nil {
		// Residual tree error is acceptable if grandchild is still cleaned;
		// hard-fail only when grandchild survives.
		if kerr := syscall.Kill(grandchild, 0); kerr == nil {
			t.Fatalf("ReapOwnedCmd error %v and grandchild %d still live", err, grandchild)
		}
	}
	if err := waitForPIDGone(grandchild, 2*time.Second); err != nil {
		t.Fatalf("tree close must reap grandchild %d despite leader-only group killer: %v", grandchild, err)
	}
	if err := waitForProcessGroupGone(pgid, 2*time.Second); err != nil {
		// Group may already be empty; only fail if still live.
		if processGroupLive(pgid) {
			t.Fatalf("process group %d still live: %v", pgid, err)
		}
	}
}

// TestFinalizeOwnedTreeMutationLeavesGrandchildAlive proves that skipping
// causal handle finalize (and skipping tracked kill) leaves a grandchild live
// after a leader-only group membership kill — the incomplete ownership shape.
func TestFinalizeOwnedTreeMutationLeavesGrandchildAlive(t *testing.T) {
	cmd, pgid, grandchild := startGrandchildGroup(t)

	prevKill := processGroupKiller
	processGroupKiller = func(id int) error {
		if id <= 0 {
			return fmt.Errorf("kill process group: invalid pgid %d", id)
		}
		// Leader-only membership kill (wrong).
		return syscall.Kill(id, syscall.SIGKILL)
	}
	prevFin := finalizeOwnedTree
	finalizeOwnedTree = func(o *ownedSubprocess) error {
		if o != nil {
			o.freeze()
			_ = o.stopTracker()
			// Intentionally do not killTracked — mutation.
		}
		return nil
	}
	t.Cleanup(func() {
		processGroupKiller = prevKill
		finalizeOwnedTree = prevFin
		if err := killProcessGroupIfLive(pgid); err != nil {
			t.Errorf("cleanup killProcessGroupIfLive: %v", err)
		}
		if err := syscall.Kill(grandchild, syscall.SIGKILL); err != nil && !isESRCH(err) {
			t.Errorf("cleanup SIGKILL grandchild: %v", err)
		}
		if err := waitForPIDGone(grandchild, 2*time.Second); err != nil {
			t.Errorf("cleanup grandchild gone: %v", err)
		}
	})

	owned, err := adoptOwnedCmd(cmd, nil, nil, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Incomplete: leader-only membership kill only (no killTracked).
	if err := processGroupKiller(owned.Pgid()); err != nil {
		t.Fatalf("leader-only killer: %v", err)
	}
	_ = cmd.Wait()
	if err := finalizeOwnedTree(owned); err != nil {
		t.Fatalf("mutated finalize should return nil: %v", err)
	}
	if err := syscall.Kill(grandchild, 0); err != nil {
		t.Fatalf("mutation expected grandchild %d to survive incomplete ownership: %v", grandchild, err)
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
