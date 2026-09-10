package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

type poolProcessInspectorFunc func(context.Context, string) (resources.ProcessUsage, error)

func (f poolProcessInspectorFunc) InUse(ctx context.Context, path string) (resources.ProcessUsage, error) {
	return f(ctx, path)
}

// silentCensusInspector reports a definitive, evidence-free census. It
// replaces the default native census in tests whose purpose is something
// other than the census itself (pool-state consistency, reachability,
// lease-history fences, dry-run purity): the real walk is host-load
// dependent, and truncation now fails closed as metadata-unavailable.
func silentCensusInspector() poolProcessInspectorFunc {
	return poolProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
}

// writePoolState overwrites pool.json directly so a test can construct a
// state the normal Ensure/Lease API would never produce -- corrupt,
// mismatched, or maliciously substituted slot records.
func writePoolState(t *testing.T, poolRoot string, state poolState) {
	t.Helper()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(poolRoot, "pool.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPoolGCRefusesUnregisteredDirectory(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	poolRoot := filepath.Join(root, ".herd", "pool")
	pool := NewPool(root, poolRoot, 0)
	pool.DefaultBase = "main"
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// A plain directory inside the pool root that was never `git worktree
	// add`ed -- e.g. hand-placed, or left behind by an unrelated process.
	decoy := filepath.Join(poolRoot, "pool-01")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(decoy, "not-a-worktree.txt")
	if err := os.WriteFile(sentinel, []byte("real content"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePoolState(t, poolRoot, poolState{Version: 1, Slots: []PoolSlot{{Name: "pool-01", Path: decoy}}})

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err == nil || !strings.Contains(err.Error(), "registered git worktree") {
		t.Fatalf("GC error = %v, want refusal for unregistered directory", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("unregistered directory must survive refused GC, stat error: %v", err)
	}
}

func TestPoolGCRefusesPathOutsidePoolRoot(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	poolRoot := filepath.Join(root, ".herd", "pool")
	pool := NewPool(root, poolRoot, 0)
	pool.DefaultBase = "main"
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// A real, unrelated directory that lives under the repository but
	// strictly outside the pool root -- what the reported bug's relative,
	// un-normalized slot.Path could resolve to.
	outside := filepath.Join(root, "outside-secret")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "do-not-delete.txt")
	if err := os.WriteFile(sentinel, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Recorded relative to the REPO root (as repoPath resolves it), landing
	// outside the pool root -- the exact shape the reported bug's
	// un-normalized relative slot.Path could take.
	writePoolState(t, poolRoot, poolState{Version: 1, Slots: []PoolSlot{{Name: "pool-01", Path: "outside-secret"}}})

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err == nil || !strings.Contains(err.Error(), "outside the pool root") {
		t.Fatalf("GC error = %v, want refusal for path outside pool root", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("out-of-pool directory must survive refused GC, stat error: %v", err)
	}
}

func TestPoolGCRefusesSymlinkSlot(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	poolRoot := filepath.Join(root, ".herd", "pool")
	pool := NewPool(root, poolRoot, 0)
	pool.DefaultBase = "main"
	if err := os.MkdirAll(poolRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	decoy := filepath.Join(root, "symlink-target")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(decoy, "do-not-delete.txt")
	if err := os.WriteFile(sentinel, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(poolRoot, "pool-01")
	if err := os.Symlink(decoy, slotPath); err != nil {
		t.Fatal(err)
	}
	writePoolState(t, poolRoot, poolState{Version: 1, Slots: []PoolSlot{{Name: "pool-01", Path: slotPath}}})

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("GC error = %v, want refusal for symlinked slot", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("symlink target must survive refused GC, stat error: %v", err)
	}
	if info, err := os.Lstat(slotPath); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink itself must survive refused GC untouched, lstat: %+v err=%v", info, err)
	}
}

func TestPoolGCRefusesDirtyUntrackedContent(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	slotPath := filepath.Join(pool.Root, "pool-01")
	if err := os.WriteFile(filepath.Join(slotPath, "untracked.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("GC error = %v, want dirty refusal", err)
	}
	if _, err := os.Stat(slotPath); err != nil {
		t.Fatalf("dirty slot must survive refused GC, stat error: %v", err)
	}
}

func TestPoolGCRefusesIgnoredContent(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(root, "git", "add", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(root, "git", "commit", "-m", "add gitignore"); err != nil {
		t.Fatal(err)
	}
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	slotPath := filepath.Join(pool.Root, "pool-01")
	if err := os.MkdirAll(filepath.Join(slotPath, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotPath, "build", "artifact.bin"), []byte("left behind"), 0o644); err != nil {
		t.Fatal(err)
	}

	// gitClean (used by Lease) does not see this: it is plain `git status
	// --porcelain`, which omits ignored paths -- proving GC's stricter
	// gitFullyClean check is doing real, additional work.
	if clean, err := gitClean(context.Background(), pool.RepoRoot, slotPath); err != nil || !clean {
		t.Fatalf("precondition: gitClean should report clean for ignored-only content, clean=%v err=%v", clean, err)
	}

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("GC error = %v, want refusal for ignored content", err)
	}
	if _, err := os.Stat(slotPath); err != nil {
		t.Fatalf("slot with ignored content must survive refused GC, stat error: %v", err)
	}
}

func TestPoolGCRefusesHeadNotReachableFromBase(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	pool.ProcessInspector = silentCensusInspector()
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	slotPath := filepath.Join(pool.Root, "pool-01")
	// A real local commit that only exists in this slot -- never merged into
	// or reachable from base.
	if err := os.WriteFile(filepath.Join(slotPath, "unique-work.txt"), []byte("only here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(slotPath, "git", "add", "unique-work.txt"); err != nil {
		t.Fatal(err)
	}
	if err := runCmd(slotPath, "git", "commit", "-m", "unique work never merged"); err != nil {
		t.Fatal(err)
	}

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err == nil || !strings.Contains(err.Error(), "not reachable from base") {
		t.Fatalf("GC error = %v, want refusal for HEAD not reachable from base", err)
	}
	if _, err := os.Stat(slotPath); err != nil {
		t.Fatalf("divergent slot must survive refused GC, stat error: %v", err)
	}
}

func TestPoolGCRefusesWhileAnySlotLeased(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 2)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := pool.Lease(context.Background(), "reviewer"); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err == nil || !strings.Contains(err.Error(), "leased") {
		t.Fatalf("GC error = %v, want refusal while a slot is leased", err)
	}
	slots, err := pool.Slots()
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 {
		t.Fatalf("leased-batch refusal must remove nothing, slots=%d", len(slots))
	}
	for _, s := range slots {
		if _, err := os.Stat(s.Path); err != nil {
			t.Fatalf("slot %s must survive refused GC, stat error: %v", s.Name, err)
		}
	}
}

// TestPoolGCRealCensusReclaimsGenuinelyOwnerlessSlot proves the production
// census path (no injected inspector) still comes back definitively clean
// for a genuinely ownerless slot: fake lsof/ps binaries report no evidence,
// so every refusal path -- including the fail-closed metadata guards -- must
// stay silent and GC must reclaim the slot.
func TestPoolGCRealCensusReclaimsGenuinelyOwnerlessSlot(t *testing.T) {
	// The fake ps dir must stay outside the probed tree: it lands in this
	// process's PATH environment, which the Linux /proc/self/environ walk
	// reads, and must never substring-match the probed slot path.
	fakeBin := t.TempDir()
	lsofScript := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "lsof"), []byte(lsofScript), 0o700); err != nil {
		t.Fatal(err)
	}
	psScript := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *\"pid=,uid=\"*) printf '%s %s\\n' \"$FAKE_PS_SELF_PID\" \"$FAKE_PS_SELF_UID\" ;;\n" +
		"  *-axo*) printf '%s\\n' \"$FAKE_PS_SELF_PID\" ;;\n" +
		"  *\"-o uid=\"*) if [ \"$2\" = \"$FAKE_PS_SELF_PID\" ]; then printf '%s\\n' \"$FAKE_PS_SELF_UID\"; fi ;;\n" +
		"  *) printf '%s\\n' \"$FAKE_PS_SELF_UID\" ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "ps"), []byte(psScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PS_SELF_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("FAKE_PS_SELF_UID", strconv.Itoa(os.Getuid()))

	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	slots, err := pool.Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("precondition: 1 clean slot, got %d err=%v", len(slots), err)
	}
	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err != nil {
		t.Fatalf("genuinely ownerless clean slot must be reclaimable by the real census path: %v", err)
	}
	if _, statErr := os.Stat(slots[0].Path); !os.IsNotExist(statErr) {
		t.Fatalf("slot must be removed after reclaim, stat err=%v", statErr)
	}
}

func TestPoolGCSucceedsAndLeavesPoolConsistentForEnsure(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 2)
	pool.DefaultBase = "main"
	pool.ProcessInspector = silentCensusInspector()
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	before, err := pool.Slots()
	if err != nil || len(before) != 2 {
		t.Fatalf("precondition: 2 clean slots, got %d err=%v", len(before), err)
	}

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err != nil {
		t.Fatalf("GC on clean, unleased, registered slots must succeed: %v", err)
	}
	for _, s := range before {
		if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
			t.Fatalf("slot %s should be removed after successful GC, stat err=%v", s.Name, err)
		}
	}
	after, err := pool.Slots()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("pool state should be empty after full GC, got %d slots", len(after))
	}

	// Successful GC must leave the pool in a state Ensure can rebuild from.
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after GC: %v", err)
	}
	recreated, err := pool.Slots()
	if err != nil || len(recreated) != 2 {
		t.Fatalf("Ensure after GC should recreate 2 slots, got %d err=%v", len(recreated), err)
	}
	for _, s := range recreated {
		if _, err := os.Stat(s.Path); err != nil {
			t.Fatalf("recreated slot %s must exist on disk: %v", s.Name, err)
		}
	}
}

func TestPoolGCPlanDryRunNeverDeletes(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 2)
	pool.DefaultBase = "main"
	pool.ProcessInspector = silentCensusInspector()
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pool.Root, "pool-02", "dirty.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	decisions, err := pool.GCPlan(context.Background(), allowAllRetirementAuthority{})
	if err != nil {
		t.Fatalf("GCPlan: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("GCPlan decisions=%d, want 2", len(decisions))
	}
	want := map[string]bool{"pool-01": false, "pool-02": true}
	for _, d := range decisions {
		if d.Refused != want[d.Slot] {
			t.Fatalf("slot %s refused=%v, want %v (reason=%q)", d.Slot, d.Refused, want[d.Slot], d.Reason)
		}
	}
	slots, err := pool.Slots()
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 {
		t.Fatalf("GCPlan must not mutate pool state, slots=%d", len(slots))
	}
	for _, s := range slots {
		if _, err := os.Stat(s.Path); err != nil {
			t.Fatalf("GCPlan must never delete anything, slot %s stat error: %v", s.Name, err)
		}
	}
}

func TestPoolGCRefusesUnleasedSlotWithOwnedLiveProcess(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(pool.Root, "pool-01")
	child := exec.Command("sleep", "30")
	child.Dir = slotPath
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	resolvedSlot, err := filepath.EvalSymlinks(slotPath)
	if err != nil {
		t.Fatal(err)
	}
	pids := []int{child.Process.Pid}
	pool.ProcessInspector = poolProcessInspectorFunc(func(_ context.Context, path string) (resources.ProcessUsage, error) {
		if path != resolvedSlot {
			return resources.ProcessUsage{}, nil
		}
		return resources.ProcessUsage{CWD: true, PIDs: pids}, nil
	})

	err = pool.GC(context.Background(), allowAllRetirementAuthority{})
	if err == nil || !strings.Contains(err.Error(), "live process") {
		t.Fatalf("GC error = %v, want live-process refusal", err)
	}
	if _, statErr := os.Stat(slotPath); statErr != nil {
		t.Fatalf("live-process slot must survive refused GC: %v", statErr)
	}
}

func TestPoolGCDefaultCensusRefusesLiveProcessWithOwnerEvidence(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skipf("lsof unavailable: %v", err)
	}
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	slotPath := filepath.Join(pool.Root, "pool-01")
	child := exec.Command("sh", "-c", `cd "$1" && exec 3<README.md && sleep 30`, "sh", slotPath)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})

	// The real census walks every host pid with one ps exec per pid, so its
	// completion time is a property of the host, not the test (a truncated
	// walk now correctly fails closed as metadata-unavailable). Synthesize
	// the process table instead: exactly this test process and the live
	// child, whose command line carries the slot path so the walk derives
	// explicit same-owner reference evidence.
	fakeBin := t.TempDir()
	psScript := "#!/bin/sh\ncase \"$*\" in\n" +
		"  *\"pid=,uid=\"*) printf '%s %s\\n%s %s\\n' \"$FAKE_PS_SELF_PID\" \"$FAKE_PS_SELF_UID\" \"$FAKE_PS_CHILD_PID\" \"$FAKE_PS_SELF_UID\" ;;\n" +
		"  *-axo*) printf '%s\\n%s\\n' \"$FAKE_PS_SELF_PID\" \"$FAKE_PS_CHILD_PID\" ;;\n" +
		"  *\"-o uid=\"*) printf '%s\\n' \"$FAKE_PS_SELF_UID\" ;;\n" +
		"  *) printf '%s\\n' \"$FAKE_PS_COMMAND_LINE\" ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "ps"), []byte(psScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PS_SELF_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("FAKE_PS_SELF_UID", strconv.Itoa(os.Getuid()))
	t.Setenv("FAKE_PS_CHILD_PID", strconv.Itoa(child.Process.Pid))
	t.Setenv("FAKE_PS_COMMAND_LINE", "sh -c cd-and-sleep "+slotPath)

	err := pool.GC(context.Background(), allowAllRetirementAuthority{})
	if err == nil || !strings.Contains(err.Error(), "live process owns or references") {
		t.Fatalf("default native census error = %v, want explicit owner refusal", err)
	}
	if _, statErr := os.Stat(slotPath); statErr != nil {
		t.Fatalf("live-process slot must survive refused GC: %v", statErr)
	}
}

func TestPoolGCRefusesUnknownProcessCensus(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	pool.ProcessInspector = poolProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{MetadataUnavailable: true}, nil
	})

	err := pool.GC(context.Background(), allowAllRetirementAuthority{})
	if err == nil || !strings.Contains(err.Error(), "metadata unavailable") {
		t.Fatalf("GC error = %v, want fail-closed census refusal", err)
	}
	slots, err := pool.Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("unknown census must preserve pool state, slots=%d err=%v", len(slots), err)
	}
}

func TestPoolGCCleanInjectedCensusRemovesAndEnsureRebuilds(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	pool.ProcessInspector = poolProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})

	if err := pool.GC(context.Background(), allowAllRetirementAuthority{}); err != nil {
		t.Fatalf("clean ownerless GC: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pool.Root, "pool-01")); !os.IsNotExist(err) {
		t.Fatalf("clean slot should be removed, stat error=%v", err)
	}
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after GC: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pool.Root, "pool-01")); err != nil {
		t.Fatalf("Ensure should rebuild removed slot: %v", err)
	}
}

func TestPoolGCRefusesPositiveExitOneWithoutDescriptorName(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nprintf 'p99999\\nf3\\n'\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pool.ProcessInspector = resources.LSOFProcessInspector{Executable: lsof, Timeout: time.Second}

	err := pool.GC(context.Background(), allowAllRetirementAuthority{})
	if err == nil || !strings.Contains(err.Error(), "metadata unavailable") {
		t.Fatalf("GC error = %v, want fail-closed census refusal on partial positive exit 1", err)
	}
	slots, err := pool.Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("partial positive census must preserve pool state, slots=%d err=%v", len(slots), err)
	}
	if _, err := os.Stat(filepath.Join(pool.Root, "pool-01")); err != nil {
		t.Fatalf("slot must not be deleted on partial positive exit 1: %v", err)
	}
}

// allowAllRetirementAuthority is the test stand-in for verified retirement
// evidence: it authorizes every pool root, so the pre-existing GC safety
// tests exercise their own dedicated guards without the fence refusing.
type allowAllRetirementAuthority struct{}

func (allowAllRetirementAuthority) AuthorizePoolRoot(poolRootAbs string) error { return nil }

// TestPoolGCRefusesNilRetirementAuthority pins that the destructive
// primitive itself requires the retirement-evidence fence: no authority, no
// GC, regardless of caller.
func TestPoolGCRefusesNilRetirementAuthority(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	pool.ProcessInspector = silentCensusInspector()
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := pool.GC(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "retirement-evidence authority") {
		t.Fatalf("nil authority must refuse GC outright, got err=%v", err)
	}
	slots, err := pool.Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("refused GC must remove nothing, slots=%d err=%v", len(slots), err)
	}
}

// TestPoolGCRefusesPoolUnderUnconfirmedRetirementEvidence pins the direct
// path to the same evidence the idle-discovery fence reads: a pool named by
// an unconfirmed manifest generation is refused; once the phase journal
// records that generation complete, the same pool is reclaimable.
func TestPoolGCRefusesPoolUnderUnconfirmedRetirementEvidence(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	pool.ProcessInspector = silentCensusInspector()
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	manifestPath := filepath.Join(root, "retirement-manifests.jsonl")
	journalPath := filepath.Join(root, "retirement-phases.jsonl")
	if err := os.WriteFile(manifestPath, []byte(`{"pool":".herd/pool","generation":"g1","candidate_sha":"c1","reviewer":"r1","binding_digest":"b1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := NewManifestRetirementAuthority(root, manifestPath, journalPath)

	if err := pool.GC(context.Background(), authority); err == nil || !strings.Contains(err.Error(), "retirement evidence") {
		t.Fatalf("pool under unconfirmed retirement evidence must be refused, got err=%v", err)
	}
	slots, err := pool.Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("refused GC must remove nothing, slots=%d err=%v", len(slots), err)
	}

	// The authoritative completion record clears the fence.
	if err := os.WriteFile(journalPath, []byte(`{"pool":".herd/pool","generation":"g1","candidate_sha":"c1","reviewer":"r1","binding_digest":"b1","phase":"complete"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pool.GC(context.Background(), authority); err != nil {
		t.Fatalf("pool with every manifested generation complete must be reclaimable: %v", err)
	}
	if _, statErr := os.Stat(slots[0].Path); !os.IsNotExist(statErr) {
		t.Fatalf("slot must be removed once evidence authorizes, stat err=%v", statErr)
	}
}

// TestPoolGCPlanReflectsRetirementEvidenceRefusal keeps the dry run from
// diverging from the destructive decision: the same authority refuses in
// both.
func TestPoolGCPlanReflectsRetirementEvidenceRefusal(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	pool.ProcessInspector = silentCensusInspector()
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	manifestPath := filepath.Join(root, "retirement-manifests.jsonl")
	journalPath := filepath.Join(root, "retirement-phases.jsonl")
	if err := os.WriteFile(manifestPath, []byte(`{"pool":".herd/pool","generation":"g1","candidate_sha":"c1","reviewer":"r1","binding_digest":"b1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := NewManifestRetirementAuthority(root, manifestPath, journalPath)
	decisions, err := pool.GCPlan(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || !decisions[0].Refused || !strings.Contains(decisions[0].Reason, "retirement evidence") {
		t.Fatalf("dry run must refuse the protected pool, got %+v", decisions)
	}
}

// TestPoolGCRefusesPoolAbsentFromManifest pins the positive-evidence
// contract: a same-owner pool with NO retirement evidence on file is not
// automatically disposable -- lease/history plus registered/reachable clean
// state alone is not authority. Unknown scope retains.
func TestPoolGCRefusesPoolAbsentFromManifest(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	pool.ProcessInspector = silentCensusInspector()
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Real authority over evidence files that exist but never name this
	// pool: some OTHER pool's completed generation, or nothing at all.
	manifestPath := filepath.Join(root, "retirement-manifests.jsonl")
	journalPath := filepath.Join(root, "retirement-phases.jsonl")
	if err := os.WriteFile(manifestPath, []byte(`{"pool":".herd/pool-other","generation":"g1","candidate_sha":"c1","reviewer":"r1","binding_digest":"b1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := NewManifestRetirementAuthority(root, manifestPath, journalPath)

	if err := pool.GC(context.Background(), authority); err == nil || !strings.Contains(err.Error(), "not automatically disposable") {
		t.Fatalf("pool absent from the manifest must refuse GC, got err=%v", err)
	}
	slots, err := pool.Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("refused GC must remove nothing, slots=%d err=%v", len(slots), err)
	}
	decisions, err := pool.GCPlan(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || !decisions[0].Refused || !strings.Contains(decisions[0].Reason, "not automatically disposable") {
		t.Fatalf("dry run must refuse the unknown-scope pool, got %+v", decisions)
	}
}

// TestPoolGCReplacementAtDestructiveBoundarySurvives is the deterministic
// regression for the destructive bug: a replacement that occupies the slot
// path exactly at the destructive boundary (via the injected seam, standing
// in for the racer that otherwise needs a microscopic timing window to
// exploit) must survive untouched, the pass must refuse, and git metadata
// must stay consistent. Under the previous direct git-worktree-remove
// behavior the same injected replacement is silently destroyed and the
// removal is claimed as success.
func TestPoolGCReplacementAtDestructiveBoundarySurvives(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	pool.ProcessInspector = silentCensusInspector()
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	slots, err := pool.Slots()
	if err != nil || len(slots) != 1 {
		t.Fatalf("precondition: 1 clean slot, got %d err=%v", len(slots), err)
	}
	slotPath := slots[0].Path

	// The forged replacement must look exactly like the registered worktree
	// it displaced (same .git pointer, fully clean tree) so that the
	// previous direct-removal behavior has nothing to refuse it on.
	gitPointer, err := os.ReadFile(filepath.Join(slotPath, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	var replacementIno uint64
	pool.OnDestructiveBoundary = func(path string) error {
		scratch := filepath.Join(t.TempDir(), "racer-stole-the-original")
		if err := os.Rename(path, scratch); err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, ".git"), gitPointer, 0o644); err != nil {
			return err
		}
		reset := exec.Command("git", "-C", path, "reset", "--hard")
		if out, resetErr := reset.CombinedOutput(); resetErr != nil {
			return fmt.Errorf("forging clean replacement: %v (%s)", resetErr, strings.TrimSpace(string(out)))
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("stat_t identity unavailable on this platform")
		}
		replacementIno = stat.Ino
		return nil
	}

	gcErr := pool.GC(context.Background(), allowAllRetirementAuthority{})

	// No removal may be claimed for a pass that met a replacement at the
	// destructive boundary.
	if gcErr == nil || !strings.Contains(gcErr.Error(), "replaced at the destructive boundary") {
		t.Fatalf("replacement at the destructive boundary must refuse GC, got err=%v", gcErr)
	}
	// The replacement's content survives untouched at the slot path.
	info, statErr := os.Lstat(slotPath)
	if statErr != nil {
		t.Fatalf("replacement must survive the refused pass, stat err=%v", statErr)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Ino != replacementIno {
		t.Fatalf("the surviving directory is not the replacement (ino %d != %d)", stat.Ino, replacementIno)
	}
	if _, statErr := os.Lstat(filepath.Join(slotPath, ".git")); statErr != nil {
		t.Fatalf("replacement content must be intact, stat err=%v", statErr)
	}
	// Pool state was not mutated by the refused pass.
	after, err := pool.Slots()
	if err != nil || len(after) != 1 || after[0].Path != slotPath {
		t.Fatalf("refused pass must leave pool state intact, slots=%+v err=%v", after, err)
	}
	// Git metadata stays consistent: the registration still exists.
	listed, listErr := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").CombinedOutput()
	if listErr != nil || !strings.Contains(string(listed), slotPath) {
		t.Fatalf("refused pass must keep git registration consistent: %v (%s)", listErr, strings.TrimSpace(string(listed)))
	}
	// Nothing was left parked: the atomic rollback restored the directory.
	entries, readErr := os.ReadDir(filepath.Join(root, ".herd", "pool"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gc-quarantine-") {
			t.Fatalf("refused pass must not park a quarantine dir: %s", e.Name())
		}
	}
}
