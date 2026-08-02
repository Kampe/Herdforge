package claim

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(taskRef string) LeaseKey {
	return LeaseKey{Repo: "Herdforge", Provider: "kaneo", Project: "FAC", TaskRef: taskRef}
}

func newTestStore(t *testing.T) *SQLiteLeaseStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leases.db")
	store, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// clock is a mutex-guarded fake clock so tests can move time forward
// deterministically instead of sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(start time.Time) *clock { return &clock{t: start} }
func (c *clock) now() time.Time       { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// countingCapacity records Reserve/Release calls per role so tests can
// assert exactly-once release accounting.
type countingCapacity struct {
	mu       sync.Mutex
	reserved map[string]int
	released map[string]int
	failNext bool
}

func newCountingCapacity() *countingCapacity {
	return &countingCapacity{reserved: map[string]int{}, released: map[string]int{}}
}

func (c *countingCapacity) Reserve(_ context.Context, role string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failNext {
		c.failNext = false
		return errors.New("capacity exhausted")
	}
	c.reserved[role]++
	return nil
}

func (c *countingCapacity) Release(_ context.Context, role string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released[role]++
	return nil
}

func (c *countingCapacity) counts(role string) (reserved, released int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserved[role], c.released[role]
}

func TestClaimManager_ConcurrentClaims_ExactlyOneWinner(t *testing.T) {
	store := newTestStore(t)
	mgr := NewClaimManager(store)
	ctx := context.Background()
	key := testKey("FAC-15")

	var wg sync.WaitGroup
	var successCount int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			_, err := mgr.Claim(ctx, ClaimRequest{
				Key: key, OwnerID: fmt.Sprintf("worker-%d", workerID),
				Role: "herd-smith", TaskRole: "herd-smith", WorktreePath: "/wt",
			})
			if err == nil {
				atomic.AddInt64(&successCount, 1)
			}
		}(i)
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful claim out of 20 concurrent requests, got %d", successCount)
	}

	claimed, err := mgr.IsClaimed(ctx, key)
	if err != nil {
		t.Fatalf("IsClaimed: %v", err)
	}
	if !claimed {
		t.Fatal("expected key to be claimed")
	}
}

// TestSQLiteLeaseStore_CrossProcess_ExactlyOneWinner opens two independent
// SQLiteLeaseStore instances (two *sql.DB, like two OS processes) against
// the same database file and races Acquire across both, proving atomicity
// comes from the shared file, not from any in-process mutex.
func TestSQLiteLeaseStore_CrossProcess_ExactlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.db")
	procA, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatalf("open procA: %v", err)
	}
	defer procA.Close()
	procB, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatalf("open procB: %v", err)
	}
	defer procB.Close()

	ctx := context.Background()
	key := testKey("FAC-120")
	now := time.Now()

	var wg sync.WaitGroup
	results := make(chan error, 40)
	race := func(store *SQLiteLeaseStore, owner string) {
		defer wg.Done()
		_, err := store.Acquire(ctx, key, owner, "herd-smith", "/wt", now, time.Minute)
		results <- err
	}
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go race(procA, fmt.Sprintf("procA-%d", i))
		go race(procB, fmt.Sprintf("procB-%d", i))
	}
	wg.Wait()
	close(results)

	var wins, conflicts int
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.As(err, new(*ClaimConflictError)), errors.Is(err, ErrAlreadyClaimed):
			conflicts++
		default:
			t.Fatalf("unexpected acquire error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner across both simulated processes, got %d (conflicts=%d)", wins, conflicts)
	}
	if wins+conflicts != 40 {
		t.Fatalf("expected 40 accounted attempts, got wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestClaimManager_RoleEnforcement(t *testing.T) {
	store := newTestStore(t)
	mgr := NewClaimManager(store)
	ctx := context.Background()

	_, err := mgr.Claim(ctx, ClaimRequest{Key: testKey("FAC-1"), OwnerID: "w1", Role: "herd-smith", TaskRole: ""})
	if !errors.Is(err, ErrUnlabeledTask) {
		t.Fatalf("expected ErrUnlabeledTask, got %v", err)
	}

	_, err = mgr.Claim(ctx, ClaimRequest{Key: testKey("FAC-1"), OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-reviewer"})
	if !errors.Is(err, ErrRoleMismatch) {
		t.Fatalf("expected ErrRoleMismatch, got %v", err)
	}

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: testKey("FAC-1"), OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("expected exact role match to succeed: %v", err)
	}
	if lease.Role != "herd-smith" {
		t.Fatalf("expected lease role herd-smith, got %s", lease.Role)
	}
}

func TestClaimManager_RenewAndFencing(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	mgr := NewClaimManager(store, WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-2")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	clk.advance(30 * time.Second)
	renewed, err := mgr.Renew(ctx, key, "w1", lease.Generation)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("expected renew to push expiry forward, got %v vs original %v", renewed.ExpiresAt, lease.ExpiresAt)
	}

	// A stale generation must not be able to renew.
	if _, err := mgr.Renew(ctx, key, "w1", lease.Generation-1); !errors.Is(err, ErrStaleGeneration) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected stale generation renew to be rejected, got %v", err)
	}

	// Let the lease expire, someone else reclaims it (new generation)...
	clk.advance(2 * time.Minute)
	next, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("reclaim after expiry: %v", err)
	}
	if next.Generation <= lease.Generation {
		t.Fatalf("expected monotonically increasing generation, got %d after %d", next.Generation, lease.Generation)
	}

	// ...and the original holder's stale generation can no longer renew or release.
	if _, err := mgr.Renew(ctx, key, "w1", lease.Generation); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("expected ErrStaleGeneration on renew after reclaim, got %v", err)
	}
	if err := mgr.Release(ctx, key, "w1", lease.Generation); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("expected ErrStaleGeneration on release after reclaim, got %v", err)
	}
}

func TestClaimManager_IdempotentRelease_CapacityExactlyOnce(t *testing.T) {
	store := newTestStore(t)
	cap := newCountingCapacity()
	mgr := NewClaimManager(store, WithCapacityCoordinator(cap))
	ctx := context.Background()
	key := testKey("FAC-3")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if r, rel := cap.counts("herd-smith"); r != 1 || rel != 0 {
		t.Fatalf("expected 1 reserve / 0 release after claim, got %d/%d", r, rel)
	}

	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("idempotent second release should not error: %v", err)
	}
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("idempotent third release should not error: %v", err)
	}

	if r, rel := cap.counts("herd-smith"); r != 1 || rel != 1 {
		t.Fatalf("expected exactly 1 reserve and 1 release despite 3 release calls, got %d/%d", r, rel)
	}

	claimed, err := mgr.IsClaimed(ctx, key)
	if err != nil {
		t.Fatalf("IsClaimed: %v", err)
	}
	if claimed {
		t.Fatal("expected key to be free after release")
	}
}

func TestClaimManager_CapacityReserveFailure_CompensatesLease(t *testing.T) {
	store := newTestStore(t)
	cap := newCountingCapacity()
	cap.failNext = true
	mgr := NewClaimManager(store, WithCapacityCoordinator(cap))
	ctx := context.Background()
	key := testKey("FAC-4")

	_, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err == nil {
		t.Fatal("expected claim to fail when capacity reservation fails")
	}

	claimed, err := mgr.IsClaimed(ctx, key)
	if err != nil {
		t.Fatalf("IsClaimed: %v", err)
	}
	if claimed {
		t.Fatal("expected the durable lease to be released after capacity reservation failed")
	}

	// A second worker (fresh capacity call, no failure injected) can now claim.
	if _, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith"}); err != nil {
		t.Fatalf("expected key free for a new claim after compensation, got %v", err)
	}
}

func TestClaimManager_ExpiryAndActiveClaims(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	cap := newCountingCapacity()
	mgr := NewClaimManager(store, WithClock(clk.now), WithTTL(time.Minute), WithCapacityCoordinator(cap))
	ctx := context.Background()

	live := testKey("FAC-5")
	stale := testKey("FAC-6")

	if _, err := mgr.Claim(ctx, ClaimRequest{Key: live, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"}); err != nil {
		t.Fatalf("claim live: %v", err)
	}
	if _, err := mgr.Claim(ctx, ClaimRequest{Key: stale, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith"}); err != nil {
		t.Fatalf("claim stale: %v", err)
	}

	clk.advance(2 * time.Minute) // both TTLs pass

	claims, err := mgr.ActiveClaims(ctx)
	if err != nil {
		t.Fatalf("active claims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("expected 0 active claims after TTL expiry, got %d", len(claims))
	}

	expired, err := mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if len(expired) != 2 {
		t.Fatalf("expected 2 leases transitioned to expired, got %d", len(expired))
	}
	if r, rel := cap.counts("herd-smith"); r != 2 || rel != 2 {
		t.Fatalf("expected capacity released exactly once per expired lease, got reserve=%d release=%d", r, rel)
	}

	// A second sweep must not re-transition (and must not double-release capacity).
	again, err := mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale (second sweep): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("expected second sweep to find nothing new, got %d", len(again))
	}
	if r, rel := cap.counts("herd-smith"); r != 2 || rel != 2 {
		t.Fatalf("expected capacity counts unchanged after redundant sweep, got reserve=%d release=%d", r, rel)
	}

	// The key is free again after expiry.
	if _, err := mgr.Claim(ctx, ClaimRequest{Key: live, OwnerID: "w3", Role: "herd-smith", TaskRole: "herd-smith"}); err != nil {
		t.Fatalf("expected expired key reclaimable, got %v", err)
	}
}

func TestClaimManager_OperatorHold_PreventsExpiry(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	mgr := NewClaimManager(store, WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-7")

	if _, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := mgr.Hold(ctx, key, true); err != nil {
		t.Fatalf("hold: %v", err)
	}

	clk.advance(10 * time.Minute) // well past TTL

	claimed, err := mgr.IsClaimed(ctx, key)
	if err != nil {
		t.Fatalf("IsClaimed: %v", err)
	}
	if !claimed {
		t.Fatal("expected held lease to survive past TTL")
	}

	expired, err := mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("expected held lease to be skipped by ExpireStale, got %d transitioned", len(expired))
	}

	// A competing claim must still be rejected while held.
	if _, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith"}); err == nil {
		t.Fatal("expected held lease to block a competing claim")
	}

	if _, err := mgr.Hold(ctx, key, false); err != nil {
		t.Fatalf("unhold: %v", err)
	}
	expired, err = mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale after unhold: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("expected unheld, past-TTL lease to expire, got %d", len(expired))
	}
}
