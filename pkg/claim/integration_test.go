package claim

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// recordingOutbox captures every OutboxIntent Record was called with, so
// tests can assert ClaimManager actually wires the FAC-119 integration
// point instead of only defining the interface.
type recordingOutbox struct {
	mu       sync.Mutex
	intents  []OutboxIntent
	seenKeys map[string]bool
}

func newRecordingOutbox() *recordingOutbox {
	return &recordingOutbox{seenKeys: map[string]bool{}}
}

func (o *recordingOutbox) Record(_ context.Context, intent OutboxIntent) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.intents = append(o.intents, intent)
	o.seenKeys[intent.IdempotencyKey] = true
	return nil
}

func (o *recordingOutbox) kinds() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var kinds []string
	for _, i := range o.intents {
		kinds = append(kinds, i.Kind)
	}
	return kinds
}

func TestClaimManager_OutboxRecorder_WiredOnClaimAndRelease(t *testing.T) {
	store := newTestStore(t)
	outbox := newRecordingOutbox()
	mgr := NewClaimManager(store, WithOutboxRecorder(outbox), WithHoldReader(newTestHoldAuthority(t)))
	ctx := context.Background()
	key := testKey("FAC-14")

	lease, err := mgr.Claim(ctx, testClaimRequest(key, "w1", "herd-smith"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatalf("release: %v", err)
	}

	kinds := outbox.kinds()
	if len(kinds) != 2 || kinds[0] != "lease_claimed" || kinds[1] != "lease_released" {
		t.Fatalf("expected [lease_claimed lease_released], got %v", kinds)
	}
	if len(outbox.seenKeys) != 2 {
		t.Fatalf("expected 2 distinct idempotency keys, got %d", len(outbox.seenKeys))
	}
}

func TestClaimManager_SatisfiesReconciler(t *testing.T) {
	store := newTestStore(t)
	clk := newClock(time.Now())
	mgr := NewClaimManager(store, WithHoldReader(newTestHoldAuthority(t)), WithClock(clk.now), WithTTL(0))
	ctx := context.Background()
	key := testKey("FAC-15")

	if _, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "w1", Role: "herd-smith", TaskRole: "herd-smith", HoldIdentities: []lifecycle.HoldIdentity{{Repository: key.Repo, Owner: "worker", Lane: "smith", Scope: "lane"}, {Repository: key.Repo, Owner: "worker", Lane: "smith", Task: key.TaskRef, Scope: "task"}}}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	clk.advance(time.Nanosecond)

	var r Reconciler = mgr
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	claimed, err := mgr.IsClaimed(ctx, key)
	if err != nil {
		t.Fatalf("IsClaimed: %v", err)
	}
	if claimed {
		t.Fatal("expected Reconcile to sweep the zero-TTL lease as expired")
	}
}
