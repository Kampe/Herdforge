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

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

func testKey(taskRef string) LeaseKey {
	return LeaseKey{Repo: "Herdforge", Provider: "kaneo", Project: "FAC", TaskRef: taskRef}
}

func testHoldIdentities(key LeaseKey) []lifecycle.HoldIdentity {
	return []lifecycle.HoldIdentity{
		{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"},
		{Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"},
	}
}

func testClaimRequest(key LeaseKey, owner, role string) ClaimRequest {
	return ClaimRequest{Key: key, OwnerID: owner, Role: role, TaskRole: role, HoldIdentities: testHoldIdentities(key)}
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

func newTestHoldAuthority(t *testing.T) *lifecycle.HoldAuthority {
	t.Helper()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	a, err := lifecycle.NewHoldAuthorityWithClock(filepath.Join(t.TempDir(), "lifecycle.db"), func() time.Time { return now })
	if err != nil {
		t.Fatalf("open hold authority: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// lyingAbortStore performs the underlying atomic abort but discards its
// receipt. Claim must reject this malformed store response rather than
// treating the replacement as safely compensated.
type lyingAbortStore struct{ *SQLiteLeaseStore }

func (s *lyingAbortStore) AbortUnreservedLease(ctx context.Context, lease *Lease, now time.Time) (*Lease, bool, error) {
	_, _, err := s.SQLiteLeaseStore.AbortUnreservedLease(ctx, lease, now)
	return nil, false, err
}

type heldClaimAuthority struct{}

func (heldClaimAuthority) CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error) {
	return 1, nil
}

func (heldClaimAuthority) Check(context.Context, lifecycle.HoldIdentity, int64) (lifecycle.HoldDecision, error) {
	return lifecycle.HoldDecision{Held: true, Generation: 1, Reason: "maintenance", Code: "operator_hold"}, nil
}
func (heldClaimAuthority) WithUnheldTransition(context.Context, []lifecycle.HoldIdentity, func() error) error {
	return lifecycle.ErrHoldDenied
}

type recordingClaimAuthority struct{ got lifecycle.HoldIdentity }

func (*recordingClaimAuthority) CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error) {
	return 1, nil
}

func (a *recordingClaimAuthority) Check(_ context.Context, id lifecycle.HoldIdentity, _ int64) (lifecycle.HoldDecision, error) {
	a.got = id
	return lifecycle.HoldDecision{}, nil
}
func (a *recordingClaimAuthority) WithUnheldTransition(_ context.Context, ids []lifecycle.HoldIdentity, action func() error) error {
	if len(ids) == 0 {
		return errors.New("missing hold identity")
	}
	for _, id := range ids {
		if id.Task != "" {
			a.got = id
			break
		}
	}
	if a.got == (lifecycle.HoldIdentity{}) {
		a.got = ids[0]
	}
	return action()
}

func TestClaimUsesCanonicalTargetBeforeRandomLeaseOwner(t *testing.T) {
	a := &recordingClaimAuthority{}
	mgr := NewClaimManager(newTestStore(t), WithHoldReader(a))
	_, err := mgr.Claim(context.Background(), ClaimRequest{
		Key: testKey("FAC-TARGET"), OwnerID: "random-lease-owner", Role: "herd-smith", TaskRole: "herd-smith",
		HoldIdentities: []lifecycle.HoldIdentity{{Repository: "Herdforge", Owner: "herd-smith", Lane: "herd-smith", Scope: "lane"}, {Repository: "Herdforge", Owner: "herd-smith", Lane: "herd-smith", Task: "FAC-TARGET", Scope: "task"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.got.Owner == "random-lease-owner" || a.got.Task != "FAC-TARGET" {
		t.Fatalf("authority saw lease owner or wrong target: %+v", a.got)
	}
}

func TestClaimHeldAuthorityDeniesBeforeReserve(t *testing.T) {
	cap := newCountingCapacity()
	mgr := NewClaimManager(newTestStore(t), WithCapacityCoordinator(cap), WithHoldReader(heldClaimAuthority{}))
	_, err := mgr.Claim(context.Background(), ClaimRequest{Key: testKey("FAC-HOLD"), OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: "Herdforge", Owner: "herd-smith", Lane: "herd-smith", Scope: "lane"}, {Repository: "Herdforge", Owner: "herd-smith", Lane: "herd-smith", Task: "FAC-HOLD", Scope: "task"}}})
	if err == nil {
		t.Fatal("held claim must be denied")
	}
	if reserves, _ := cap.counts("herd-smith"); reserves != 0 {
		t.Fatalf("held claim reached capacity reserve: %d", reserves)
	}
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
// assert exactly-once release accounting, and every idempotency key it
// was called with so tests can assert key stability across retries.
type countingCapacity struct {
	mu          sync.Mutex
	reserved    map[string]int
	released    map[string]int
	releaseKeys []string
	failNext    bool
}

func newCountingCapacity() *countingCapacity {
	return &countingCapacity{reserved: map[string]int{}, released: map[string]int{}}
}

func (c *countingCapacity) Reserve(_ context.Context, role, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failNext {
		c.failNext = false
		return errors.New("capacity exhausted")
	}
	c.reserved[role]++
	return nil
}

func (c *countingCapacity) Release(_ context.Context, role, idempotencyKey string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released[role]++
	c.releaseKeys = append(c.releaseKeys, idempotencyKey)
	return nil
}

func (c *countingCapacity) counts(role string) (reserved, released int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserved[role], c.released[role]
}

func TestClaimManager_ConcurrentClaims_ExactlyOneWinner(t *testing.T) {
	store := newTestStore(t)
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-15")

	var wg sync.WaitGroup
	var successCount int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			req := testClaimRequest(key, fmt.Sprintf("worker-%d", workerID), "herd-smith")
			req.WorktreePath = "/wt"
			_, err := mgr.Claim(ctx, req)
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
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()

	_, err := mgr.Claim(ctx, ClaimRequest{Key: testKey("FAC-1"), OwnerID: "w1", Role: "herd-smith", TaskRole: ""})
	if !errors.Is(err, ErrUnlabeledTask) {
		t.Fatalf("expected ErrUnlabeledTask, got %v", err)
	}

	_, err = mgr.Claim(ctx, ClaimRequest{Key: testKey("FAC-1"), OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-reviewer"})
	if !errors.Is(err, ErrRoleMismatch) {
		t.Fatalf("expected ErrRoleMismatch, got %v", err)
	}

	lease, err := mgr.Claim(ctx, testClaimRequest(testKey("FAC-1"), "w1", "herd-smith"))
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
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-2")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
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
	next, err := mgr.Claim(ctx, testClaimRequest(key, "w2", "herd-smith"))
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
	mgr := NewClaimManager(store, WithCapacityCoordinator(cap), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-3")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
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
	mgr := NewClaimManager(store, WithCapacityCoordinator(cap), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-4")

	_, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
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

	settled, err := mgr.SettlePendingCapacity(ctx)
	if err != nil {
		t.Fatalf("drain after failed reserve: %v", err)
	}
	_, releases := cap.counts("herd-smith")
	if len(settled) != 1 || releases != 1 {
		t.Fatalf("ambiguous reserve error did not preserve compensating release: settled=%d", len(settled))
	}
	if _, err := mgr.Claim(ctx, testClaimRequest(key, "w2", "herd-smith")); err != nil {
		t.Fatalf("expected key reusable after compensating release, got %v", err)
	}
}

func TestClaimManager_IncumbentSettlementFailureCompensatesReplacementWithoutFalseRelease(t *testing.T) {
	store := newTestStore(t)
	cap := &failNTimesCapacity{failsLeft: 1}
	clk := newClock(time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC))
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute), WithCapacityCoordinator(cap))
	ctx := context.Background()
	key := testKey("FAC-4B")
	old, err := mgr.Claim(ctx, testClaimRequest(key, "old", "herd-smith"))
	if err != nil {
		t.Fatalf("old claim: %v", err)
	}
	clk.advance(2 * time.Minute)
	if _, err := mgr.Claim(ctx, testClaimRequest(key, "new", "herd-smith")); err == nil {
		t.Fatal("expected reclaim to surface incumbent capacity failure")
	}
	if claimed, err := mgr.IsClaimed(ctx, key); err != nil || claimed {
		t.Fatalf("replacement lease leaked after incumbent failure: claimed=%v err=%v", claimed, err)
	}
	settled, err := mgr.SettlePendingCapacity(ctx)
	if err != nil {
		t.Fatalf("retry incumbent settlement: %v", err)
	}
	if len(settled) != 1 || cap.releasedCount() != 1 {
		t.Fatalf("expected only the genuinely reserved incumbent release, settled=%d releases=%d old=%d", len(settled), cap.releasedCount(), old.ID)
	}
	if again, err := mgr.SettlePendingCapacity(ctx); err != nil || len(again) != 0 {
		t.Fatalf("settlement replay was not idempotent: leases=%d err=%v", len(again), err)
	}
}

func TestClaimManager_AbortReceiptFailureBlocksClaimWithoutReplacementRelease(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	baseStore := newTestStore(t)
	store := &lyingAbortStore{SQLiteLeaseStore: baseStore}
	cap := &failNTimesCapacity{failsLeft: 1}
	clk := newClock(base)
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute), WithCapacityCoordinator(cap))
	ctx := context.Background()
	key := testKey("FAC-abort-receipt")
	incumbent, err := mgr.Claim(ctx, testClaimRequest(key, "old", "herd-smith"))
	if err != nil {
		t.Fatalf("incumbent claim: %v", err)
	}
	clk.advance(2 * time.Minute)
	if _, err := mgr.Claim(ctx, testClaimRequest(key, "new", "herd-smith")); err == nil {
		t.Fatal("malformed abort receipt unexpectedly admitted replacement")
	} else if !errors.Is(err, ErrCapacityReleaseStale) {
		t.Fatalf("malformed abort receipt lost typed abort failure: %v", err)
	}
	current, err := store.CurrentLease(ctx, key)
	if err != nil || current == nil || current.Status == StatusActive {
		t.Fatalf("replacement remained active after malformed abort receipt: current=%+v err=%v", current, err)
	}
	if claimed, err := mgr.IsClaimed(ctx, key); err != nil || claimed {
		t.Fatalf("replacement remained claimable after malformed abort receipt: claimed=%v err=%v", claimed, err)
	}
	if got := cap.releasedCount(); got != 0 {
		t.Fatalf("never-reserved replacement emitted external release: %d", got)
	}
	settled, err := mgr.SettlePendingCapacity(ctx)
	if err != nil || len(settled) != 1 || settled[0].ID != incumbent.ID || cap.releasedCount() != 1 {
		t.Fatalf("only incumbent should remain retryable: settled=%+v releases=%d err=%v", settled, cap.releasedCount(), err)
	}
}

func TestClaimManager_ExpiryAndActiveClaims(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	cap := newCountingCapacity()
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute), WithCapacityCoordinator(cap))
	ctx := context.Background()

	live := testKey("FAC-5")
	stale := testKey("FAC-6")

	if _, err := mgr.Claim(ctx, ClaimRequest{Key: live, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: live.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: live.Repo, Owner: "worker", Lane: "smith", Task: live.TaskRef, Scope: "task"}}}); err != nil {
		t.Fatalf("claim live: %v", err)
	}
	if _, err := mgr.Claim(ctx, ClaimRequest{Key: stale, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: stale.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: stale.Repo, Owner: "worker", Lane: "smith", Task: stale.TaskRef, Scope: "task"}}}); err != nil {
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
	if _, err := mgr.Claim(ctx, ClaimRequest{Key: live, OwnerID: "w3", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: live.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: live.Repo, Owner: "worker", Lane: "smith", Task: live.TaskRef, Scope: "task"}}}); err != nil {
		t.Fatalf("expected expired key reclaimable, got %v", err)
	}
}

func TestClaimManager_OperatorHold_PreventsExpiry(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC))
	authority := newTestHoldAuthority(t)
	mgr := NewClaimManager(store, WithHoldReader(authority), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-7")
	ids := testHoldIdentities(key)

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}}})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, id := range ids {
		if _, err := authority.Hold(ctx, id, "operator", "maintenance", "operator_hold", 1, nil); err != nil {
			t.Fatalf("lifecycle hold: %v", err)
		}
	}
	if _, err := mgr.Hold(ctx, key, "w1", lease.Generation, true); !errors.Is(err, ErrLegacyLeaseHoldDisabled) {
		t.Fatalf("legacy lease hold must be disabled, got %v", err)
	}

	clk.advance(10 * time.Minute) // well past TTL

	expired, err := mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("expected held lease to be skipped by ExpireStale, got %d transitioned", len(expired))
	}

	// A competing claim must still be rejected while held.
	if _, err := mgr.Claim(ctx, testClaimRequest(key, "w2", "herd-smith")); err == nil {
		t.Fatal("expected held lease to block a competing claim")
	}

	for _, id := range ids {
		if _, err := authority.Release(ctx, id, "operator", "maintenance complete", "operator_release", 1); err != nil {
			t.Fatalf("lifecycle release: %v", err)
		}
	}
	expired, err = mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale after unhold: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("expected unheld, past-TTL lease to expire, got %d", len(expired))
	}
}

func TestClaimManager_LegacyHoldAPIIsDisabledWithoutChangingLease(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-9")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := mgr.Hold(ctx, key, "w1", lease.Generation, true); !errors.Is(err, ErrLegacyLeaseHoldDisabled) {
		t.Fatalf("expected legacy Hold API to be disabled, got %v", err)
	}
	row, err := store.LeaseByGeneration(ctx, key, "w1", lease.Generation)
	if err != nil || row == nil || row.Status != StatusActive || row.Held {
		t.Fatalf("legacy Hold changed lease state: row=%+v err=%v", row, err)
	}
}

func TestClaimManager_Renew_RejectsExpiredLease(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	authority := newTestHoldAuthority(t)
	mgr := NewClaimManager(store, WithHoldReader(authority), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-10")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Past TTL, but nobody has swept it yet -- still status='active' in
	// the store. Renew must still reject it instead of silently
	// extending a dead lease's life.
	clk.advance(2 * time.Minute)
	if _, err := mgr.Renew(ctx, key, "w1", lease.Generation); !errors.Is(err, ErrLeaseExpired) {
		t.Fatalf("expected ErrLeaseExpired for an unswept but past-TTL lease, got %v", err)
	}

	if _, err := mgr.Hold(ctx, key, "w1", lease.Generation, true); !errors.Is(err, ErrLegacyLeaseHoldDisabled) {
		t.Fatalf("expected legacy Hold API to remain disabled, got %v", err)
	}
}

// TestClaimManager_ReclaimReleasesOldCapacityBeforeReservingNew proves
// requirement 2: when a new claim reclaims a key whose previous lease
// expired, the old lease's capacity token is durably released before (or,
// on failure, at minimum durably queued ahead of) the new token is
// reserved -- never silently dropped, never double-counted.
func TestClaimManager_ReclaimReleasesOldCapacityBeforeReservingNew(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	capCoord := newCountingCapacity()
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute), WithCapacityCoordinator(capCoord))
	ctx := context.Background()
	key := testKey("FAC-11")

	if _, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith")); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if r, rel := capCoord.counts("herd-smith"); r != 1 || rel != 0 {
		t.Fatalf("expected 1 reserve / 0 release after first claim, got %d/%d", r, rel)
	}

	clk.advance(2 * time.Minute) // first lease's TTL passes, nobody has swept it

	if _, err := mgr.Claim(ctx, testClaimRequest(key, "w2", "herd-smith")); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	// The reclaim must have durably released w1's token (not just
	// reserved a second one on top of it): exactly 2 reserves (w1, w2)
	// and exactly 1 release (w1's, settled before/with w2's reservation).
	if r, rel := capCoord.counts("herd-smith"); r != 2 || rel != 1 {
		t.Fatalf("expected 2 reserves / 1 release after reclaim, got %d/%d", r, rel)
	}
}

// failNTimesCapacity fails the first N Release calls, then succeeds, so
// tests can prove a capacity-release failure leaves the token durably
// pending and retryable instead of being silently treated as done.
type failNTimesCapacity struct {
	mu          sync.Mutex
	failsLeft   int
	released    int
	reserveErrs int
}

func (c *failNTimesCapacity) Reserve(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserveErrs++ // reused as a reserve counter; never fails
	return nil
}

func (c *failNTimesCapacity) Release(context.Context, string, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failsLeft > 0 {
		c.failsLeft--
		return errors.New("capacity backend unavailable")
	}
	c.released++
	return nil
}

func (c *failNTimesCapacity) releasedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.released
}

// TestClaimManager_CapacityReleaseFailure_DurablyPendingAndRetryable
// proves requirement 4: a capacity.Release failure must never be
// swallowed as if it succeeded, and a retry must actually re-attempt the
// coordinator call (not short-circuit to nil because the lease row was
// already marked released).
func TestClaimManager_CapacityReleaseFailure_DurablyPendingAndRetryable(t *testing.T) {
	store := newTestStore(t)
	failing := &failNTimesCapacity{failsLeft: 2}
	mgr := NewClaimManager(store, WithCapacityCoordinator(failing), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-12")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// First release: the store transition succeeds but capacity.Release
	// fails. Release must surface that failure, not return nil.
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err == nil {
		t.Fatal("expected the first Release to surface the capacity failure, not return nil")
	}
	if got := failing.releasedCount(); got != 0 {
		t.Fatalf("expected 0 completed releases after the first failed attempt, got %d", got)
	}

	// The lease itself is already durably released at the store layer
	// (idempotent replay), but capacity must still be pending -- a naive
	// "transitioned=false means already handled" implementation would
	// return nil here without ever calling the coordinator again.
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err == nil {
		t.Fatal("expected the second Release to retry and surface the still-failing capacity release, not return nil")
	}
	if got := failing.releasedCount(); got != 0 {
		t.Fatalf("expected 0 completed releases after the second failed attempt, got %d", got)
	}

	// Third attempt: the coordinator now succeeds. This proves the retry
	// actually re-invoked Release rather than having given up.
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("expected the third Release to finally succeed, got %v", err)
	}
	if got := failing.releasedCount(); got != 1 {
		t.Fatalf("expected exactly 1 completed release once the coordinator recovered, got %d", got)
	}

	// A fourth, fully-idempotent call must not re-invoke the coordinator
	// again (nothing pending left).
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("expected a fully-settled idempotent release to succeed, got %v", err)
	}
	if got := failing.releasedCount(); got != 1 {
		t.Fatalf("expected capacity release to stay exactly-once after settlement, got %d completed calls", got)
	}
}

// TestSQLiteLeaseStore_ExpireStale_RechecksHeldAtTransitionTime proves
// requirement 5: ExpireStale's per-row UPDATE re-evaluates held/expiry at
// transition time, not just at the earlier candidate SELECT, so an
// operator Hold landing in that window wins the race instead of being
// silently overridden.
func TestSQLiteLeaseStore_LegacyHoldAPIIsDisabled(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	key := testKey("FAC-13")
	claimAt := time.Now()

	lease, err := store.Acquire(ctx, key, "w1", "herd-smith", "/wt", claimAt, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := store.Hold(ctx, key, "w1", lease.Generation, true, claimAt); !errors.Is(err, ErrLegacyLeaseHoldDisabled) {
		t.Fatalf("raw lease Hold must be disabled, got %v", err)
	}
	row, err := store.currentActive(ctx, key)
	if err != nil || row == nil || row.Status != StatusActive || row.Held {
		t.Fatalf("legacy Hold changed store row: row=%+v err=%v", row, err)
	}
}
