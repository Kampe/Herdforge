package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lock"
)

// sandbox returns a temp dir that mimics a checkout: `.git` exists so the lock
// dir's parent is present. All herd subprocesses run with cwd = sandbox.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// runHerd runs the freshly-built binary in dir with the given args/env.
func runHerd(t *testing.T, dir string, env []string, args ...string) ([]byte, error) {
	t.Helper()
	binary := buildHerd(t)
	cmd := exec.Command(binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

// installFakeGit puts a fake `git` on PATH returning porcelain for any call,
// so the dirty gate never touches a real repository.
func installFakeGit(t *testing.T, dir, porcelain string) func() {
	t.Helper()
	binDir := filepath.Join(dir, "fakebin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "#!/bin/sh\nprintf '%s' '" + porcelain + "'\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	old := os.Getenv("PATH")
	if err := os.Setenv("PATH", binDir+":"+old); err != nil {
		t.Fatal(err)
	}
	return func() { os.Setenv("PATH", old) }
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

func lockDirFor(dir string) string {
	// The binary resolves the canonical root through EvalSymlinks (macOS
	// /var -> /private/var), so resolve here too or the paths won't match.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return filepath.Join(dir, lock.DefaultRelDir)
}

func TestLockWithTrueReleases(t *testing.T) {
	dir := sandbox(t)
	out, err := runHerd(t, dir, nil, "lock", "with", "--wait", "2", "--reason", "test", "--", "true")
	if err != nil {
		t.Fatalf("with true: %v (out=%s)", err, out)
	}
	if _, statErr := os.Stat(lockDirFor(dir)); statErr == nil {
		t.Fatalf("lock dir leaked after successful with: %s", lockDirFor(dir))
	}
}

func TestLockWithFalsePropagatesExit(t *testing.T) {
	dir := sandbox(t)
	out, err := runHerd(t, dir, nil, "lock", "with", "--wait", "2", "--", "false")
	if exitCode(err) != 1 {
		t.Fatalf("child exit = %d, want 1 (out=%s)", exitCode(err), out)
	}
	if _, statErr := os.Stat(lockDirFor(dir)); statErr == nil {
		t.Fatalf("lock dir leaked after failing child: %s", lockDirFor(dir))
	}
}

func TestLockWithChildSeesEnvHeld(t *testing.T) {
	dir := sandbox(t)
	probe := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s' \"${HERD_SHARED_LOCK_HELD-}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runHerd(t, dir, nil, "lock", "with", "--", probe)
	if err != nil {
		t.Fatalf("with probe: %v", err)
	}
	// chainseer parity: HERD_SHARED_LOCK_HELD is the full lockdir path
	// (herd-shared-checkout-lock:114), so a zsh caller and a herd caller in
	// the same checkout contend for the SAME directory.
	if strings.TrimSpace(string(out)) != lockDirFor(dir) {
		t.Fatalf("child did not see HERD_SHARED_LOCK_HELD=%s, out=%s", lockDirFor(dir), out)
	}
}

func TestLockStatusUnlocked(t *testing.T) {
	dir := sandbox(t)
	out, err := runHerd(t, dir, nil, "lock", "status")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if strings.TrimSpace(string(out)) != "unlocked" {
		t.Fatalf("status = %q", out)
	}
}

func TestLockStatusLocked(t *testing.T) {
	dir := sandbox(t)
	if _, err := runHerd(t, dir, nil, "lock", "acquire", "--reason", "hold"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	out, err := runHerd(t, dir, nil, "lock", "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(string(out), "LOCKED [") || !strings.Contains(string(out), "reason=hold") {
		t.Fatalf("status = %q", out)
	}
}

func TestLockAcquireTimeout(t *testing.T) {
	dir := sandbox(t)
	holder := lockDirFor(dir)
	// simulate a live foreign hold: mkdir + a holder file with our own pid
	if err := os.Mkdir(holder, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid() // live process, so never stale
	content := "pid=" + strconv.Itoa(pid) + "\nagent=foreign-fixture\nreason=foreign\n"
	if err := os.WriteFile(filepath.Join(holder, "holder"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runHerd(t, dir, nil, "lock", "acquire", "--wait", "1", "--reason", "r")
	if exitCode(err) != 1 {
		t.Fatalf("acquire on live foreign lock: exit=%d want 1 (out=%s)", exitCode(err), out)
	}
	if !strings.Contains(string(out), "locked by") {
		t.Fatalf("timeout message missing 'locked by': %q", out)
	}
}

func TestLockDeadHolderRecovery(t *testing.T) {
	dir := sandbox(t)
	holder := lockDirFor(dir)
	if err := os.Mkdir(holder, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "pid=999999999\nagent=dead\nreason=crash\n"
	if err := os.WriteFile(filepath.Join(holder, "holder"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runHerd(t, dir, nil, "lock", "with", "--wait", "1", "--", "true")
	if err != nil {
		t.Fatalf("with over dead holder: %v (out=%s)", err, out)
	}
	if _, statErr := os.Stat(holder); statErr == nil {
		t.Fatal("dead-holder lock dir not cleaned up")
	}
}

func TestUnknownLockMode(t *testing.T) {
	dir := sandbox(t)
	out, err := runHerd(t, dir, nil, "lock", "frobnicate")
	if exitCode(err) != 2 {
		t.Fatalf("unknown mode exit = %d, want 2 (out=%s)", exitCode(err), out)
	}
	if !strings.Contains(string(out), "unknown mode") {
		t.Fatalf("out = %q", out)
	}
}

func TestLockWithNoCommandUsage(t *testing.T) {
	dir := sandbox(t)
	out, err := runHerd(t, dir, nil, "lock", "with")
	if exitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2 (out=%s)", exitCode(err), out)
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Fatalf("out = %q", out)
	}
}

func TestLockDirtyGateRefuses(t *testing.T) {
	dir := sandbox(t)
	cleanup := installFakeGit(t, dir, "!! fake.go\n?? new.txt\n")
	defer cleanup()
	out, err := runHerd(t, dir, nil, "lock", "with", "--wait", "1", "--reason", "r", "--", "git", "pull")
	if exitCode(err) != 3 {
		t.Fatalf("dirty gate exit = %d, want 3 (out=%s)", exitCode(err), out)
	}
	if !strings.Contains(string(out), "CHA-544") {
		t.Fatalf("missing CHA-544 message: %q", out)
	}
	if !strings.Contains(string(out), "  !! fake.go") {
		t.Fatalf("missing indented dirty line: %q", out)
	}
}

func TestLockDirtyOKBypass(t *testing.T) {
	dir := sandbox(t)
	cleanup := installFakeGit(t, dir, "!! fake.go\n")
	defer cleanup()
	out, err := runHerd(t, dir, []string{"HERD_SHARED_DIRTY_OK=1"}, "lock", "with", "--reason", "r", "--", "git", "pull")
	if err != nil {
		t.Fatalf("HERD_SHARED_DIRTY_OK=1 bypass failed: %v (out=%s)", err, out)
	}
}

func TestLockNonGitNeverGates(t *testing.T) {
	dir := sandbox(t)
	out, err := runHerd(t, dir, nil, "lock", "with", "--reason", "r", "--", "true")
	if err != nil {
		t.Fatalf("non-git command gated incorrectly: %v (out=%s)", err, out)
	}
}

func TestLockReleaseCleanAndAdviceSafe(t *testing.T) {
	dir := sandbox(t)
	lockDir := lockDirFor(dir)
	if err := os.Mkdir(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runHerd(t, dir, nil, "lock", "release")
	if err != nil {
		t.Fatalf("release: %v (out=%s)", err, out)
	}
	_ = out
	if _, err := os.Stat(lockDir); err == nil {
		t.Fatal("release did not remove lockdir")
	}
}

func TestLockAcquireHoldsRelease(t *testing.T) {
	dir := sandbox(t)
	// nested re-entrancy check: hold the lock, spawn a nested `with` -- must
	// run child immediately even though outer lock is held.
	if _, err := runHerd(t, dir, nil, "lock", "acquire", "--reason", "outer"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	probe := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Re-entrancy (chainseer contract, herd-shared-checkout-lock:111): a nested
	// `with` is detected via the HERD_SHARED_LOCK_HELD env marker the outer
	// caller exports — NOT by two independent processes. Simulate the ancestor
	// hold with the env var; the nested `with` must run the child and NOT
	// release the lock it does not own.
	env := []string{lock.EnvHeld + "=" + lockDirFor(dir)}
	out, err := runHerd(t, dir, env, "lock", "with", "--", probe)
	if err != nil {
		t.Fatalf("nested with over held acquire: %v (out=%s)", err, out)
	}
	// lock still held after nested with (the marker path is where acquire put it)
	if _, err := os.Stat(lockDirFor(dir)); err != nil {
		t.Fatal("outer acquire lock lost to nested with")
	}
	// release is never enforced here — this is not a test run; just check binary exits 0 on fresh release
	if _, err := runHerd(t, dir, nil, "lock", "release"); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestLockEnvOverrides(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom-checkout")
	// The lockdir lives under <canonical>/.git, so the canonical checkout must
	// have a .git directory (chainseer assumes it exists).
	if err := os.MkdirAll(filepath.Join(custom, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// HERD_CANONICAL_ROOT points lockdir at the custom checkout even when cwd
	// is elsewhere. Use `acquire` (leaves the lock held) rather than `with`
	// (which releases on exit, removing the lockdir before we could stat it).
	env := []string{"HERD_CANONICAL_ROOT=" + custom}
	binary := buildHerd(t)
	cmd := exec.Command(binary, "lock", "acquire", "--wait", "2", "--reason", "override")
	cmd.Dir = filepath.Dir(custom) // run from outside the canonical root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("acquire outside canonical: %v (out=%s)", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(custom, lock.DefaultRelDir)); statErr != nil {
		t.Fatalf("lock not placed under HERD_CANONICAL_ROOT: %v", statErr)
	}
}

func TestLockUsageAndUnknownModeExit0(t *testing.T) {
	dir := sandbox(t)
	out, err := runHerd(t, dir, nil, "lock")
	if err != nil {
		t.Fatalf("lock with no args should exit 0: %v", err)
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Fatalf("help output missing Usage, got %q", out)
	}
	out, err = runHerd(t, dir, nil, "lock", "-h")
	if err != nil {
		t.Fatalf("lock -h should exit 0: %v", err)
	}
	if !strings.Contains(string(out), "Usage:") {
		t.Fatalf("-h output missing Usage, got %q", out)
	}
}
