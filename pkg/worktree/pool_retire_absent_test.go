package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// retireFixture prepares a one-slot pool whose single slot is released, with
// the exact release identity RetireExact authenticates. It returns the slot
// name, path, release lease id, and release generation.
func retireFixture(t *testing.T) (*Pool, string, string, string, int64) {
	t.Helper()
	root := t.TempDir()
	initRepo(t, root)
	pool := NewPool(root, filepath.Join(root, ".herd", "pool"), 1)
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	lease, err := pool.Lease(context.Background(), "review")
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if err := pool.Release(context.Background(), lease.LeaseID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	state, err := pool.readState()
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if len(state.Slots) != 1 {
		t.Fatalf("slots = %d, want 1", len(state.Slots))
	}
	slot := state.Slots[0]
	if slot.LeaseID != "" || slot.LastReleaseLeaseID == "" {
		t.Fatalf("slot is not in the released state: %+v", slot)
	}
	return pool, slot.Name, slot.Path, slot.LastReleaseLeaseID, slot.LastReleaseGeneration
}

// The FAC-795 repro: the exact released surface is already absent AND git no
// longer registers it (a prior exact retirement completed its filesystem and
// registration steps but not the state write, or something external removed
// it). Retirement must complete idempotently under the pool lock, and a
// second identical call must be a safe no-op.
func TestRetireExactCompletesWhenSurfaceAlreadyAbsent(t *testing.T) {
	pool, slotName, slotPath, leaseID, generation := retireFixture(t)
	// Simulate the earlier completed removal without the pool state write.
	if err := os.RemoveAll(slotPath); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", pool.RepoRoot, "worktree", "remove", "--force", slotPath).CombinedOutput(); err != nil {
		t.Fatalf("fixture deregistration failed: %v (%s)", err, out)
	}
	ctx := context.Background()
	if err := pool.RetireExact(ctx, slotName, slotPath, leaseID, generation); err != nil {
		t.Fatalf("absent surface must complete idempotently, got %v", err)
	}
	state, err := pool.readState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Slots) != 0 {
		t.Fatalf("completed retirement must drop the slot, %d remain", len(state.Slots))
	}
	// A repeated exact transaction is a safe no-op.
	if err := pool.RetireExact(ctx, slotName, slotPath, leaseID, generation); err != nil {
		t.Fatalf("repeat retirement must be a safe no-op, got %v", err)
	}
}

// The directory is gone but git still registers the path. That state is
// corrupt or ambiguous — the registration may belong to work only a full
// prune would disown — so retirement must refuse and keep the slot.
func TestRetireExactRefusesRegisteredButMissingPath(t *testing.T) {
	pool, slotName, slotPath, leaseID, generation := retireFixture(t)
	if err := os.RemoveAll(slotPath); err != nil {
		t.Fatal(err)
	}
	err := pool.RetireExact(context.Background(), slotName, slotPath, leaseID, generation)
	if err == nil {
		t.Fatal("registered-but-missing path must refuse")
	}
	if !strings.Contains(err.Error(), "still registered") {
		t.Fatalf("refusal must name the ambiguity, got %v", err)
	}
	state, readErr := pool.readState()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(state.Slots) != 1 {
		t.Fatalf("refused retirement must keep the slot, %d remain", len(state.Slots))
	}
}

// Something else now occupies the path and git no longer registers it. That
// content is not owned by this slot record; blanket removal would destroy it.
func TestRetireExactRefusesUnregisteredReplacementContent(t *testing.T) {
	pool, slotName, slotPath, leaseID, generation := retireFixture(t)
	if err := os.RemoveAll(slotPath); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", pool.RepoRoot, "worktree", "remove", "--force", slotPath).CombinedOutput(); err != nil {
		t.Fatalf("fixture deregistration failed: %v (%s)", err, out)
	}
	if err := os.MkdirAll(slotPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotPath, "someone-elses.txt"), []byte("unowned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pool.RetireExact(context.Background(), slotName, slotPath, leaseID, generation); err == nil {
		t.Fatal("unregistered replacement content must refuse")
	}
	if _, err := os.Stat(filepath.Join(slotPath, "someone-elses.txt")); err != nil {
		t.Fatal("refused retirement destroyed the unowned content")
	}
}

// Wrong generation and stale release nonce keep the existing identity
// refusals; absence never bypasses the release identity checks.
func TestRetireExactRefusesStaleIdentityEvenWhenAbsent(t *testing.T) {
	pool, slotName, slotPath, leaseID, generation := retireFixture(t)
	if err := os.RemoveAll(slotPath); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", pool.RepoRoot, "worktree", "remove", "--force", slotPath).CombinedOutput(); err != nil {
		t.Fatalf("fixture deregistration failed: %v (%s)", err, out)
	}
	ctx := context.Background()
	if err := pool.RetireExact(ctx, slotName, slotPath, leaseID, generation+1); err == nil {
		t.Fatal("wrong generation must refuse")
	}
	if err := pool.RetireExact(ctx, slotName, slotPath, leaseID+"-stale", generation); err == nil {
		t.Fatal("stale release nonce must refuse")
	}
	state, err := pool.readState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Slots) != 1 {
		t.Fatalf("stale-identity refusals must keep the slot, %d remain", len(state.Slots))
	}
}
