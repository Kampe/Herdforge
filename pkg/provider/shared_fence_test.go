package provider

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// TestSharedFenceAuthority_TwoProcessesSameClaimDir proves the deployment
// invariant: CanonicalClaimDir / shared fences.db is the multi-process
// authority. Gen2 on process B advances high-water; process A's gen1 CAS
// is fence-rejected with zero board effect.
func TestSharedFenceAuthority_TwoProcessesSameClaimDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	claimDir := filepath.Join(dir, ".herd", "claim")

	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "sf1", Ref: "FAC-SF", Title: "t", Status: StatusInReview, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now, Labels: []string{"worker"}})

	stackA, err := OpenClaimStack(claimDir, mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stackA.Close()
	stackB, err := OpenClaimStack(claimDir, mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stackB.Close()

	key := claim.LeaseKey{Repo: dir, Provider: "memory", Project: "p", TaskRef: "FAC-SF"}
	// Process A: gen1
	leaseA, err := stackA.AcquireLease(ctx, key, "owner-a", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := stackA.CAS.AdvanceFence(ctx, "sf1", leaseA.Generation); err != nil {
		t.Fatal(err)
	}
	// Release A so B can reclaim (simulates TTL/handback).
	if err := stackA.Manager.Release(ctx, key, "owner-a", leaseA.Generation); err != nil {
		t.Fatal(err)
	}

	// Process B: gen2 reclaim + done
	leaseB, err := stackB.AcquireLease(ctx, key, "owner-b", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if leaseB.Generation <= leaseA.Generation {
		t.Fatalf("want gen > %d got %d", leaseA.Generation, leaseB.Generation)
	}
	if err := stackB.CAS.AdvanceFence(ctx, "sf1", leaseB.Generation); err != nil {
		t.Fatal(err)
	}
	if err := stackB.Board.MutateStatus(ctx, stackB.Manager, key, "owner-b", leaseB.Generation, "sf1", StatusDone); err != nil {
		t.Fatal(err)
	}
	got, _ := mp.GetTask(ctx, "sf1")
	if NormalizeStatus(got.Status) != StatusDone {
		t.Fatalf("status=%s", got.Status)
	}

	// Stale gen1 mutation from A must be fence-rejected (shared high-water).
	// Re-open A path: try Complete under gen1 without live lease — use CAS directly.
	rev, _ := stackA.CAS.ReadRevision(ctx, "sf1")
	mutated := 0
	_, err = stackA.CAS.CompareAndSwap(ctx, "sf1", rev, leaseA.Generation, "stale-op-gen1", func(ctx context.Context) error {
		mutated++
		return mp.UpdateStatus(ctx, "sf1", StatusToDo)
	})
	if err == nil {
		t.Fatal("stale gen1 CAS must fail on shared fence authority")
	}
	if mutated != 0 {
		t.Fatalf("stale gen1 must not mutate board (mutated=%d)", mutated)
	}
	got, _ = mp.GetTask(ctx, "sf1")
	if NormalizeStatus(got.Status) != StatusDone {
		t.Fatalf("board must stay done, got %s", got.Status)
	}
	_ = claim.ErrProviderFenceRejected
}

// TestSharedFenceAuthority_IndependentClaimDirDoesNotShare proves two
// independent claim dirs have separate high-water (deployment: multi-host
// without shared .herd/claim is NOT the FAC-147 authority boundary).
func TestSharedFenceAuthority_IndependentClaimDirDoesNotShare(t *testing.T) {
	ctx := context.Background()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "id1", Ref: "FAC-ID", Title: "t", Status: StatusInReview, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})

	a, err := OpenClaimStack(filepath.Join(t.TempDir(), "claim-a"), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenClaimStack(filepath.Join(t.TempDir(), "claim-b"), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.CAS.AdvanceFence(ctx, "id1", 5); err != nil {
		t.Fatal(err)
	}
	// B has independent store — high is 0 until it advances.
	highB, err := b.Fences.Highest(ctx, "id1")
	if err != nil {
		t.Fatal(err)
	}
	if highB != 0 {
		t.Fatalf("independent claim dir must not see peer high-water; highB=%d (deploy shared .herd/claim for multi-host)", highB)
	}
}
