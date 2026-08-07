package provider

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// TestFencedCAS_ExclusiveHoldsThroughMutate proves a concurrent CAS with a
// higher fence cannot pass while mutate is in flight (unlock-before-mutate
// pause window closed via WithExclusive).
func TestFencedCAS_ExclusiveHoldsThroughMutate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fences.db")
	// Shared SQLite fence store (two CAS instances, one file).
	storeA, err := NewSQLiteFenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := NewSQLiteFenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	mp := NewMemoryProvider()
	mp.AddTask(testTask("race-1", "FAC-147r", "to-do"))

	casA, err := NewFencedCAS(storeA, mp)
	if err != nil {
		t.Fatal(err)
	}
	casB, err := NewFencedCAS(storeB, mp)
	if err != nil {
		t.Fatal(err)
	}

	rev, err := casA.ReadRevision(ctx, "race-1")
	if err != nil {
		t.Fatal(err)
	}

	var gen2Started atomic.Bool
	var gen1Mutated atomic.Bool
	var gen2Mutated atomic.Bool
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)

	// Gen 1: pass checks, pause inside mutate while exclusive held.
	go func() {
		defer wg.Done()
		_, err := casA.CompareAndSwap(ctx, "race-1", rev, 1, "test-op", func(ctx context.Context) error {
			gen1Mutated.Store(true)
			gen2Started.Store(true) // signal gen2 may try
			<-release
			return mp.UpdateStatus(ctx, "race-1", StatusInProgress)
		})
		if err != nil {
			t.Errorf("gen1 CAS: %v", err)
		}
	}()

	// Wait until gen1 is inside mutate (or timeout).
	deadline := time.Now().Add(2 * time.Second)
	for !gen2Started.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !gen2Started.Load() {
		close(release)
		wg.Wait()
		t.Fatal("gen1 never entered mutate")
	}

	// Gen 2 tries while gen1 still holds exclusive — must block inside
	// WithExclusive, then either succeed or fail revision after gen1 finishes.
	// Use the pre-captured revision so ReadRevision is not called outside
	// exclusive (avoids racy reads of the board while gen1 mutates).
	go func() {
		defer wg.Done()
		_, err := casB.CompareAndSwap(ctx, "race-1", rev, 2, "test-op", func(ctx context.Context) error {
			// If we get here while gen1 still in mutate, exclusive failed.
			if !gen1Mutated.Load() {
				t.Error("gen2 mutate before gen1 started")
			}
			gen2Mutated.Store(true)
			return mp.UpdateStatus(ctx, "race-1", StatusDone)
		})
		_ = err
	}()

	time.Sleep(50 * time.Millisecond)
	// Gen2 must still be blocked inside WithExclusive (not mutated yet).
	if gen2Mutated.Load() {
		close(release)
		wg.Wait()
		t.Fatal("gen2 mutated while gen1 exclusive held — pause window open")
	}
	close(release)
	wg.Wait()

	if !gen1Mutated.Load() {
		t.Fatal("gen1 must have mutated")
	}
}

// TestFencedCAS_TwoProcessesSharedFenceFile_StaleRejected after reclaim
// AdvanceFence on a shared SQLite path (crash-restart friendly durable state).
func TestFencedCAS_TwoProcessesSharedFenceFile_StaleRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mp := NewMemoryProvider()
	mp.AddTask(testTask("shared-1", "FAC-147s", "to-do"))

	stack1, err := OpenClaimStack(dir, mp)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process exit: close stack1, reopen stack2 on same path.
	key := LeaseKey(".", "kaneo", "p1", "FAC-147s")
	lease1, err := stack1.MutateClaimGuarded(ctx, key, "p1", "worker", "worker", "shared-1")
	if err != nil {
		t.Fatalf("claim1: %v", err)
	}
	if err := stack1.Manager.Release(ctx, key, "p1", lease1.Generation); err != nil {
		t.Fatal(err)
	}
	_ = stack1.Close()

	// "Restart" process: new stack on same canonical dir.
	_ = mp.UpdateStatus(ctx, "shared-1", "to-do")
	stack2, err := OpenClaimStack(dir, mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack2.Close()

	lease2, err := stack2.MutateClaimGuarded(ctx, key, "p2", "worker", "worker", "shared-1")
	if err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if lease2.Generation <= lease1.Generation {
		t.Fatalf("gen not advanced: %d vs %d", lease1.Generation, lease2.Generation)
	}

	// Stale process-1 CAS against shared fence DB must reject.
	rev, _ := stack2.CAS.ReadRevision(ctx, "shared-1")
	var mutated int
	_, err = stack2.CAS.CompareAndSwap(ctx, "shared-1", rev, lease1.Generation, "test-op", func(ctx context.Context) error {
		mutated++
		return mp.UpdateStatus(ctx, "shared-1", StatusDone)
	})
	if mutated != 0 {
		t.Fatalf("stale gen mutated after shared reopen; err=%v", err)
	}
	if err == nil || !errors.Is(err, claim.ErrProviderFenceRejected) {
		t.Fatalf("want fence reject, got %v", err)
	}
}

// TestMutateStatusGuarded_RefusesClaimConflict ensures no high+1 preemption.
func TestMutateStatusGuarded_RefusesClaimConflict(t *testing.T) {
	ctx := context.Background()
	mp := NewMemoryProvider()
	mp.AddTask(testTask("conf-1", "FAC-147x", "to-do"))
	stack, err := OpenClaimStack(t.TempDir(), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()
	key := LeaseKey(".", "kaneo", "p1", "FAC-147x")
	if _, err := stack.AcquireLease(ctx, key, "owner-a", "worker", "worker"); err != nil {
		t.Fatal(err)
	}
	// Contender must not mint a generation and mutate.
	_, err = stack.MutateStatusGuarded(ctx, key, "owner-b", "worker", "worker", "conf-1", StatusDone)
	if err == nil {
		t.Fatal("expected refuse on claim conflict")
	}
	got, _ := mp.GetTask(ctx, "conf-1")
	if got.Status == StatusDone {
		t.Fatal("conflict path must not mark done")
	}
}
