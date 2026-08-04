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

// fakeProviderTask models one task's revision/status/highest-accepted
// fence token in a fake provider board, e.g. a Kaneo card.
type fakeProviderTask struct {
	revision  int
	status    string
	fenceHigh int64 // highest fence token ever accepted or advanced to
}

// fakeProviderCAS is a realistic revision-AND-fence-checked test double:
// CompareAndSwap checks the current revision against expected (optimistic
// concurrency, like a real provider's ETag/updatedAt-based CAS) AND
// fenceToken against the highest fence ever seen for that taskID
// (monotonic fencing, per ProviderCAS's contract), calling mutate only if
// both hold. It counts every call so tests can assert how many times the
// external mutation actually ran, and separately tracks whether a call
// was rejected specifically for staleness vs fencing.
type fakeProviderCAS struct {
	mu              sync.Mutex
	tasks           map[string]*fakeProviderTask
	casCalls        int
	fenceRejections int
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

func (f *fakeProviderCAS) fenceRejectionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fenceRejections
}

// CompareAndSwap implements ProviderCAS, enforcing fenceToken exactly as
// the interface's contract requires: a fenceToken lower than the highest
// ever accepted/advanced-to for taskID is rejected with
// ErrProviderFenceRejected, no mutation applied, no revision bump --
// regardless of whether expected matches the current revision. This is
// checked BEFORE the revision check and BEFORE mutate is ever called, so
// a fenced-out call has zero effect no matter what mutate would have
// done.
func (f *fakeProviderCAS) CompareAndSwap(ctx context.Context, taskID string, expected ProviderRevision, fenceToken int64, mutate func(ctx context.Context) error) (ProviderRevision, error) {
	f.mu.Lock()
	f.casCalls++
	t, ok := f.tasks[taskID]
	if !ok {
		t = &fakeProviderTask{}
		f.tasks[taskID] = t
	}
	if fenceToken < t.fenceHigh {
		f.fenceRejections++
		cur := ProviderRevision(fmt.Sprintf("%d", t.revision))
		f.mu.Unlock()
		return cur, fmt.Errorf("%w: fence token %d is behind %d for %s", ErrProviderFenceRejected, fenceToken, t.fenceHigh, taskID)
	}
	if fenceToken > t.fenceHigh {
		t.fenceHigh = fenceToken
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

// AdvanceFence implements ProviderCAS.
func (f *fakeProviderCAS) AdvanceFence(ctx context.Context, taskID string, fenceToken int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[taskID]
	if !ok {
		t = &fakeProviderTask{}
		f.tasks[taskID] = t
	}
	if fenceToken > t.fenceHigh {
		t.fenceHigh = fenceToken
	}
	return nil
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

type countingDurableOutbox struct {
	*SQLiteOutbox
	claimCalls atomic.Int32
}

func (o *countingDurableOutbox) Claim(ctx context.Context, key, owner string, staleAfter time.Duration, now time.Time) (*OutboxRecord, error) {
	o.claimCalls.Add(1)
	return o.SQLiteOutbox.Claim(ctx, key, owner, staleAfter, now)
}

func TestCompleteProviderTransitionRejectsInvalidSettlerAndTimeoutBeforeOutbox(t *testing.T) {
	for _, tc := range []struct {
		name    string
		settler string
		timeout time.Duration
	}{
		{name: "invalid settler", settler: " ", timeout: time.Minute},
		{name: "zero timeout", settler: "settler", timeout: 0},
		{name: "negative timeout", settler: "settler", timeout: -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outbox := &countingDurableOutbox{SQLiteOutbox: newTestOutbox(t)}
			mgr := NewClaimManager(newTestStore(t), WithProviderCAS(newFakeProviderCAS()), WithDurableOutbox(outbox), WithSettlerID(tc.settler), WithCapacityClaimTimeout(tc.timeout), WithHoldReader(newTestHoldAuthority(t)))
			if _, err := mgr.CompleteProviderTransition(context.Background(), testKey("FAC-invalid-transition"), "owner", 1, "FAC-invalid-transition", "1", func(context.Context) error { return nil }); err == nil {
				t.Fatal("invalid transition configuration unexpectedly succeeded")
			}
			if calls := outbox.claimCalls.Load(); calls != 0 {
				t.Fatalf("invalid transition configuration reached outbox: %d calls", calls)
			}
		})
	}
}

func TestClaimManager_ProviderTransition_HappyPath(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-30", "to-do", 1)
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-30")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}}})
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
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-31")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}}})
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
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithSettlerID("crashed"), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-32")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
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
	if _, err := provider.CompareAndSwap(ctx, "FAC-32", "1", lease.Generation, func(ctx context.Context) error {
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
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-33")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
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
	authority := newTestHoldAuthority(t)
	mgrA := NewClaimManager(storeA, WithProviderCAS(provider), WithDurableOutbox(outboxA), WithSettlerID("mgrA"), WithHoldReader(authority))
	mgrB := NewClaimManager(storeB, WithProviderCAS(provider), WithDurableOutbox(outboxB), WithSettlerID("mgrB"), WithHoldReader(authority))
	ctx := context.Background()
	key := testKey("FAC-34")

	lease, err := mgrA.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
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
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-40")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
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
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-41")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	clk.advance(2 * time.Minute) // past TTL, nobody has swept it yet
	next, err := mgr.Claim(ctx, testClaimRequest(key, "w2", "herd-smith"))
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
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-42")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
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
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)))
	key := testKey("FAC-43")

	assertProviderTransitionFenced(t, mgr, outbox, provider, key, "w1", 1, "FAC-43")
}

// TestClaimManager_CompleteProviderTransition_ReleaseBlockedDuringInFlightCAS
// reproduces the exact race the review's probe exploited and proves the
// mutual-exclusion fix closes it. The probe: gate the SECOND read-only
// check, release the exact owner/generation, then resume -- outcome
// complete_err=<nil> provider_calls=1 mutated=true. Two reads can never
// close that window, because the second read observing "still current"
// and the ProviderCAS call actually running are not the same instant;
// anything can happen between them. There is no way to make an
// independent external call (ProviderCAS) atomic with a local read, so
// the fix instead makes the two operations that must not interleave --
// Release/reclaim and an in-flight provider mutation -- mutually
// exclusive via a real lock: AcquireProviderLock verifies AND locks the
// lease in one atomic store statement, held for the duration of the
// ProviderCAS call.
//
// completeProviderTransitionTestHook fires with that lock already held,
// immediately before ProviderCAS. This test uses it to attempt a REAL
// Release for the exact owner/generation CompleteProviderTransition is
// mid-flight for -- reproducing the review's "release after the final
// check, before the provider call" scenario precisely. Under the fix,
// that Release must fail (the lock blocks it, returning
// ErrProviderTransitionInProgress) rather than silently succeeding, and
// CompleteProviderTransition -- which legitimately still owns the lease
// throughout, because the interleaved release never actually took
// effect -- completes normally with exactly one correct (non-stale)
// ProviderCAS call. Zero release/reclaim-and-provider-call interleaving
// is possible; there is no outcome where the provider is mutated on
// behalf of a lease that is not, at that exact instant, still current.
func TestClaimManager_CompleteProviderTransition_ReleaseBlockedDuringInFlightCAS(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-44", "to-do", 1)
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-44")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation"); err != nil {
		t.Fatalf("begin: %v", err)
	}

	var releaseErr error
	completeProviderTransitionTestHook = func() {
		releaseErr = mgr.Release(ctx, key, "w1", lease.Generation)
	}
	t.Cleanup(func() { completeProviderTransitionTestHook = nil })

	rec, err := mgr.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-44", "1", func(ctx context.Context) error {
		provider.setStatus("FAC-44", "claimed")
		return nil
	})

	if !errors.Is(releaseErr, ErrProviderTransitionInProgress) {
		t.Fatalf("expected the interleaved Release to be blocked by the in-flight lock with ErrProviderTransitionInProgress, got %v", releaseErr)
	}
	if err != nil {
		t.Fatalf("expected CompleteProviderTransition to succeed (it legitimately still owns the lease throughout), got %v", err)
	}
	if rec.Status != OutboxApplied {
		t.Fatalf("expected applied, got %s", rec.Status)
	}
	if calls := provider.callCount(); calls != 1 {
		t.Fatalf("expected exactly 1 correct ProviderCAS call, got %d", calls)
	}
	if status := provider.statusOf("FAC-44"); status != "claimed" {
		t.Fatalf("expected the legitimate mutation to have applied, got %q", status)
	}

	// The interleaved release never actually took effect: the lease is
	// still active, same generation, once the lock is released.
	claims, err := mgr.ActiveClaims(ctx)
	if err != nil {
		t.Fatalf("active claims: %v", err)
	}
	if len(claims) != 1 || claims[0].Generation != lease.Generation || claims[0].OwnerID != "w1" {
		t.Fatalf("expected the original lease still active and untouched, got %+v", claims)
	}

	// Now that the lock is released (CompleteProviderTransition returned),
	// the owner can release normally.
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("expected release to succeed once the lock clears, got %v", err)
	}
}

// TestClaimManager_ExpireStale_BlockedDuringInFlightCAS reproduces the
// follow-up race the review found: the provider-transition lock only
// gated Release and Acquire's reclaim-eviction step, not the standalone
// ExpireStale sweep -- so a lease genuinely past its TTL, but mid an
// in-flight ProviderCAS call, could be expired by ExpireStale and then
// reclaimed at a new generation by a concurrent Acquire while the old
// CompleteProviderTransition was still running. The independent
// deterministic probe: acquire the provider lock, advance time past TTL
// while CAS is paused, invoke ExpireStale, claim the replacement
// generation 2, then resume CAS -- outcome expired=1
// replacement_generation=2 complete_err=<nil> provider_calls=1
// mutated=true.
//
// completeProviderTransitionTestHook fires with the lock already held,
// immediately before ProviderCAS -- exactly that window. This test
// reproduces the probe's steps precisely: advances the fake clock past
// TTL, calls ExpireStale (must transition nothing), then attempts a
// reclaim via Claim with a different owner (must be rejected, not
// silently handed generation 2). Only once the hook returns and
// CompleteProviderTransition's deferred ReleaseProviderLock actually
// runs does the lease become expirable/reclaimable again -- proving the
// lock, not luck, is what kept it pinned during the call, and that this
// is deliberate crash-recovery-bounded exclusion (see the staleness
// assertion below), not a permanent hole.
func TestClaimManager_ExpireStale_BlockedDuringInFlightCAS(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-45", "to-do", 1)
	clk := newClock(time.Now())
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-45")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}}})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation"); err != nil {
		t.Fatalf("begin: %v", err)
	}

	var expired []*Lease
	var expireErr error
	var reclaimErr error
	completeProviderTransitionTestHook = func() {
		clk.advance(2 * time.Minute) // now well past the 1-minute TTL

		expired, expireErr = mgr.ExpireStale(ctx)

		_, reclaimErr = mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}}})
	}
	t.Cleanup(func() { completeProviderTransitionTestHook = nil })

	rec, err := mgr.CompleteProviderTransition(ctx, key, "w1", lease.Generation, "FAC-45", "1", func(ctx context.Context) error {
		provider.setStatus("FAC-45", "claimed")
		return nil
	})

	if expireErr != nil {
		t.Fatalf("expire stale (called from inside the hook): %v", expireErr)
	}
	if len(expired) != 0 {
		t.Fatalf("expected ExpireStale to transition nothing while the provider lock is held, got %d: %+v", len(expired), expired)
	}
	var conflict *ClaimConflictError
	if !errors.As(reclaimErr, &conflict) {
		t.Fatalf("expected the reclaim attempt to be rejected with a ClaimConflictError while locked, got %v", reclaimErr)
	}

	if err != nil {
		t.Fatalf("expected CompleteProviderTransition to succeed (it legitimately still owns generation %d throughout), got %v", lease.Generation, err)
	}
	if rec.Status != OutboxApplied {
		t.Fatalf("expected applied, got %s", rec.Status)
	}
	if calls := provider.callCount(); calls != 1 {
		t.Fatalf("expected exactly 1 correct ProviderCAS call, got %d", calls)
	}
	if status := provider.statusOf("FAC-45"); status != "claimed" {
		t.Fatalf("expected the legitimate mutation to have applied, got %q", status)
	}

	// The original generation is still what's on record -- no reclaim
	// snuck through while the lock was held. Checked directly against the
	// store rather than via ClaimManager.ActiveClaims, since the clock
	// has been advanced past TTL and ActiveClaims deliberately excludes
	// unswept-but-expired leases regardless of the lock; status='active'
	// here is exactly the fact this test cares about.
	current, err := store.currentActive(ctx, key)
	if err != nil {
		t.Fatalf("current active: %v", err)
	}
	if current == nil || current.Generation != lease.Generation || current.OwnerID != "w1" {
		t.Fatalf("expected the original generation %d still active on record, got %+v", lease.Generation, current)
	}

	// Now that the lock has cleared (CompleteProviderTransition returned),
	// the genuinely-past-TTL lease is expirable and reclaimable again --
	// this is bounded exclusion for the live call's duration, not a
	// permanent block.
	stillExpired, err := mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale after lock release: %v", err)
	}
	if len(stillExpired) != 1 || stillExpired[0].Generation != lease.Generation {
		t.Fatalf("expected the lease to finally expire once the lock cleared, got %+v", stillExpired)
	}
}

// TestSQLiteLeaseStore_StoreLayer_NeverPreemptsProviderLockByTimeAlone
// proves the store layer's half of the fix for the review's
// "provider/local partial failure" finding: Release/Acquire/ExpireStale
// no longer have any staleness carve-out of their own at all -- a
// provider lock unconditionally blocks them, live or stale, full stop.
// (Whether a stale lock is safe to clear is a decision only
// ClaimManager can make, since only it knows whether a ProviderCAS is
// configured and can durably confirm a fence advance first -- see
// TestClaimManager_ExpireStale_RequiresFenceAdvanceBeforePreemptingStaleLock
// for that half.)
func TestSQLiteLeaseStore_StoreLayer_NeverPreemptsProviderLockByTimeAlone(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	key := testKey("FAC-46")
	claimAt := time.Now()

	lease, err := store.AcquireWithIdentity(ctx, key, "w1", "herd-smith", "/wt", key.Repo, "worker", "smith", claimAt, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := store.AcquireProviderLock(ctx, key, "w1", lease.Generation, "crashed-settler", time.Minute, claimAt); err != nil {
		t.Fatalf("acquire provider lock: %v", err)
	}

	// Arbitrarily far past both the TTL and providerLockStaleAfter: the
	// store must still refuse to mutate it. Lifecycle recovery owns the
	// only valid claim/fence/finalize path.
	farFuture := claimAt.Add(24 * time.Hour)

	if _, err := store.ExpireStale(ctx, farFuture); err == nil {
		t.Fatal("expected raw store expiry to be disabled")
	}

	if _, err := store.Acquire(ctx, key, "w2", "herd-smith", "/wt", farFuture, time.Minute); err == nil {
		t.Fatal("expected a reclaim attempt to still be blocked by the provider lock no matter how much time passed")
	}

	if err := store.ForceReleaseProviderLock(ctx, key, lease.Generation); err == nil {
		t.Fatal("expected raw provider-lock force release to be disabled")
	}
}

// TestClaimManager_ExpireStale_RequiresFenceAdvanceBeforePreemptingStaleLock
// is the ClaimManager half: proves the review's "do not reintroduce a
// five-minute live-call hole" requirement is satisfied deliberately, not
// accidentally, AND that a crashed settler's lock still self-heals once
// the provider is reachable again -- exactly like a stale
// capacity-release or outbox claim does elsewhere in this package, just
// gated on a durable fence advance succeeding first.
func TestClaimManager_ExpireStale_RequiresFenceAdvanceBeforePreemptingStaleLock(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-46", "to-do", 1)
	failing := &advanceFenceFailer{provider: provider, failNext: true}
	ctx := context.Background()
	key := testKey("FAC-46")
	claimAt := time.Now()

	lease, err := store.AcquireWithIdentity(ctx, key, "w1", "herd-smith", "/wt", key.Repo, "worker", "smith", claimAt, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := store.AcquireProviderLock(ctx, key, "w1", lease.Generation, "crashed-settler", time.Minute, claimAt); err != nil {
		t.Fatalf("acquire provider lock: %v", err)
	}

	farFuture := claimAt.Add(24 * time.Hour)
	unblockedNow := func() time.Time { return farFuture }
	mgr := NewClaimManager(store, WithProviderCAS(failing), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)), WithClock(unblockedNow))

	// First sweep: AdvanceFence fails (provider "unreachable"). The lock
	// must be left in place -- not cleared, not the lease expired.
	if _, err := mgr.ExpireStale(ctx); err == nil {
		t.Fatal("expected ExpireStale to surface the fence-advance failure, not swallow it")
	}
	stillLocked, err := store.ObserveStaleProviderLock(ctx, key, farFuture)
	if err != nil {
		t.Fatalf("peek stale provider lock: %v", err)
	}
	if stillLocked == nil || !stillLocked.Recovery || stillLocked.LeaseID != lease.ID || stillLocked.Generation != lease.Generation || stillLocked.RecoveryOwner != recoveryOwnerFor(lease.ID, lease.Generation) {
		t.Fatalf("expected exact durable recovery claim after failed fence advance, got %+v", stillLocked)
	}
	if _, changed, err := store.ExpireLeaseCAS(ctx, lease.ID, lease.Generation, farFuture); err != nil || changed {
		t.Fatalf("recovery claim must block expiry before fence advancement: changed=%v err=%v", changed, err)
	}

	// Recovery: the provider becomes reachable again. A later sweep
	// retries the SAME durable fence-advance record and succeeds.
	failing.failNext = false
	expired, err := mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale after recovery: %v", err)
	}
	if len(expired) != 1 || expired[0].Generation != lease.Generation {
		t.Fatalf("expected the lease to expire once fence-advance recovered, got %+v", expired)
	}
	if provider.fenceRejectionCount() != 0 {
		t.Fatalf("sanity: expected no fence rejections in this test, got %d", provider.fenceRejectionCount())
	}
}

// advanceFenceFailer wraps a fakeProviderCAS and can be told to fail the
// next AdvanceFence call, to test durable retry after an initial
// failure.
type advanceFenceFailer struct {
	provider *fakeProviderCAS
	failNext bool
}

func (f *advanceFenceFailer) CompareAndSwap(ctx context.Context, taskID string, expected ProviderRevision, fenceToken int64, mutate func(ctx context.Context) error) (ProviderRevision, error) {
	return f.provider.CompareAndSwap(ctx, taskID, expected, fenceToken, mutate)
}

func (f *advanceFenceFailer) AdvanceFence(ctx context.Context, taskID string, fenceToken int64) error {
	if f.failNext {
		return fmt.Errorf("provider unavailable")
	}
	return f.provider.AdvanceFence(ctx, taskID, fenceToken)
}

var _ ProviderCAS = (*advanceFenceFailer)(nil)

// TestClaimManager_ProviderFencing_RejectsStaleCASAfterSixMinutePause is
// the review's exact scenario, reproduced precisely: a settler holds the
// provider-transition lock and pauses mid-CAS (simulating a slow or
// GC-stalled in-flight external call -- NOT a context we can cancel,
// since from another process's perspective it may as well be a crash).
// The fake clock advances six minutes -- past the lock's five-minute
// staleness window -- expiry sweeps the now-genuinely-abandoned lease,
// and a new owner reclaims it (generation 2). ONLY THEN does the
// "paused" CompareAndSwap call finally resume and reach the provider,
// carrying the OLD generation (1) as its fence token.
//
// A local lock timeout cannot prove that external call has stopped --
// that's exactly the review's point, and this test does not pretend
// otherwise: the reclaim happens, and the stale call genuinely reaches
// CompareAndSwap. Safety instead comes from the provider itself: because
// ClaimManager.Claim called AdvanceFence(taskID, 2) as part of the
// reclaim, the provider's highest-accepted fence for this taskID is now
// 2, and the resumed call's fence token of 1 is rejected --
// unconditionally, before revision is even checked, before mutate is
// ever invoked -- with zero effect on provider state. Liveness is
// preserved throughout: the lease was NOT blocked forever (reclaimed
// after the bounded 5-minute window, not indefinitely).
func TestClaimManager_ProviderFencing_RejectsStaleCASAfterSixMinutePause(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-47", "to-do", 1)
	clk := newClock(time.Now())
	mgr := NewClaimManager(store, WithProviderCAS(provider), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-47")

	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}}})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	rec, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	claimAt := clk.now()
	claimedOutbox, err := outbox.Claim(ctx, rec.IdempotencyKey, "paused-settler", providerLockStaleAfter, claimAt)
	if err != nil {
		t.Fatalf("manual outbox claim: %v", err)
	}
	if claimedOutbox == nil {
		t.Fatal("expected the manual outbox claim to succeed")
	}
	if _, err := store.AcquireProviderLock(ctx, key, "w1", lease.Generation, "paused-settler", providerLockStaleAfter, claimAt); err != nil {
		t.Fatalf("acquire provider lock: %v", err)
	}
	// The provider-transition lock is now held, exactly as if
	// CompleteProviderTransition were paused immediately before its
	// ProviderCAS call.

	// Six minutes pass: past both the 1-minute TTL and the 5-minute
	// provider-lock staleness window.
	clk.advance(providerLockStaleAfter + time.Minute)
	expired, err := mgr.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("expire stale: %v", err)
	}
	if len(expired) != 1 || expired[0].Generation != lease.Generation {
		t.Fatalf("expected the lease to expire once the lock itself went stale (liveness preserved), got %+v", expired)
	}

	replacement, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w2", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}}})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if replacement.Generation != lease.Generation+1 {
		t.Fatalf("expected the replacement to be generation %d, got %d", lease.Generation+1, replacement.Generation)
	}

	// The paused CAS finally "resumes": it reaches the provider carrying
	// the OLD generation as its fence token, with the revision it
	// originally read before pausing.
	_, casErr := provider.CompareAndSwap(ctx, "FAC-47", "1", lease.Generation, func(ctx context.Context) error {
		provider.setStatus("FAC-47", "mutated-by-stale-generation-1-call")
		return nil
	})

	if !errors.Is(casErr, ErrProviderFenceRejected) {
		t.Fatalf("expected the resumed stale-generation call to be rejected with ErrProviderFenceRejected, got %v", casErr)
	}
	if status := provider.statusOf("FAC-47"); status != "to-do" {
		t.Fatalf("expected zero stale provider effect (status unchanged from seed), got %q", status)
	}
	if got := provider.fenceRejectionCount(); got != 1 {
		t.Fatalf("expected exactly 1 fence rejection, got %d", got)
	}
}

// TestClaimManager_Claim_RejectsReclaimWhenFenceAdvanceFails_ThenRecoversSafely
// reproduces the review's exact follow-up finding: Claim previously
// called AdvanceFence only AFTER local generation 2 was already durably
// acquired, then ignored its error -- so new local ownership was exposed
// to the rest of the system with the provider never told about it.
// Independent deterministic probe: pause generation 1's CAS, expire/
// reclaim generation 2 while AdvanceFence returns "provider unavailable",
// resume generation 1's CAS -- outcome replacement_generation=2
// complete_err=<nil> advance_calls=1 provider_calls=1 mutated=true.
//
// This test drives Claim itself (not a manual store dance) through both
// halves the review demanded: (1) a failed AdvanceFence must make Claim
// itself fail -- no local reclaim exposed without it -- and (2) a later
// retry, once the provider recovers, must complete the reclaim safely.
func TestClaimManager_Claim_RejectsReclaimWhenFenceAdvanceFails_ThenRecoversSafely(t *testing.T) {
	store := newTestStore(t)
	outbox := newTestOutbox(t)
	provider := newFakeProviderCAS()
	provider.seed("FAC-48", "to-do", 1)
	failing := &advanceFenceFailer{provider: provider, failNext: true}
	clk := newClock(time.Now())
	mgr := NewClaimManager(store, WithProviderCAS(failing), WithDurableOutbox(outbox), WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(time.Minute))
	ctx := context.Background()
	key := testKey("FAC-48")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	rec, err := mgr.BeginProviderTransition(ctx, key, "w1", lease.Generation, "provider_mutation")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	// Pause generation 1's CAS immediately before it would call the
	// provider: manually claim the outbox record and the provider lock,
	// exactly as CompleteProviderTransition would have at that point.
	pauseAt := clk.now()
	if _, err := outbox.Claim(ctx, rec.IdempotencyKey, "paused-settler", providerLockStaleAfter, pauseAt); err != nil {
		t.Fatalf("manual outbox claim: %v", err)
	}
	if _, err := store.AcquireProviderLock(ctx, key, "w1", lease.Generation, "paused-settler", providerLockStaleAfter, pauseAt); err != nil {
		t.Fatalf("acquire provider lock: %v", err)
	}

	// Six minutes pass: the lock is now stale.
	clk.advance(providerLockStaleAfter + time.Minute)

	// Attempt to reclaim while the provider is "unavailable" for fence
	// advancement. This MUST fail -- no local generation 2, no exposure.
	if _, err := mgr.Claim(ctx, testClaimRequest(key, "w2", "herd-smith")); err == nil {
		t.Fatal("expected Claim to fail when the provider fence advance fails, not silently reclaim")
	}

	// Nothing changed locally: still generation 1, still active, still
	// locked.
	current, err := store.currentActive(ctx, key)
	if err != nil {
		t.Fatalf("current active: %v", err)
	}
	if current == nil || current.Generation != lease.Generation || current.OwnerID != "w1" {
		t.Fatalf("expected generation %d still active and untouched after the failed reclaim attempt, got %+v", lease.Generation, current)
	}

	// The "paused" CAS finally resumes. Since no reclaim ever actually
	// succeeded, generation 1 is still the legitimate, current owner --
	// this completion is NOT stale, and must succeed normally.
	_, casErr := provider.CompareAndSwap(ctx, "FAC-48", "1", lease.Generation, func(ctx context.Context) error {
		provider.setStatus("FAC-48", "claimed-by-legitimate-generation-1")
		return nil
	})
	if casErr != nil {
		t.Fatalf("expected generation 1's own (non-stale) completion to succeed, got %v", casErr)
	}
	if got := provider.fenceRejectionCount(); got != 0 {
		t.Fatalf("expected zero fence rejections (nothing was ever wrongly superseded), got %d", got)
	}

	// Recovery: the provider becomes reachable again. A retried Claim now
	// succeeds, safely, via the same durable fence-advance record.
	failing.failNext = false
	replacement, err := mgr.Claim(ctx, testClaimRequest(key, "w2", "herd-smith"))
	if err != nil {
		t.Fatalf("reclaim after recovery: %v", err)
	}
	if replacement.Generation != lease.Generation+1 {
		t.Fatalf("expected generation %d after recovery, got %d", lease.Generation+1, replacement.Generation)
	}

	// And NOW a stale call carrying the old (generation 1) fence token is
	// correctly rejected -- proving recovery didn't just complete the
	// reclaim, it completed it SAFELY, with the fence genuinely advanced.
	_, staleErr := provider.CompareAndSwap(ctx, "FAC-48", provider.revisionOf("FAC-48"), lease.Generation, func(ctx context.Context) error {
		t.Fatal("mutate must not run for a fence-rejected call")
		return nil
	})
	if !errors.Is(staleErr, ErrProviderFenceRejected) {
		t.Fatalf("expected a post-recovery stale-generation call to be fence-rejected, got %v", staleErr)
	}
}
