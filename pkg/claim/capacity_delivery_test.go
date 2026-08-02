package claim

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ledgerCapacity simulates a real external capacity system with its own
// idempotency ledger, exactly what CapacityCoordinator's doc comment asks
// implementations to provide: Release only actually applies the first
// time it sees a given idempotencyKey; any redelivery of the same key is
// a safe no-op. It separately counts every *call* so tests can tell
// "the coordinator was invoked N times" (delivery attempts) apart from
// "the external effect happened M times" (the deduped, real effect) --
// the whole point of the durable delivery protocol is N can be > 1 while
// M stays exactly 1.
type ledgerCapacity struct {
	mu          sync.Mutex
	callsByKey  map[string]int
	applied     map[string]bool
	effectCount int
}

func newLedgerCapacity() *ledgerCapacity {
	return &ledgerCapacity{callsByKey: map[string]int{}, applied: map[string]bool{}}
}

func (c *ledgerCapacity) Reserve(context.Context, string, string) error { return nil }

func (c *ledgerCapacity) Release(_ context.Context, _ string, idempotencyKey string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callsByKey[idempotencyKey]++
	if c.applied[idempotencyKey] {
		return nil // dedup: already applied, redelivery is a safe no-op.
	}
	c.applied[idempotencyKey] = true
	c.effectCount++
	return nil
}

func (c *ledgerCapacity) callCount(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callsByKey[key]
}

func (c *ledgerCapacity) deduped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.effectCount
}

// TestSQLiteLeaseStore_ClaimCapacityRelease_ConcurrentCallers_ExactlyOneClaimsIt
// isolates the store-level atomic-claim primitive: two independent
// SQLiteLeaseStore handles (two DB connections, standing in for two
// processes) both call ClaimCapacityRelease for the same pending lease at
// the same time. Exactly one of them must see it in its claimed batch.
func TestSQLiteLeaseStore_ClaimCapacityRelease_ConcurrentCallers_ExactlyOneClaimsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.db")
	storeA, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatalf("open storeA: %v", err)
	}
	defer storeA.Close()
	storeB, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatalf("open storeB: %v", err)
	}
	defer storeB.Close()

	ctx := context.Background()
	key := testKey("FAC-18")
	now := time.Now()

	lease, err := storeA.Acquire(ctx, key, "w1", "herd-smith", "/wt", now, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, _, err := storeA.Release(ctx, key, "w1", lease.Generation, now); err != nil {
		t.Fatalf("release: %v", err)
	}
	// storeA.Release only ends the lease lifecycle; capacity is
	// deliberately untouched here (that's ClaimManager's job) so the
	// lease sits pending capacity release for both stores to race on.

	var wg sync.WaitGroup
	var claimedA, claimedB []*Lease
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		claimedA, errA = storeA.ClaimCapacityRelease(ctx, "settlerA", time.Minute, now, nil)
	}()
	go func() {
		defer wg.Done()
		claimedB, errB = storeB.ClaimCapacityRelease(ctx, "settlerB", time.Minute, now, nil)
	}()
	wg.Wait()

	if errA != nil {
		t.Fatalf("storeA claim: %v", errA)
	}
	if errB != nil {
		t.Fatalf("storeB claim: %v", errB)
	}

	total := len(claimedA) + len(claimedB)
	if total != 1 {
		t.Fatalf("expected exactly one store to claim the pending lease, got %d claimed by A and %d claimed by B", len(claimedA), len(claimedB))
	}
}

// TestClaimManager_TwoManagersTwoDBHandles_ConcurrentSettlement_NoDoubleDelivery
// directly reproduces the reviewer's probe: two ClaimManagers (two
// SQLiteLeaseStore handles on the same file, simulating two processes)
// both settling pending capacity for the same lease at the same time
// against a SHARED capacity coordinator (as a real external service would
// be shared). Before this fix, both managers called
// CapacityCoordinator.Release unconditionally for anything pending,
// producing concurrent_pending_release_calls=2 for one lease. With the
// atomic ClaimCapacityRelease claim, only one of them can ever get that
// lease into its batch, so the coordinator is called exactly once.
func TestClaimManager_TwoManagersTwoDBHandles_ConcurrentSettlement_NoDoubleDelivery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.db")
	storeA, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatalf("open storeA: %v", err)
	}
	defer storeA.Close()
	storeB, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatalf("open storeB: %v", err)
	}
	defer storeB.Close()

	ledger := newLedgerCapacity()
	mgrA := NewClaimManager(storeA, WithSettlerID("mgrA"), WithCapacityCoordinator(ledger))
	mgrB := NewClaimManager(storeB, WithSettlerID("mgrB"), WithCapacityCoordinator(ledger))
	ctx := context.Background()
	key := testKey("FAC-19")
	now := time.Now()

	lease, err := storeA.Acquire(ctx, key, "w1", "herd-smith", "/wt", now, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, _, err := storeA.Release(ctx, key, "w1", lease.Generation, now); err != nil {
		t.Fatalf("release: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = mgrA.SettlePendingCapacity(ctx) }()
	go func() { defer wg.Done(); _, _ = mgrB.SettlePendingCapacity(ctx) }()
	wg.Wait()

	wantKey := capacityKey("release", lease)
	if calls := ledger.callCount(wantKey); calls != 1 {
		t.Fatalf("concurrent_pending_release_calls: expected exactly 1 coordinator call for lease %d, got %d", lease.ID, calls)
	}

	claimed, err := storeA.ClaimCapacityRelease(ctx, "verify", time.Minute, now, nil)
	if err != nil {
		t.Fatalf("verify claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("expected the lease to already be settled (Acked), got %d still pending", len(claimed))
	}
}

// TestClaimManager_CrashBetweenSendAndMark_EffectiveExactlyOnceAtCoordinator
// simulates the crash window a purely-local CAS claim cannot close: a
// settler successfully calls CapacityCoordinator.Release, then crashes
// before it can Ack. A recovering settler reclaims the now-stale claim
// after staleAfter passes and calls Release again -- unavoidable
// at-least-once redelivery -- using the SAME idempotency key. This proves
// the external effect only happens once (via the coordinator's own
// dedup ledger) even though the coordinator was genuinely invoked twice,
// i.e. effectively-exactly-once delivery at the coordinator boundary.
func TestClaimManager_CrashBetweenSendAndMark_EffectiveExactlyOnceAtCoordinator(t *testing.T) {
	store := newTestStore(t)
	ledger := newLedgerCapacity()
	clk := newClock(time.Now())
	crashed := NewClaimManager(store, WithSettlerID("crashed-settler"), WithCapacityCoordinator(ledger),
		WithClock(clk.now), WithCapacityClaimTimeout(time.Minute))
	recovering := NewClaimManager(store, WithSettlerID("recovering-settler"), WithCapacityCoordinator(ledger),
		WithClock(clk.now), WithCapacityClaimTimeout(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-20")

	lease, err := store.Acquire(ctx, key, "w1", "herd-smith", "/wt", clk.now(), time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, _, err := store.Release(ctx, key, "w1", lease.Generation, clk.now()); err != nil {
		t.Fatalf("release: %v", err)
	}

	// "crashed" settler: claims the release, calls the coordinator
	// (which durably applies it on the coordinator's side), then
	// deliberately never Acks -- simulating a process death right there.
	claimed, err := store.ClaimCapacityRelease(ctx, "crashed-settler", time.Minute, clk.now(), nil)
	if err != nil {
		t.Fatalf("crashed settler claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected the crashed settler to claim exactly 1 lease, got %d", len(claimed))
	}
	idKey := capacityKey("release", claimed[0])
	if err := ledger.Release(ctx, claimed[0].Role, idKey); err != nil {
		t.Fatalf("crashed settler's coordinator call: %v", err)
	}
	_ = crashed // the crashed manager is never used again past this point.

	if calls := ledger.callCount(idKey); calls != 1 {
		t.Fatalf("expected 1 call before recovery, got %d", calls)
	}
	if applied := ledger.deduped(); applied != 1 {
		t.Fatalf("expected the effect to have applied once already, got %d", applied)
	}

	// Time passes the claim-staleness window; the crashed settler's claim
	// is now reclaimable.
	clk.advance(2 * time.Minute)

	// The recovering settler sweeps and reclaims + redelivers.
	settled, err := recovering.SettlePendingCapacity(ctx)
	if err != nil {
		t.Fatalf("recovering settlement: %v", err)
	}
	if len(settled) != 1 {
		t.Fatalf("expected the recovering settler to settle exactly 1 lease, got %d", len(settled))
	}

	// The coordinator really was called twice (genuine at-least-once
	// redelivery, not faked)...
	if calls := ledger.callCount(idKey); calls != 2 {
		t.Fatalf("expected 2 total coordinator calls (crashed + recovered) for the same idempotency key, got %d", calls)
	}
	// ...but the external effect only happened once, because both calls
	// carried the same stable idempotency key and the coordinator deduped.
	if applied := ledger.deduped(); applied != 1 {
		t.Fatalf("expected effective-exactly-once: the external effect must have applied exactly once despite 2 delivery attempts, got %d", applied)
	}

	// And the lease is durably marked settled now.
	claimedAgain, err := store.ClaimCapacityRelease(ctx, "verify", time.Minute, clk.now(), nil)
	if err != nil {
		t.Fatalf("verify claim: %v", err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("expected the lease to be durably Acked after recovery, got %d still pending", len(claimedAgain))
	}
}

// TestClaimManager_CapacityKey_StableAcrossRetries_DistinctAcrossGenerations
// proves the idempotency key is bound to lease ID/generation as the
// review requires: stable across repeated calls for the same lease, and
// distinct once a key is reclaimed at a new generation.
func TestClaimManager_CapacityKey_StableAcrossRetries_DistinctAcrossGenerations(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	capCoord := newCountingCapacity()
	mgr := NewClaimManager(store, WithClock(clk.now), WithTTL(time.Minute), WithCapacityCoordinator(capCoord))
	ctx := context.Background()
	key := testKey("FAC-21")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("release: %v", err)
	}
	// A second, fully-idempotent release replay should not add a new key.
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("idempotent release replay: %v", err)
	}

	if len(capCoord.releaseKeys) != 1 {
		t.Fatalf("expected exactly 1 release call/key for one lease's lifecycle, got %d: %v", len(capCoord.releaseKeys), capCoord.releaseKeys)
	}
	firstKey := capCoord.releaseKeys[0]

	clk.advance(time.Second)
	next, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if err := mgr.Release(ctx, key, "w2", next.Generation); err != nil {
		t.Fatalf("release w2: %v", err)
	}

	if len(capCoord.releaseKeys) != 2 {
		t.Fatalf("expected a second, distinct release key after reclaim, got %d: %v", len(capCoord.releaseKeys), capCoord.releaseKeys)
	}
	if capCoord.releaseKeys[1] == firstKey {
		t.Fatalf("expected the reclaimed lease's generation to produce a distinct idempotency key, both were %q", firstKey)
	}
}
