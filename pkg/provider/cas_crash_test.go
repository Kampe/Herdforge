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

// TestSQLiteFence_BusyTimeoutBounded proves contenders fail within the
// configured busy_timeout rather than hanging forever while another
// process holds BEGIN IMMEDIATE on the lock DB.
func TestSQLiteFence_BusyTimeoutBounded(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fences.db")
	busy := 150 * time.Millisecond

	a, err := NewSQLiteFenceStoreWithBusy(path, busy)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := NewSQLiteFenceStoreWithBusy(path, busy)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	hold := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = a.WithExclusive(ctx, "t1", func(ctx context.Context) error {
			<-hold
			return nil
		})
	}()
	// Ensure A holds the lock.
	time.Sleep(30 * time.Millisecond)

	start := time.Now()
	err = b.WithExclusive(ctx, "t1", func(ctx context.Context) error { return nil })
	elapsed := time.Since(start)
	close(hold)
	wg.Wait()

	if err == nil {
		t.Fatal("expected busy timeout while A holds exclusive")
	}
	// Bound: should not wait much longer than busy_timeout (+ slack).
	if elapsed > busy+2*time.Second {
		t.Fatalf("busy wait unbounded: elapsed=%s busy=%s err=%v", elapsed, busy, err)
	}
	if elapsed < busy/2 {
		// Very fast failure is OK (immediate SQLITE_BUSY in some modes);
		// only flag if suspiciously instant with nil — already handled.
		t.Logf("contender failed quickly: %s (%v)", elapsed, err)
	}
}

// TestFencedCAS_CrashAfterAdvance_NoDuplicateMutation simulates a process
// that advanced the durable fence then died mid-mutate: reopening the
// shared fence DB must reject the same fence token (no double apply).
func TestFencedCAS_CrashAfterAdvance_NoDuplicateMutation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fencePath := filepath.Join(dir, "fences.db")

	mp := NewMemoryProvider()
	mp.AddTask(testTask("crash-1", "FAC-147cr", "to-do"))

	// Process A: advance fence to 1, mutate once, "crash" (close without
	// rolling back data — data is autocommit).
	storeA, err := NewSQLiteFenceStore(fencePath)
	if err != nil {
		t.Fatal(err)
	}
	casA, err := NewFencedCAS(storeA, mp)
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := casA.ReadRevision(ctx, "crash-1")
	_, err = casA.CompareAndSwap(ctx, "crash-1", rev, 1, "test-op", func(ctx context.Context) error {
		return mp.UpdateStatus(ctx, "crash-1", StatusInProgress)
	})
	if err != nil {
		t.Fatalf("first CAS: %v", err)
	}
	// Crash: close process A stores.
	_ = casA.Close()

	// Process B (restart): same fence token must not re-mutate.
	storeB, err := NewSQLiteFenceStore(fencePath)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	casB, err := NewFencedCAS(storeB, mp)
	if err != nil {
		t.Fatal(err)
	}
	// Restart with CURRENT revision + same token + same opID — must not re-mutate.
	curRev, err := casB.ReadRevision(ctx, "crash-1")
	if err != nil {
		t.Fatal(err)
	}
	var mutated atomic.Int32
	_, err = casB.CompareAndSwap(ctx, "crash-1", curRev, 1, "test-op", func(ctx context.Context) error {
		mutated.Add(1)
		return mp.UpdateStatus(ctx, "crash-1", StatusDone)
	})
	if err != nil {
		t.Fatalf("idempotent retry must succeed: %v", err)
	}
	if mutated.Load() != 0 {
		t.Fatalf("duplicate mutate after crash recovery with current rev")
	}
	got, _ := mp.GetTask(ctx, "crash-1")
	if got.Status == StatusDone {
		t.Fatal("board must not be double-mutated to done")
	}
	_ = rev
}

// TestFencedCAS_StalePausedOwner_CannotMutateAfterReclaim: owner holds
// exclusive mid-mutate; after release, reclaim advances fence; stale
// token cannot apply.
func TestFencedCAS_StalePausedOwner_CannotMutateAfterReclaim(t *testing.T) {
	ctx := context.Background()
	mp := NewMemoryProvider()
	mp.AddTask(testTask("pause-1", "FAC-147p", "to-do"))

	stack := NewTestStack(t, mp)
	key := LeaseKey(".", "kaneo", "p1", "FAC-147p")
	lease1 := MustAcquireLease(t, stack, key, "owner-1", "worker", "pause-1")

	// Simulate reclaim: release + new owner advances fence.
	if err := stack.Manager.Release(ctx, key, "owner-1", lease1.Generation); err != nil {
		t.Fatal(err)
	}
	_ = mp.UpdateStatus(ctx, "pause-1", "to-do")
	lease2 := MustAcquireLease(t, stack, key, "owner-2", "worker", "pause-1")
	if lease2.Generation <= lease1.Generation {
		t.Fatalf("gen %d not advanced past %d", lease2.Generation, lease1.Generation)
	}

	rev, _ := stack.CAS.ReadRevision(ctx, "pause-1")
	var mutated int
	_, err := stack.CAS.CompareAndSwap(ctx, "pause-1", rev, lease1.Generation, "test-op", func(ctx context.Context) error {
		mutated++
		return mp.UpdateStatus(ctx, "pause-1", StatusDone)
	})
	if mutated != 0 || err == nil || !errors.Is(err, claim.ErrProviderFenceRejected) {
		t.Fatalf("stale paused owner must be fence-rejected; mutated=%d err=%v", mutated, err)
	}
}

// TestFencedCAS_AdvanceIsDurableBeforeMutateCompletes: if mutate panics,
// fence high-water is still visible after reopen.
func TestFencedCAS_AdvanceIsDurableBeforeMutateCompletes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fences.db")
	mp := NewMemoryProvider()
	mp.AddTask(testTask("dur-1", "FAC-147d", "to-do"))

	store, err := NewSQLiteFenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cas, err := NewFencedCAS(store, mp)
	if err != nil {
		t.Fatal(err)
	}
	rev, _ := cas.ReadRevision(ctx, "dur-1")
	// mutate "crashes" after side effect via error after status write —
	// fence should already be at 5 from Advance inside exclusive.
	_, err = cas.CompareAndSwap(ctx, "dur-1", rev, 5, "test-op", func(ctx context.Context) error {
		_ = mp.UpdateStatus(ctx, "dur-1", StatusInProgress)
		return errors.New("simulated crash after board write")
	})
	if err == nil {
		t.Fatal("expected mutate error")
	}
	_ = cas.Close()

	store2, err := NewSQLiteFenceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	high, err := store2.Highest(ctx, "dur-1")
	if err != nil {
		t.Fatal(err)
	}
	if high < 5 {
		t.Fatalf("fence high-water=%d want >=5 after advance before failed mutate", high)
	}
}
