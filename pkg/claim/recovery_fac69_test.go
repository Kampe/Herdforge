package claim

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

func recoveryFixture(t *testing.T, task string, repo string) (*SQLiteLeaseStore, *lifecycle.HoldAuthority, *Lease, lifecycle.HoldIdentity) {
	t.Helper()
	store, err := NewSQLiteLeaseStore(filepath.Join(t.TempDir(), "leases.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	authority, err := lifecycle.NewHoldAuthorityWithClock(filepath.Join(t.TempDir(), "lifecycle.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	taskIdentity := lifecycle.HoldIdentity{Repository: repo, Owner: "worker", Lane: "smith", Task: task, Scope: "task"}
	laneIdentity := lifecycle.HoldIdentity{Repository: repo, Owner: "worker", Lane: "smith", Scope: "lane"}
	lease, err := store.AcquireWithIdentity(context.Background(), LeaseKey{Repo: repo, Provider: "memory", Project: "p", TaskRef: task}, "owner", "worker", "/wt", repo, "worker", "smith", now.Add(-time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_ = taskIdentity
	_ = laneIdentity
	return store, authority, lease, taskIdentity
}

func TestExpireStaleRequiresAuthorityBeforeSnapshotMutation(t *testing.T) {
	store, authority, lease, _ := recoveryFixture(t, "FAC-1", "repo")
	_ = authority
	mgr := NewClaimManager(store)
	if _, err := mgr.ExpireStale(context.Background()); err == nil {
		t.Fatal("missing authority unexpectedly recovered lease")
	}
	current, err := store.currentActive(context.Background(), lease.LeaseKey)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Status != StatusActive {
		t.Fatalf("missing-authority path mutated lease: %+v", current)
	}
}

func TestExpireStalePrevalidatesAllCandidatesBeforeMutation(t *testing.T) {
	store, authority, good, _ := recoveryFixture(t, "FAC-1", "repo")
	bad, err := store.Acquire(context.Background(), LeaseKey{Repo: "repo", Provider: "memory", Project: "p", TaskRef: "FAC-2"}, "owner2", "worker", "/wt2", time.Now().Add(-time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewClaimManager(store, WithHoldReader(authority))
	if _, err := mgr.ExpireStale(context.Background()); err == nil {
		t.Fatal("malformed candidate unexpectedly admitted")
	}
	first, err := store.currentActive(context.Background(), good.LeaseKey)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.currentActive(context.Background(), bad.LeaseKey)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil || first.Status != StatusActive || second.Status != StatusActive {
		t.Fatalf("malformed candidate mutated prefix: first=%+v second=%+v", first, second)
	}
}

func TestExpireStaleHeldCandidatePreservedAndUnheldExpires(t *testing.T) {
	store, authority, heldLease, heldTask := recoveryFixture(t, "FAC-held", "repo")
	free, err := store.AcquireWithIdentity(context.Background(), LeaseKey{Repo: "repo", Provider: "memory", Project: "p", TaskRef: "FAC-free"}, "owner2", "worker", "/wt2", "repo", "worker", "scout", time.Date(2026, 8, 4, 11, 0, 0, 0, time.UTC), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	lane := lifecycle.HoldIdentity{Repository: "repo", Owner: "worker", Lane: "smith", Scope: "lane"}
	if _, err := authority.Hold(context.Background(), lane, "actor", "maintenance", "operator_hold", 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Hold(context.Background(), heldTask, "actor", "maintenance", "operator_hold", 1, nil); err != nil {
		t.Fatal(err)
	}
	_ = heldLease
	mgr := NewClaimManager(store, WithHoldReader(authority))
	if _, err := mgr.ExpireStale(context.Background()); err != nil {
		t.Fatalf("unexpected recovery error: %v", err)
	}
	gotHeld, _ := store.currentActive(context.Background(), heldLease.LeaseKey)
	gotFree, _ := store.byGeneration(context.Background(), free.LeaseKey, free.OwnerID, free.Generation)
	if gotHeld == nil || gotHeld.Status != StatusActive {
		t.Fatalf("held lease was mutated: %+v", gotHeld)
	}
	if gotFree == nil || gotFree.Status != StatusExpired {
		t.Fatalf("unheld lease did not expire: %+v", gotFree)
	}
}
