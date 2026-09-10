package worktree

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

type poolProcessInspectorFunc func(context.Context, string) (resources.ProcessUsage, error)

func (f poolProcessInspectorFunc) InUse(ctx context.Context, path string) (resources.ProcessUsage, error) {
	return f(ctx, path)
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

	if err := pool.GC(context.Background()); err == nil || !strings.Contains(err.Error(), "registered git worktree") {
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

	if err := pool.GC(context.Background()); err == nil || !strings.Contains(err.Error(), "outside the pool root") {
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

	if err := pool.GC(context.Background()); err == nil || !strings.Contains(err.Error(), "symlink") {
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

	if err := pool.GC(context.Background()); err == nil || !strings.Contains(err.Error(), "dirty") {
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

	if err := pool.GC(context.Background()); err == nil || !strings.Contains(err.Error(), "dirty") {
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

	if err := pool.GC(context.Background()); err == nil || !strings.Contains(err.Error(), "not reachable from base") {
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

	if err := pool.GC(context.Background()); err == nil || !strings.Contains(err.Error(), "leased") {
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

func TestPoolGCSucceedsAndLeavesPoolConsistentForEnsure(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 2)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	before, err := pool.Slots()
	if err != nil || len(before) != 2 {
		t.Fatalf("precondition: 2 clean slots, got %d err=%v", len(before), err)
	}

	if err := pool.GC(context.Background()); err != nil {
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
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pool.Root, "pool-02", "dirty.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	decisions, err := pool.GCPlan(context.Background())
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

	err = pool.GC(context.Background())
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

	err := pool.GC(context.Background())
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

	err := pool.GC(context.Background())
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

	if err := pool.GC(context.Background()); err != nil {
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

	err := pool.GC(context.Background())
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
