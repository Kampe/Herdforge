package claim

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	_ "modernc.org/sqlite"
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

func TestProviderLockKindSchemaAndRowsFailClosed(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE leases (provider_lock_kind INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := ensureProviderLockKind(db); err == nil {
		t.Fatal("incompatible provider_lock_kind schema was accepted")
	}

	store, _, lease, _ := recoveryFixture(t, "FAC-kind", "repo")
	if _, err := store.db.Exec(`UPDATE leases SET provider_lock_kind='bogus' WHERE id=?`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PeekStaleProviderLock(context.Background(), lease.LeaseKey, time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("unknown runtime provider lock kind was accepted")
	}
}

func TestProviderLockCoherenceReservedOwnerAndZeroRows(t *testing.T) {
	store, _, lease, _ := recoveryFixture(t, "FAC-coherence", "repo")
	ctx := context.Background()
	if _, err := store.db.Exec(`UPDATE leases SET provider_lock_owner='ordinary', provider_lock_at=NULL WHERE id=?`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PeekAllStaleProviderLocks(ctx, time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("ordinary owner/timestamp mismatch was accepted")
	}

	store2, _, lease2, _ := recoveryFixture(t, "FAC-recovery", "repo")
	if _, err := store2.db.Exec(`UPDATE leases SET provider_lock_kind='recovery', provider_lock_owner=?, provider_lock_at=? WHERE id=?`, recoveryOwnerFor(lease2.ID+1, lease2.Generation), time.Date(2026, 8, 4, 11, 0, 0, 0, time.UTC), lease2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store2.ObserveStaleProviderLock(ctx, lease2.LeaseKey, time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("wrong-row recovery owner was accepted")
	}

	store3, _, lease3, _ := recoveryFixture(t, "FAC-zero", "repo")
	if err := store3.ReleaseProviderLock(ctx, lease3.LeaseKey, lease3.Generation, "missing"); err == nil {
		t.Fatal("zero-row release returned success")
	}
	if err := store3.ForceReleaseProviderLock(ctx, lease3.LeaseKey, lease3.Generation); err == nil {
		t.Fatal("zero-row force release returned success")
	}
}

func TestAcquireProviderLockRejectsEntireReservedNamespace(t *testing.T) {
	store, _, lease, _ := recoveryFixture(t, "FAC-reserved", "repo")
	for _, owner := range []string{"herd-provider-recovery:", "herd-provider-recovery:bad", "herd-provider-recovery:1:1", "herd-provider-recovery:1:0"} {
		if _, err := store.AcquireProviderLock(context.Background(), lease.LeaseKey, lease.OwnerID, lease.Generation, owner, time.Minute, time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)); err == nil {
			t.Fatalf("reserved or malformed recovery owner %q was admitted", owner)
		}
	}
	if _, err := store.AcquireProviderLock(context.Background(), lease.LeaseKey, lease.OwnerID, lease.Generation, "recovery-lookalike", time.Minute, time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("ordinary lookalike owner was incorrectly rejected: %v", err)
	}
}

func TestMemoryProviderLockCASRequiresExactObservation(t *testing.T) {
	store := NewInMemoryLeaseStore()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	key := LeaseKey{Repo: "repo", Provider: "memory", Project: "p", TaskRef: "FAC-memory"}
	lease, err := store.Acquire(context.Background(), key, "owner", "worker", "/wt", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireProviderLock(context.Background(), key, lease.OwnerID, lease.Generation, "ordinary", time.Minute, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	obs, err := store.ObserveStaleProviderLock(context.Background(), key, now)
	if err != nil || obs == nil {
		t.Fatalf("observe stale lock: %+v %v", obs, err)
	}
	for name, mutate := range map[string]func(*ProviderLockObservation){
		"id":         func(o *ProviderLockObservation) { o.LeaseID++ },
		"generation": func(o *ProviderLockObservation) { o.Generation++ },
		"owner":      func(o *ProviderLockObservation) { o.Owner = "other" },
		"timestamp":  func(o *ProviderLockObservation) { o.LockedAt = o.LockedAt.Add(time.Second) },
	} {
		bad := *obs
		mutate(&bad)
		if ok, err := store.ClaimProviderLockCAS(context.Background(), bad); err != nil || ok {
			t.Fatalf("%s mismatch was accepted: ok=%v err=%v", name, ok, err)
		}
	}
	ok, err := store.ClaimProviderLockCAS(context.Background(), *obs)
	if err != nil || !ok {
		t.Fatalf("valid recovery claim failed: %v", err)
	}
	badFinalize := *obs
	badFinalize.ObservedAt = badFinalize.ObservedAt.Add(time.Second)
	if ok, err := store.FinalizeProviderLockCAS(context.Background(), badFinalize); err != nil || ok {
		t.Fatalf("zero-row finalize was accepted: ok=%v err=%v", ok, err)
	}
	store.provLock[lease.ID].kind = "corrupt"
	if _, err := store.ObserveStaleProviderLock(context.Background(), key, now); err == nil {
		t.Fatal("memory corrupt lock kind was accepted")
	}
}

func TestSQLiteProviderLockStalenessUsesChronologicalOffsetTimes(t *testing.T) {
	ctx := context.Background()
	key := testKey("FAC-OFFSET")
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.FixedZone("CST", -6*60*60))
	for _, tc := range []struct {
		name  string
		age   time.Duration
		stale bool
	}{{"fresh", 4 * time.Minute, false}, {"stale", 6 * time.Minute, true}} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			lease, err := store.AcquireWithIdentity(ctx, key, "w1", "worker", "/wt", key.Repo, "worker", "smith", base, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			lockAt := base.Add(-tc.age)
			if _, err := store.AcquireProviderLock(ctx, key, lease.OwnerID, lease.Generation, "lock", time.Minute, lockAt); err != nil {
				t.Fatal(err)
			}
			now := base
			obs, err := store.ObserveStaleProviderLock(ctx, key, now)
			if err != nil {
				t.Fatal(err)
			}
			if (obs != nil) != tc.stale {
				t.Fatalf("stale=%v observation=%+v", tc.stale, obs)
			}
			if tc.stale {
				ok, err := store.ClaimProviderLockCAS(ctx, *obs)
				reobserved, readErr := store.ObserveStaleProviderLock(ctx, key, now)
				if err != nil || readErr != nil || !ok || reobserved == nil || !reobserved.Recovery || reobserved.LeaseID != lease.ID || reobserved.Generation != lease.Generation || reobserved.RecoveryOwner != recoveryOwnerFor(lease.ID, lease.Generation) || reobserved.LockedAt.IsZero() {
					t.Fatalf("claim recovery: ok=%v observed=%+v reobserved=%+v err=%v readErr=%v", ok, obs, reobserved, err, readErr)
				}
			}
		})
	}
}

func TestHistoricalLeaseSnapshotIncludesExpiredAndReleasedRows(t *testing.T) {
	store, authority, lease, _ := recoveryFixture(t, "FAC-history", "repo")
	ctx := context.Background()
	snap, ok := any(store).(LeaseSnapshotStore)
	if !ok {
		t.Fatal("sqlite store lacks historical snapshot interface")
	}
	current, err := snap.CurrentLease(ctx, lease.LeaseKey)
	if err != nil || current == nil {
		t.Fatalf("current snapshot: %+v %v", current, err)
	}
	if _, changed, err := store.ExpireLeaseCAS(ctx, lease.ID, lease.Generation, time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)); err != nil || !changed {
		t.Fatalf("expire incumbent: changed=%v err=%v", changed, err)
	}
	expired, err := snap.CurrentLease(ctx, lease.LeaseKey)
	if err != nil || expired == nil || expired.Status != StatusExpired {
		t.Fatalf("expired incumbent was not readable: %+v %v", expired, err)
	}
	byGeneration, err := snap.LeaseByGeneration(ctx, lease.LeaseKey, lease.OwnerID, lease.Generation)
	if err != nil || byGeneration == nil || byGeneration.ID != lease.ID {
		t.Fatalf("historical generation missing: %+v %v", byGeneration, err)
	}
	claimed, changed, err := store.ClaimCapacityReleaseExact(ctx, lease.ID, lease.Generation, "settler", time.Minute, time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC))
	if err != nil || !changed || claimed == nil {
		t.Fatalf("claim capacity for historical row: %+v %v %v", claimed, changed, err)
	}
	if err := store.AckCapacityRelease(ctx, lease.ID, "settler", time.Date(2026, 8, 4, 13, 1, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	settled, err := snap.LeaseByGeneration(ctx, lease.LeaseKey, lease.OwnerID, lease.Generation)
	if err != nil || settled == nil || settled.CapacityReleasedAt == nil {
		t.Fatalf("settled historical row missing ack: %+v %v", settled, err)
	}
	mgr := NewClaimManager(store)
	if _, err := mgr.settleCapacityExact(ctx, settled); err != nil {
		t.Fatalf("already-settled replay was not idempotent: %v", err)
	}

	releasedStore, _, releasedLease, releasedTask := recoveryFixture(t, "FAC-history-released", "repo")
	if _, _, err := releasedStore.Release(ctx, releasedLease.LeaseKey, releasedLease.OwnerID, releasedLease.Generation, time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	releasedSnap, ok := any(releasedStore).(LeaseSnapshotStore)
	if !ok {
		t.Fatal("sqlite store lacks historical snapshot interface")
	}
	releasedRow, err := releasedSnap.LeaseByGeneration(ctx, releasedLease.LeaseKey, releasedLease.OwnerID, releasedLease.Generation)
	if err != nil || releasedRow == nil || releasedRow.Status != StatusReleased {
		t.Fatalf("expected actual released row: %+v %v", releasedRow, err)
	}
	releasedClaimMgr := NewClaimManager(releasedStore, WithHoldReader(authority))
	if err := releasedClaimMgr.Release(ctx, releasedLease.LeaseKey, releasedLease.OwnerID, releasedLease.Generation); err != nil {
		t.Fatalf("public released replay: %v", err)
	}
	claimReq := ClaimRequest{Key: releasedLease.LeaseKey, OwnerID: "new-owner", Role: "worker", TaskRole: "worker", HoldIdentities: []lifecycle.HoldIdentity{{Repository: releasedLease.Repo, Owner: releasedLease.HoldOwner, Lane: releasedLease.HoldLane, Scope: "lane"}, releasedTask}}
	if _, err := releasedClaimMgr.Claim(ctx, claimReq); err != nil {
		t.Fatalf("public claim after released replay: %v", err)
	}
}

func TestSQLiteAbortUnreservedLeasePreservesExactHistoricalIdentityAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases.db")
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatal(err)
	}
	key := LeaseKey{Repo: "repo", Provider: "memory", Project: "p", TaskRef: "FAC-abort-history"}
	incumbent, err := store.AcquireWithIdentity(ctx, key, "old", "worker", "/incumbent", key.Repo, "worker", "smith", base.Add(-2*time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claimNow := base.Add(123456 * time.Nanosecond)
	replacement, err := store.AcquireWithIdentity(ctx, key, "new", "worker", "/replacement-exact", key.Repo, "worker", "smith", claimNow, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	persistedBeforeAbort, err := store.LeaseByGeneration(ctx, key, replacement.OwnerID, replacement.Generation)
	if err != nil || persistedBeforeAbort == nil || !sameLeaseImmutable(replacement, persistedBeforeAbort) {
		t.Fatalf("acquire returned noncanonical claimed_at identity: returned=%+v persisted=%+v err=%v", replacement, persistedBeforeAbort, err)
	}
	changedSnapshot := *replacement
	changedSnapshot.ClaimedAt = changedSnapshot.ClaimedAt.Add(time.Microsecond)
	if _, changed, err := store.AbortUnreservedLease(ctx, &changedSnapshot, base.Add(time.Minute)); err == nil || changed {
		t.Fatalf("changed claimed_at snapshot was accepted: changed=%v err=%v", changed, err)
	}
	active, err := store.CurrentLease(ctx, key)
	if err != nil || active == nil || active.Status != StatusActive {
		t.Fatalf("failed abort mutated replacement: %+v %v", active, err)
	}
	aborted, changed, err := store.AbortUnreservedLease(ctx, replacement, base.Add(time.Minute))
	if err != nil || !changed || aborted == nil {
		t.Fatalf("abort replacement: row=%+v changed=%v err=%v", aborted, changed, err)
	}
	assertCancelledAbortRow(t, replacement, aborted)
	if pending, err := store.PendingCapacityReleases(ctx); err != nil || len(pending) != 1 || pending[0].ID != incumbent.ID {
		t.Fatalf("only incumbent should remain pending: %+v %v", pending, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	historical, err := reopened.LeaseByGeneration(ctx, key, replacement.OwnerID, replacement.Generation)
	if err != nil || historical == nil {
		t.Fatalf("reopen historical replacement: %+v %v", historical, err)
	}
	assertCancelledAbortRow(t, replacement, historical)
	if pending, err := reopened.PendingCapacityReleases(ctx); err != nil || len(pending) != 1 || pending[0].ID != incumbent.ID {
		t.Fatalf("reopen exposed replacement for settlement: %+v %v", pending, err)
	}
	if _, changed, err := reopened.ClaimCapacityReleaseExact(ctx, replacement.ID, replacement.Generation, "settler", time.Minute, base.Add(2*time.Minute)); err != nil || changed {
		t.Fatalf("cancelled replacement became settleable: changed=%v err=%v", changed, err)
	}
	if _, changed, err := reopened.ClaimCapacityReleaseExact(ctx, incumbent.ID, incumbent.Generation, "settler", time.Minute, base.Add(2*time.Minute)); err != nil || !changed {
		t.Fatalf("incumbent was not the sole settlement retry: changed=%v err=%v", changed, err)
	}
}

func TestInMemoryAbortUnreservedLeaseBindsClaimedAt(t *testing.T) {
	store := NewInMemoryLeaseStore()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	key := LeaseKey{Repo: "repo", Provider: "memory", Project: "p", TaskRef: "FAC-abort-memory"}
	lease, err := store.AcquireWithIdentity(ctx, key, "new", "worker", "/replacement", key.Repo, "worker", "smith", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	changedSnapshot := *lease
	changedSnapshot.ClaimedAt = changedSnapshot.ClaimedAt.Add(time.Microsecond)
	if _, changed, err := store.AbortUnreservedLease(ctx, &changedSnapshot, now.Add(time.Minute)); err == nil || changed {
		t.Fatalf("changed claimed_at snapshot was accepted: changed=%v err=%v", changed, err)
	}
	current, err := store.CurrentLease(ctx, key)
	if err != nil || current == nil || current.Status != StatusActive {
		t.Fatalf("failed memory abort mutated lease: %+v %v", current, err)
	}
}

func TestProviderAndSettlerIdentitiesRejectWhitespaceWithoutMutation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		store LeaseStore
	}{
		{name: "memory", store: NewInMemoryLeaseStore()},
		{name: "sqlite", store: func() LeaseStore {
			s, err := NewSQLiteLeaseStore(filepath.Join(t.TempDir(), "identity.db"))
			if err != nil {
				t.Fatal(err)
			}
			return s
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if closer, ok := tc.store.(interface{ Close() error }); ok {
				defer closer.Close()
			}
			key := LeaseKey{Repo: "repo", Provider: "memory", Project: "p", TaskRef: "FAC-identity"}
			atomicStore, ok := tc.store.(AtomicLeaseStore)
			if !ok {
				t.Fatal("store lacks atomic acquire")
			}
			lease, err := atomicStore.AcquireWithIdentity(ctx, key, "owner", "worker", "/wt", key.Repo, "worker", "smith", now.Add(-time.Hour), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			for _, owner := range []string{"", " ", " owner", "owner "} {
				if _, err := tc.store.AcquireProviderLock(ctx, key, lease.OwnerID, lease.Generation, owner, time.Minute, now); err == nil {
					t.Fatalf("invalid provider owner %q was accepted", owner)
				}
				current, readErr := tc.store.(LeaseSnapshotStore).LeaseByGeneration(ctx, key, lease.OwnerID, lease.Generation)
				if readErr != nil || current == nil || !sameLeaseImmutable(lease, current) || current.Status != StatusActive {
					t.Fatalf("invalid provider owner %q mutated lease: %+v %v", owner, current, readErr)
				}
			}
			if _, _, err := tc.store.Release(ctx, key, lease.OwnerID, lease.Generation, now); err != nil {
				t.Fatal(err)
			}
			released, err := tc.store.(LeaseSnapshotStore).LeaseByGeneration(ctx, key, lease.OwnerID, lease.Generation)
			if err != nil || released == nil {
				t.Fatalf("release for capacity test: %+v %v", released, err)
			}
			exact := tc.store.(ExactCapacityReleaseStore)
			for _, settler := range []string{"", " ", " settler", "settler "} {
				if _, changed, err := exact.ClaimCapacityReleaseExact(ctx, lease.ID, lease.Generation, settler, time.Minute, now); err == nil || changed {
					t.Fatalf("invalid settler %q claim result: changed=%v err=%v", settler, changed, err)
				}
			}
			for _, timeout := range []time.Duration{0, -time.Second} {
				if _, changed, err := exact.ClaimCapacityReleaseExact(ctx, lease.ID, lease.Generation, "settler", timeout, now); err == nil || changed {
					t.Fatalf("invalid timeout %s claim result: changed=%v err=%v", timeout, changed, err)
				}
			}
			claimed, changed, err := exact.ClaimCapacityReleaseExact(ctx, lease.ID, lease.Generation, "settler", time.Minute, now)
			if err != nil || !changed || claimed == nil {
				t.Fatalf("valid settler claim: %+v changed=%v err=%v", claimed, changed, err)
			}
			for _, settler := range []string{"", " ", " settler", "settler "} {
				if err := tc.store.AckCapacityRelease(ctx, lease.ID, settler, now); err == nil {
					t.Fatalf("invalid settler %q ack was accepted", settler)
				}
				if err := tc.store.FailCapacityRelease(ctx, lease.ID, settler); err == nil {
					t.Fatalf("invalid settler %q fail was accepted", settler)
				}
			}
			if err := tc.store.AckCapacityRelease(ctx, lease.ID, "settler", now); err != nil {
				t.Fatalf("invalid settler mutation changed claim ownership: %v", err)
			}
		})
	}
}

func TestClaimManagerInvalidSettlerFailsClosed(t *testing.T) {
	store, authority, lease, _ := recoveryFixture(t, "FAC-invalid-settler", "repo")
	if _, changed, err := store.ExpireLeaseCAS(context.Background(), lease.ID, lease.Generation, time.Date(2026, 8, 4, 13, 0, 0, 0, time.UTC)); err != nil || !changed {
		t.Fatalf("expire fixture lease: changed=%v err=%v", changed, err)
	}
	mgr := NewClaimManager(store, WithHoldReader(authority), WithSettlerID(" "))
	if _, err := mgr.SettlePendingCapacity(context.Background()); err == nil {
		t.Fatal("invalid configured settler unexpectedly settled capacity")
	}
	validTimeoutMgr := NewClaimManager(store, WithHoldReader(authority), WithSettlerID("settler"), WithCapacityClaimTimeout(0))
	if _, err := validTimeoutMgr.SettlePendingCapacity(context.Background()); err == nil {
		t.Fatal("non-positive configured timeout unexpectedly settled capacity")
	}
}

func assertCancelledAbortRow(t *testing.T, original, row *Lease) {
	t.Helper()
	if !sameLeaseImmutable(original, row) || row.Status != StatusReleased || row.ReleasedAt == nil || row.CapacityReleaseState != "cancelled" || row.CapacityReleasedAt != nil {
		t.Fatalf("incomplete cancelled abort row: original=%+v row=%+v", original, row)
	}
	if row.WorktreePath != original.WorktreePath || row.ClaimedAt != original.ClaimedAt {
		t.Fatalf("abort row lost immutable identity: original=%+v row=%+v", original, row)
	}
}
