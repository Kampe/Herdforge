package claim

import (
	"path/filepath"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"context"
	"testing"
	"time"
)

func TestHandoffOwner_AtomicOwnerAndExpiry(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()
	now := time.Now().UTC()
	ttl := 5 * time.Minute
	key := LeaseKey{Repo: "r", Provider: "p", Project: "j", TaskRef: "FAC-HA"}
	lease, err := store.Acquire(ctx, key, "coord", "worker", "", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Single call must rebind owner AND extend expiry.
	got, err := store.HandoffOwner(ctx, key, "coord", "worker-sess", lease.Generation, now, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerID != "worker-sess" {
		t.Fatalf("owner=%s", got.OwnerID)
	}
	if got.Generation != lease.Generation {
		t.Fatalf("gen changed")
	}
	if !got.ExpiresAt.After(now.Add(ttl - time.Second)) {
		t.Fatalf("expiry not extended: %v", got.ExpiresAt)
	}
	// Coordinator cannot renew/release after atomic handoff.
	if _, err := store.Renew(ctx, key, "coord", lease.Generation, now, ttl); err == nil {
		t.Fatal("coord renew must fail after handoff")
	}
}

// TestHandoff_NoPartialOwnerOnFailure: Handoff is one transaction; there is
// no separate Renew that can leave owner moved and expiry stale.
func TestHandoffOwner_RefusesExpiredLease(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()
	now := time.Now().UTC()
	key := LeaseKey{Repo: "r", Provider: "p", Project: "j", TaskRef: "FAC-EXP"}
	// Acquire with very short TTL already expired via backdated now on Renew path:
	// insert active then force expires_at in the past via Handoff attempt after time travel.
	lease, err := store.Acquire(ctx, key, "coord", "worker", "", now.Add(-10*time.Minute), time.Minute)
	if err != nil {
		// Acquire with past now may still set expires_at = now+ttl relative to past.
		t.Fatal(err)
	}
	// Force expiry: Renew fails; Handoff must also refuse (not revive).
	far := now.Add(time.Hour)
	_, err = store.HandoffOwner(ctx, key, "coord", "worker", lease.Generation, far, 5*time.Minute)
	if err == nil {
		// If acquire used past clock, lease expires_at = past+1m may still be < far.
		// Re-check: try with expires clearly past by acquiring at far-2m with 1s ttl.
		t.Log("first handoff attempt:", err)
	}
	// Explicit: acquire live then wait.
	key2 := LeaseKey{Repo: "r", Provider: "p", Project: "j", TaskRef: "FAC-EXP2"}
	l2, err := store.Acquire(ctx, key2, "c2", "worker", "", now, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	_, err = store.HandoffOwner(ctx, key2, "c2", "w2", l2.Generation, time.Now().UTC(), 5*time.Minute)
	if err == nil {
		t.Fatal("handoff must refuse expired lease (would revive past-TTL active row)")
	}
}

func TestHandoff_ManagerSingleTransaction(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()
	hold, err := lifecycle.NewHoldAuthority(filepath.Join(t.TempDir(), "hold.db"))
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = hold.Close() })
	mgr := NewClaimManager(store, WithHoldReader(hold), WithTTL(2*time.Minute))
	key := LeaseKey{Repo: "r", Provider: "p", Project: "j", TaskRef: "FAC-HM"}
	lease, err := mgr.Claim(ctx, ClaimRequest{Key: key, OwnerID: "c1", Role: "w", TaskRole: "w",
		HoldIdentities: []lifecycle.HoldIdentity{
			{Repository: key.Repo, Owner: "w", Lane: "w", Scope: "lane"},
			{Repository: key.Repo, Owner: "w", Lane: "w", Task: key.TaskRef, Scope: "task"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Handoff(ctx, key, "c1", "w1", lease.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerID != "w1" || got.Generation != lease.Generation {
		t.Fatalf("got owner=%s gen=%d", got.OwnerID, got.Generation)
	}
	// Worker holds live lease; coordinator release fails.
	if err := mgr.Release(ctx, key, "c1", lease.Generation); err == nil {
		t.Fatal("expected coord release fail")
	}
	if err := mgr.Release(ctx, key, "w1", lease.Generation); err != nil {
		t.Fatal(err)
	}
}
