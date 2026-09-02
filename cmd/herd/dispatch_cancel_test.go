package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

func TestParseDispatchCancelArgsRequiresExactLeaseGeneration(t *testing.T) {
	req, err := parseDispatchCancelArgs([]string{"FAC-350", "--lease", "7"})
	if err != nil {
		t.Fatal(err)
	}
	if req.TicketRef != "FAC-350" || req.LeaseGeneration != 7 {
		t.Fatalf("request = %+v, want FAC-350 generation 7", req)
	}

	if _, err := parseDispatchCancelArgs([]string{"FAC-350"}); err == nil {
		t.Fatal("missing lease generation was accepted")
	}
}

func TestReleaseCoordinationLeaseBoundedRefusesStaleGeneration(t *testing.T) {
	root := t.TempDir()
	store, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := claim.LeaseKey{Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-350"}
	first, err := store.Acquire(context.Background(), key, "coordinator-dispatch", "worker", "", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Release(context.Background(), key, "coordinator-dispatch", first.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	second, err := store.Acquire(context.Background(), key, "coordinator-dispatch", "worker", "", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseCoordinationLeaseBounded(root, key, "coordinator-dispatch", first.Generation); err != nil {
		t.Fatalf("idempotent stale-generation cancellation: %v", err)
	}
	active, err := store.ActiveClaims(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Generation != second.Generation {
		t.Fatalf("active leases after stale cancellation = %+v, want generation %d", active, second.Generation)
	}
}

func TestReleaseCoordinationLeaseCancellationLeavesNoActiveLease(t *testing.T) {
	root := t.TempDir()
	store, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := claim.LeaseKey{Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-350"}
	lease, err := store.Acquire(context.Background(), key, "coordinator-dispatch", "worker", "", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	if err := releaseCoordinationLeaseBounded(root, key, "coordinator-dispatch", lease.Generation); err != nil {
		t.Fatalf("cancel release: %v", err)
	}
	active, err := store.ActiveClaims(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active leases after cancellation = %+v, want none", active)
	}
}

func TestDispatchCancelReleasesMatchingLaunchClaim(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	key := claim.LeaseKey{Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-685"}
	coord, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer coord.Close()
	launch, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "launch-claims.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer launch.Close()
	c1, err := coord.Acquire(context.Background(), key, claim.CoordinatorDispatchOwner, "worker", "", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coord.Release(context.Background(), key, claim.CoordinatorDispatchOwner, c1.Generation, now); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.Acquire(context.Background(), key, "worker-pid4-0123456789abcdef", "worker", "", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	result, err := releaseCoordinationAndLaunchLeaseBounded(root, key, claim.CoordinatorDispatchOwner, c1.Generation)
	if err != nil {
		t.Fatalf("dual cancel: %v", err)
	}
	if result.Coordinator.Disposition != claim.DispositionAlreadyReleased {
		t.Fatalf("coordinator disposition=%s", result.Coordinator.Disposition)
	}
	if result.Launch.Disposition != claim.DispositionReleased {
		t.Fatalf("launch disposition=%s want released (FAC-685 leftover)", result.Launch.Disposition)
	}
	active, err := launch.ActiveClaims(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("launch-claim fixture still active after dispatch cancel: %+v", active)
	}
}

func TestDispatchCancelOutputNamesStoreTaskGenerationDisposition(t *testing.T) {
	var b strings.Builder
	printDispatchCancelStore(&b, claim.StoreReport{
		Store: "coordinator-store", TaskRef: "FAC-710", Generation: 1, Disposition: claim.DispositionReleased,
	})
	got := b.String()
	if !strings.Contains(got, "store=coordinator-store") || !strings.Contains(got, "task=FAC-710") || !strings.Contains(got, "generation=1") || !strings.Contains(got, "disposition=released") {
		t.Fatalf("CLI report %q missing required fields", got)
	}
	if strings.Contains(got, "coordinator-dispatch") || strings.Contains(got, "pid") {
		t.Fatalf("CLI report leaked owner identity: %q", got)
	}
}
