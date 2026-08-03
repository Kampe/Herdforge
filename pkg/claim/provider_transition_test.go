package claim

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeProviderTask models one task's revision/status in a fake provider
// board, e.g. a Kaneo card.
type fakeProviderTask struct {
	revision int
	status   string
}

// fakeProviderCAS is a realistic revision-fenced test double: CompareAndSwap
// checks the current revision against expected, calls mutate only on a
// match, and bumps the revision -- optimistic concurrency exactly like a
// real provider's ETag/updatedAt-based CAS. It counts every call so tests
// can assert how many times the external mutation actually ran.
type fakeProviderCAS struct {
	mu       sync.Mutex
	tasks    map[string]*fakeProviderTask
	casCalls int
}

func newFakeProviderCAS() *fakeProviderCAS {
	return &fakeProviderCAS{tasks: map[string]*fakeProviderTask{}}
}

func (f *fakeProviderCAS) seed(taskID, status string, revision int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[taskID] = &fakeProviderTask{status: status, revision: revision}
}

func (f *fakeProviderCAS) revisionOf(taskID string) ProviderRevision {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tasks[taskID]
	if t == nil {
		return ""
	}
	return ProviderRevision(fmt.Sprintf("%d", t.revision))
}

func (f *fakeProviderCAS) statusOf(taskID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.tasks[taskID]; t != nil {
		return t.status
	}
	return ""
}

// setStatus is what test-supplied mutate closures call; it takes its own
// lock, so it must never be called while CompareAndSwap already holds
// f.mu (CompareAndSwap releases the lock before invoking mutate).
func (f *fakeProviderCAS) setStatus(taskID, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[taskID]; ok {
		t.status = status
	}
}

func (f *fakeProviderCAS) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.casCalls
}

func (f *fakeProviderCAS) CompareAndSwap(ctx context.Context, taskID string, expected ProviderRevision, mutate func(ctx context.Context) error) (ProviderRevision, error) {
	f.mu.Lock()
	f.casCalls++
	t, ok := f.tasks[taskID]
	if !ok {
		t = &fakeProviderTask{}
		f.tasks[taskID] = t
	}
	cur := ProviderRevision(fmt.Sprintf("%d", t.revision))
	if cur != expected {
		f.mu.Unlock()
		return cur, fmt.Errorf("%w: expected %s, current %s", ErrProviderRevisionStale, expected, cur)
	}
	f.mu.Unlock() // release before calling the caller's mutate

	if err := mutate(ctx); err != nil {
		return cur, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if ProviderRevision(fmt.Sprintf("%d", t.revision)) != expected {
		return cur, fmt.Errorf("%w: concurrent change during mutate", ErrProviderRevisionStale)
	}
	t.revision++
	return ProviderRevision(fmt.Sprintf("%d", t.revision)), nil
}

var _ ProviderCAS = (*fakeProviderCAS)(nil)

func newTestOutbox(t *testing.T) *SQLiteOutbox {
	t.Helper()
	path := filepath.Join(t.TempDir(), "outbox.db")
	o, err := NewSQLiteOutbox(path)
	if err != nil {
		t.Fatalf("open outbox: %v", err)
	}
	t.Cleanup(func() { o.Close() })
	return o
}

func TestClaimManager_ProviderTransition_HappyPath(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-30", "to-do", 1)
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox))
	ctx := context.Background()
	key := testKey("FAC-30")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	rec, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if rec.Status != OutboxPending {
		t.Fatalf("expected pending, got %s", rec.Status)
	}

	rec, err = mgr.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-30", "1", func(ctx context.Context) error {
		provider.setStatus("FAC-30", "in-progress")
		return nil
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if rec.Status != OutboxApplied {
		t.Fatalf("expected applied, got %s", rec.Status)
	}
	if provider.statusOf("FAC-30") != "in-progress" {
		t.Fatalf("expected provider status mutated, got %s", provider.statusOf("FAC-30"))
	}
	if provider.callCount() != 1 {
		t.Fatalf("expected exactly 1 CAS call, got %d", provider.callCount())
	}

	// Idempotent replay of an already-applied intent must not re-invoke
	// CompareAndSwap.
	rec, err = mgr.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-30", "1", func(ctx context.Context) error {
		t.Fatal("mutate should not run again for an already-applied intent")
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if rec.Status != OutboxApplied {
		t.Fatalf("expected still applied, got %s", rec.Status)
	}
	if provider.callCount() != 1 {
		t.Fatalf("expected CAS call count to stay at 1 after idempotent replay, got %d", provider.callCount())
	}
}

func TestClaimManager_ProviderTransition_StaleRevision_LeaseUntouched(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-31", "to-do", 5) // current revision is 5
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox))
	ctx := context.Background()
	key := testKey("FAC-31")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	rec, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Attempt with a stale expected revision ("1" instead of the real "5").
	rec, err = mgr.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-31", "1", func(ctx context.Context) error {
		t.Fatal("mutate must not run on a revision mismatch")
		return nil
	})
	if err != nil {
		t.Fatalf("complete (stale revision should not error the call itself): %v", err)
	}
	if rec.Status != OutboxFailed {
		t.Fatalf("expected failed status after stale revision, got %s", rec.Status)
	}

	// The lease itself must be completely untouched: still active, same
	// generation, exactly one claim.
	claims, err := mgr.ActiveClaims(ctx)
	if err != nil {
		t.Fatalf("active claims: %v", err)
	}
	if len(claims) != 1 || claims[0].Generation != lease.Generation || claims[0].OwnerID != "w1" {
		t.Fatalf("expected the original lease untouched, got %+v", claims)
	}

	// Corrected retry with the real current revision succeeds.
	rec, err = mgr.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-31", provider.revisionOf("FAC-31"), func(ctx context.Context) error {
		provider.setStatus("FAC-31", "claimed")
		return nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if rec.Status != OutboxApplied {
		t.Fatalf("expected applied after retry, got %s", rec.Status)
	}

	// No double-claim/release occurred across the retry.
	claims, err = mgr.ActiveClaims(ctx)
	if err != nil {
		t.Fatalf("active claims after retry: %v", err)
	}
	if len(claims) != 1 || claims[0].Generation != lease.Generation {
		t.Fatalf("expected still exactly the original lease/generation after retry, got %+v", claims)
	}
}

// TestClaimManager_ProviderSuccessLocalFailure_ReconciliationClosesWithoutReplay
// simulates the provider-success/local-failure direction: a settler
// claims the outbox intent, the provider mutation genuinely succeeds, but
// the process "crashes" before it can call MarkApplied (or even
// MarkFailed) -- the record is stuck InProgress. Reconciliation, using an
// independent verifier that checks the real provider state, must close
// the record out WITHOUT calling CompareAndSwap again and WITHOUT
// touching the lease.
func TestClaimManager_ProviderSuccessLocalFailure_ReconciliationClosesWithoutReplay(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-32", "to-do", 1)
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithSettlerID("crashed"))
	ctx := context.Background()
	key := testKey("FAC-32")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	rec, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Manually drive the claim + a real, successful provider mutation,
	// but deliberately stop before MarkApplied -- simulating a crash
	// right there.
	claimed, err := outbox.Claim(ctx, rec.IdempotencyKey, "crashed", 0, mgr.now())
	if err != nil {
		t.Fatalf("manual claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("expected the manual claim to succeed")
	}
	if _, err := provider.CompareAndSwap(ctx, "FAC-32", "1", func(ctx context.Context) error {
		provider.setStatus("FAC-32", "in-progress")
		return nil
	}); err != nil {
		t.Fatalf("provider mutation: %v", err)
	}
	// No MarkApplied call here: this is the crash.

	if provider.callCount() != 1 {
		t.Fatalf("expected 1 CAS call before reconciliation, got %d", provider.callCount())
	}

	closed, stillPending, err := mgr.ReconcileProviderTransitions(ctx, func(ctx context.Context, r *OutboxRecord) (bool, error) {
		return provider.statusOf("FAC-32") == "in-progress", nil
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if closed != 1 || stillPending != 0 {
		t.Fatalf("expected 1 closed / 0 still pending, got closed=%d stillPending=%d", closed, stillPending)
	}

	// CompareAndSwap must NOT have been called again by reconciliation.
	if provider.callCount() != 1 {
		t.Fatalf("expected reconciliation to close the record WITHOUT re-invoking CompareAndSwap, but call count is now %d", provider.callCount())
	}

	final, err := outbox.Get(ctx, rec.IdempotencyKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.Status != OutboxApplied {
		t.Fatalf("expected the record closed as applied, got %s", final.Status)
	}

	// No double-claim/release of the lease occurred.
	claims, err := mgr.ActiveClaims(ctx)
	if err != nil {
		t.Fatalf("active claims: %v", err)
	}
	if len(claims) != 1 || claims[0].Generation != lease.Generation {
		t.Fatalf("expected the original lease/generation untouched, got %+v", claims)
	}
}

// TestClaimManager_LocalSuccessProviderFailure_RetrySucceedsNoDoubleClaim
// covers the other direction named by the review: the provider mutation
// itself fails (a transient error, not a revision mismatch), leaving the
// outbox record Failed while the lease claim ("local success") stands.
// A retry with the corrected call succeeds, with no double-claim/release
// across the failure+retry.
func TestClaimManager_LocalSuccessProviderFailure_RetrySucceedsNoDoubleClaim(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-33", "to-do", 1)
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox))
	ctx := context.Background()
	key := testKey("FAC-33")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	rec, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	transientErr := fmt.Errorf("network timeout talking to provider")
	rec, err = mgr.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-33", "1", func(ctx context.Context) error {
		return transientErr
	})
	if err != nil {
		t.Fatalf("complete (transient failure should not error the call itself): %v", err)
	}
	if rec.Status != OutboxFailed {
		t.Fatalf("expected failed, got %s", rec.Status)
	}

	// Reconciliation with a verifier that (correctly) reports "not
	// applied" must leave it pending for a real retry, not fabricate one.
	closed, stillPending, err := mgr.ReconcileProviderTransitions(ctx, func(ctx context.Context, r *OutboxRecord) (bool, error) {
		return provider.statusOf("FAC-33") == "claimed", nil // it never got set
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if closed != 0 || stillPending != 1 {
		t.Fatalf("expected 0 closed / 1 still pending, got closed=%d stillPending=%d", closed, stillPending)
	}

	// A real retry (as a driver with the real mutate closure would do).
	rec, err = mgr.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-33", provider.revisionOf("FAC-33"), func(ctx context.Context) error {
		provider.setStatus("FAC-33", "claimed")
		return nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if rec.Status != OutboxApplied {
		t.Fatalf("expected applied after retry, got %s", rec.Status)
	}

	claims, err := mgr.ActiveClaims(ctx)
	if err != nil {
		t.Fatalf("active claims: %v", err)
	}
	if len(claims) != 1 || claims[0].Generation != lease.Generation {
		t.Fatalf("expected no double-claim/release across failure+retry, got %+v", claims)
	}
}

// TestSQLiteOutbox_Claim_ConcurrentCallers_ExactlyOneClaimsIt isolates the
// outbox's atomic-claim primitive directly, the same way
// TestSQLiteLeaseStore_ClaimCapacityRelease_ConcurrentCallers_ExactlyOneClaimsIt
// does for capacity release: two independent SQLiteOutbox handles racing
// Claim for the same idempotency key at the same instant. This is the
// deterministic, timing-independent proof; the ClaimManager-level
// TestClaimManager_ConcurrentProviderTransition_OnlyOneCASCall below is
// realistic wiring evidence on top of it, not a substitute for it -- a
// full claim+external-call+mark round trip has enough latency that a
// broken guard doesn't reliably lose the race within one test run, while
// a bare concurrent Claim call reliably does.
func TestSQLiteOutbox_Claim_ConcurrentCallers_ExactlyOneClaimsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	outboxA, err := NewSQLiteOutbox(path)
	if err != nil {
		t.Fatalf("open outboxA: %v", err)
	}
	defer outboxA.Close()
	outboxB, err := NewSQLiteOutbox(path)
	if err != nil {
		t.Fatalf("open outboxB: %v", err)
	}
	defer outboxB.Close()

	ctx := context.Background()
	rec, err := outboxA.Enqueue(ctx, OutboxIntent{IdempotencyKey: "provider:FAC-35:g1", Kind: "provider_mutation"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var wg sync.WaitGroup
	var claimedA, claimedB *OutboxRecord
	var errA, errB error
	now := time.Now()
	wg.Add(2)
	go func() { defer wg.Done(); claimedA, errA = outboxA.Claim(ctx, rec.IdempotencyKey, "settlerA", 0, now) }()
	go func() { defer wg.Done(); claimedB, errB = outboxB.Claim(ctx, rec.IdempotencyKey, "settlerB", 0, now) }()
	wg.Wait()

	if errA != nil {
		t.Fatalf("outboxA claim: %v", errA)
	}
	if errB != nil {
		t.Fatalf("outboxB claim: %v", errB)
	}

	claimedCount := 0
	if claimedA != nil {
		claimedCount++
	}
	if claimedB != nil {
		claimedCount++
	}
	if claimedCount != 1 {
		t.Fatalf("expected exactly one handle to claim the record, got claimedA=%v claimedB=%v", claimedA != nil, claimedB != nil)
	}
}

// TestClaimManager_ConcurrentProviderTransition_OnlyOneCASCall mirrors the
// capacity-delivery concurrency probe for the outbox: two ClaimManagers
// (two DB handles for both the lease store and the outbox) concurrently
// calling CompleteProviderTransition for the SAME intent against a
// shared provider. Only one may actually invoke CompareAndSwap.
func TestClaimManager_ConcurrentProviderTransition_OnlyOneCASCall(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "leases.db")
	outboxPath := filepath.Join(t.TempDir(), "outbox.db")

	storeA, err := NewSQLiteLeaseStore(leasePath)
	if err != nil {
		t.Fatalf("open storeA: %v", err)
	}
	defer storeA.Close()
	storeB, err := NewSQLiteLeaseStore(leasePath)
	if err != nil {
		t.Fatalf("open storeB: %v", err)
	}
	defer storeB.Close()
	outboxA, err := NewSQLiteOutbox(outboxPath)
	if err != nil {
		t.Fatalf("open outboxA: %v", err)
	}
	defer outboxA.Close()
	outboxB, err := NewSQLiteOutbox(outboxPath)
	if err != nil {
		t.Fatalf("open outboxB: %v", err)
	}
	defer outboxB.Close()

	provider := newFakeProviderCAS()
	provider.seed("FAC-34", "to-do", 1)
	mgrA := NewClaimManager(storeA, WithProviderCAS(provider), WithDurableOutbox(outboxA), WithSettlerID("mgrA"))
	mgrB := NewClaimManager(storeB, WithProviderCAS(provider), WithDurableOutbox(outboxB), WithSettlerID("mgrB"))
	ctx := context.Background()
	key := testKey("FAC-34")

	lease, err := mgrA.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	rec, err := mgrA.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = mgrA.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-34", "1", func(ctx context.Context) error {
			provider.setStatus("FAC-34", "claimed-by-A")
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		_, _ = mgrB.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-34", "1", func(ctx context.Context) error {
			provider.setStatus("FAC-34", "claimed-by-B")
			return nil
		})
	}()
	wg.Wait()

	if calls := provider.callCount(); calls != 1 {
		t.Fatalf("expected exactly 1 CompareAndSwap call across two concurrent settlers, got %d", calls)
	}

	final, err := outboxA.Get(ctx, rec.IdempotencyKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.Status != OutboxApplied {
		t.Fatalf("expected the intent applied exactly once, got %s", final.Status)
	}
}

// assertProviderTransitionFenced is the shared assertion for every
// negative fencing test below: BeginProviderTransition must reject with
// ErrLeaseNotCurrent and create no outbox record, and
// CompleteProviderTransition (called independently, as a driver would if
// it somehow still had a stale idempotency key) must also reject with
// ErrLeaseNotCurrent and make zero ProviderCAS calls.
func assertProviderTransitionFenced(t *testing.T, mgr *ClaimManager, outbox *SQLiteOutbox, provider *fakeProviderCAS, key LeaseKey, ownerID string, generation int64, taskID string) {
	t.Helper()
	ctx := context.Background()
	idempotencyKey := providerIntentKey(key, generation)

	_, err := mgr.BeginProviderTransition(ctx, key, ownerID, generation, "provider_mutation")
	if !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("expected BeginProviderTransition to reject with ErrLeaseNotCurrent, got %v", err)
	}
	rec, getErr := outbox.Get(ctx, idempotencyKey)
	if getErr != nil {
		t.Fatalf("outbox get: %v", getErr)
	}
	if rec != nil {
		t.Fatalf("expected no outbox intent created for a rejected Begin, got %+v", rec)
	}

	callsBefore := provider.callCount()
	_, err = mgr.CompleteProviderTransition(ctx, key, ownerID, generation, taskID, provider.revisionOf(taskID), func(ctx context.Context) error {
		t.Fatal("mutate must never run for a fenced-out CompleteProviderTransition call")
		return nil
	})
	if !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("expected CompleteProviderTransition to reject with ErrLeaseNotCurrent, got %v", err)
	}
	if calls := provider.callCount(); calls != callsBefore {
		t.Fatalf("expected zero additional ProviderCAS calls from a fenced-out Complete, calls before=%d after=%d", callsBefore, calls)
	}
}

// TestClaimManager_ProviderTransition_RejectsReleasedGeneration is the
// exact scenario the independent OpenAI probe found: claim generation 1,
// release it, then attempt a provider transition against that now-dead
// generation. Must be rejected -- no outbox intent, no ProviderCAS call.
func TestClaimManager_ProviderTransition_RejectsReleasedGeneration(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-40", "to-do", 1)
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox))
	ctx := context.Background()
	key := testKey("FAC-40")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("release: %v", err)
	}

	assertProviderTransitionFenced(t, mgr, outbox, provider, key, "w1", lease.Generation, "FAC-40")
}

// TestClaimManager_ProviderTransition_RejectsPriorGenerationAfterReclaim
// covers the expiry/reclaim direction: generation 1 expires and is
// reclaimed by a different owner at generation 2. A caller still holding
// generation 1 must be fenced out even though ITS ownerID is technically
// "correct" for that old generation -- the active lease has moved on.
func TestClaimManager_ProviderTransition_RejectsPriorGenerationAfterReclaim(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-41", "to-do", 1)
	clk := newClock(time.Now())
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-41")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	clk.advance(2 * time.Minute) // past TTL, nobody has swept it yet
	next, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if next.Generation == lease.Generation {
		t.Fatalf("expected the reclaim to produce a new generation, both were %d", lease.Generation)
	}

	assertProviderTransitionFenced(t, mgr, outbox, provider, key, "w1", lease.Generation, "FAC-41")
}

// TestClaimManager_ProviderTransition_RejectsWrongOwner covers a caller
// that guesses the correct, still-active generation but is not its owner.
func TestClaimManager_ProviderTransition_RejectsWrongOwner(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-42", "to-do", 1)
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox))
	ctx := context.Background()
	key := testKey("FAC-42")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith"})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	assertProviderTransitionFenced(t, mgr, outbox, provider, key, "definitely-not-w1", lease.Generation, "FAC-42")

	// Sanity: the real owner at the real generation still works, proving
	// the rejection above was specifically about the wrong owner.
	rec, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation")
	if err != nil {
		t.Fatalf("expected the real owner to still be able to begin a transition, got %v", err)
	}
	if rec == nil {
		t.Fatal("expected a record")
	}
}

// TestClaimManager_ProviderTransition_RejectsNoLease covers a key that
// was never claimed at all.
func TestClaimManager_ProviderTransition_RejectsNoLease(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-43", "to-do", 1)
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox))
	key := testKey("FAC-43")

	assertProviderTransitionFenced(t, mgr, outbox, provider, key, "w1", 1, "FAC-43")
}
