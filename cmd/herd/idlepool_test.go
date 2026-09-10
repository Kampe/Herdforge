package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

func idlePoolTestInitRepo(t *testing.T, root string) {
	t.Helper()
	cmds := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "test@test.com"},
		{"git", "config", "user.name", "Test"},
		{"git", "config", "commit.gpgsign", "false"},
	}
	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "README.md"},
		{"git", "commit", "-m", "initial"},
		// reclaimIdlePoolsOnPulse's default base is "origin/main" -- give the
		// fixture repo a real local ref by that literal name so the wired
		// call's reachability check has something real to check against.
		{"git", "branch", "origin/main"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, out)
		}
	}
}

// writeFakeCensusBinaries installs fake lsof/ps so the real census code walks
// a deterministic process table instead of the host's (whose walk duration is
// a host-load property; truncation now correctly fails closed). The self
// variant reports only this test process; the foreign variant reports a
// single foreign-uid pid, which the census classifies without argv reads.
func writeFakeCensusBinaries(t *testing.T, foreign bool) string {
	t.Helper()
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "lsof"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	psScript := "#!/bin/sh\ncase \"$*\" in\n"
	if foreign {
		psScript += "  *\"pid=,uid=\"*) printf '1 0\\n' ;;\n" +
			"  *-axo*) printf '1\\n' ;;\n" +
			"  *\"-o uid=\"*) printf '0\\n' ;;\n" +
			"  *) printf 'init\\n' ;;\n"
	} else {
		psScript += "  *\"pid=,uid=\"*) printf '%s %s\\n' \"$FAKE_PS_SELF_PID\" \"$FAKE_PS_SELF_UID\" ;;\n" +
			"  *-axo*) printf '%s\\n' \"$FAKE_PS_SELF_PID\" ;;\n" +
			"  *\"-o uid=\"*) if [ \"$2\" = \"$FAKE_PS_SELF_PID\" ]; then printf '%s\\n' \"$FAKE_PS_SELF_UID\"; fi ;;\n" +
			"  *) printf '%s\\n' \"$FAKE_PS_SELF_UID\" ;;\n"
	}
	psScript += "esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "ps"), []byte(psScript), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeBin
}

func fakeSelfCensusEnv(t *testing.T) {
	t.Helper()
	fakeBin := writeFakeCensusBinaries(t, false)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PS_SELF_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("FAKE_PS_SELF_UID", strconv.Itoa(os.Getuid()))
}

// TestReclaimIdlePoolsOnPulse_ReclaimsCleanRootAndLeavesLeasedAlone proves
// the pulse-wired call is real: it actually reaches worktree.ReclaimIdlePools
// against HERD_ROOT and reclaims exactly what a direct call would.
func TestReclaimIdlePoolsOnPulse_ReclaimsCleanRootAndLeavesLeasedAlone(t *testing.T) {
	root := t.TempDir()
	idlePoolTestInitRepo(t, root)
	// The wired call uses the production census; give it a deterministic
	// process table so reclaim depends on the fixture, not host load.
	fakeSelfCensusEnv(t)

	clean := worktree.NewPool(root, filepath.Join(root, ".herd", "pool-fac-clean"), 1)
	clean.DefaultBase = "origin/main"
	if err := clean.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure clean: %v", err)
	}
	leased := worktree.NewPool(root, filepath.Join(root, ".herd", "pool-fac-leased"), 1)
	leased.DefaultBase = "origin/main"
	if err := leased.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure leased: %v", err)
	}
	if _, err := leased.Lease(context.Background(), "reviewer"); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	oldHome, hadHome := os.LookupEnv("HERD_ROOT")
	if err := os.Setenv("HERD_ROOT", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HERD_ROOT", oldHome)
		} else {
			_ = os.Unsetenv("HERD_ROOT")
		}
	})

	var stderr bytes.Buffer
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reclaimIdlePoolsOnPulse(context.Background(), w)
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	stderr.Write(buf[:n])

	cleanSlots, err := clean.Slots()
	if err != nil || len(cleanSlots) != 0 {
		t.Fatalf("clean pool should be reclaimed via pulse wiring, slots=%d err=%v", len(cleanSlots), err)
	}
	leasedSlots, err := leased.Slots()
	if err != nil || len(leasedSlots) != 1 {
		t.Fatalf("leased pool must survive pulse-wired reclaim, slots=%d err=%v", len(leasedSlots), err)
	}
	if !strings.Contains(stderr.String(), "eligible=1") {
		t.Fatalf("expected pulse reclaim report to mention eligible=1, got: %q", stderr.String())
	}
}

// TestReclaimIdlePoolsOnPulse_GenuineFailurePropagatesNonzero proves a real
// (not ordinary retained/leased/protected) failure makes the wiring report
// false, which runPulseCommandContext turns into a non-zero process exit --
// the fail-closed contract Google's review found silently swallowed.
func TestReclaimIdlePoolsOnPulse_GenuineFailurePropagatesNonzero(t *testing.T) {
	root := t.TempDir()
	idlePoolTestInitRepo(t, root)
	// A schema-corrupt pool root is a genuine failure, never an ordinary
	// safety refusal -- it must count against the acting tick's success.
	badRoot := filepath.Join(root, ".herd", "pool-fac-corrupt")
	if err := os.MkdirAll(badRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRoot, "pool.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldHome, hadHome := os.LookupEnv("HERD_ROOT")
	if err := os.Setenv("HERD_ROOT", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HERD_ROOT", oldHome)
		} else {
			_ = os.Unsetenv("HERD_ROOT")
		}
	})

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if ok := reclaimIdlePoolsOnPulse(context.Background(), devnull); ok {
		t.Fatal("expected reclaimIdlePoolsOnPulse to report false on a genuine schema failure")
	}
}

// TestReclaimIdlePoolsOnPulse_BusyTickDefersBenignly proves the sentinel
// wiring: a tick that loses the lock race to a live holder is classified as
// benign deferral (true), not a genuine failure (false).
func TestReclaimIdlePoolsOnPulse_BusyTickDefersBenignly(t *testing.T) {
	root := t.TempDir()
	idlePoolTestInitRepo(t, root)
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("hostname unavailable: %v", err)
	}
	lockBody := `{"version":1,"host":"` + host + `","pid":` + strconv.Itoa(os.Getpid()) + `,"started_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}` + "\n"
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".herd", "pool-discovery.lock"), []byte(lockBody), 0o600); err != nil {
		t.Fatal(err)
	}

	oldHome, hadHome := os.LookupEnv("HERD_ROOT")
	if err := os.Setenv("HERD_ROOT", root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadHome {
			_ = os.Setenv("HERD_ROOT", oldHome)
		} else {
			_ = os.Unsetenv("HERD_ROOT")
		}
	})

	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	if ok := reclaimIdlePoolsOnPulse(context.Background(), devnull); !ok {
		t.Fatal("busy tick lost to a live holder must defer benignly, not report failure")
	}
}

// TestRunIdlePool_RealCLIDispatch builds the actual herd binary and invokes
// `herd idle-pool` as a real subprocess, not a helper-only call, proving the
// command is reachable through main's admission switch and produces the
// documented output and exit code end to end.
func TestRunIdlePool_RealCLIDispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a real binary; skipped under -short")
	}
	bin := filepath.Join(t.TempDir(), "herd-idlepool-cli-test")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOFLAGS=-p=2")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v (%s)", err, out)
	}

	root := t.TempDir()
	idlePoolTestInitRepo(t, root)
	// The CLI subprocess runs the production census against the host process
	// table, whose walk duration is a host-load property (a truncated walk
	// now correctly fails closed). Give the subprocess a deterministic
	// single-foreign-pid table so the census is definitively clean.
	// t.Setenv keeps exactly one PATH entry, so the inherited subprocess
	// environment resolves LookPath to the fakes without duplicate keys.
	fakeBin := writeFakeCensusBinaries(t, true)
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	pool := worktree.NewPool(root, filepath.Join(root, ".herd", "pool-fac-cli"), 1)
	pool.DefaultBase = "origin/main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	run := func(args ...string) (string, int) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "HERD_ROOT="+root)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}

	// Unknown-command control: proves the binary really routes through
	// admission (a truly unwired command would fail identically either way,
	// so this establishes the failure mode looks like "unknown subcommand").
	if out, code := run("idle-pool-does-not-exist"); code == 0 || !strings.Contains(out, "unknown subcommand") {
		t.Fatalf("control case: out=%q code=%d", out, code)
	}

	dryOut, dryCode := run("idle-pool", "--base", "origin/main")
	if dryCode != 0 || !strings.Contains(dryOut, "eligible\tpool-fac-cli") {
		t.Fatalf("herd idle-pool dry run: out=%q code=%d", dryOut, dryCode)
	}
	if _, err := os.Stat(filepath.Join(root, ".herd", "pool-discovery-cursor.json")); !os.IsNotExist(err) {
		t.Fatalf("real CLI dry run must not write the cursor file: %v", err)
	}
	slots, err := pool.Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("real CLI dry run must not mutate the pool: slots=%d err=%v", len(slots), err)
	}

	actOut, actCode := run("idle-pool", "--base", "origin/main", "--act")
	if actCode != 0 || !strings.Contains(actOut, "eligible\tpool-fac-cli") {
		t.Fatalf("herd idle-pool --act: out=%q code=%d", actOut, actCode)
	}
	slots, err = pool.Slots()
	if err != nil || len(slots) != 0 {
		t.Fatalf("real CLI --act must reclaim the eligible pool: slots=%d err=%v", len(slots), err)
	}
	if _, err := os.Stat(filepath.Join(root, ".herd", "pool-discovery-cursor.json")); err != nil {
		t.Fatalf("real CLI --act must persist the cursor: %v", err)
	}
}
