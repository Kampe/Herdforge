package main

import (
	"context"
	"path/filepath"
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
	store, err := claim.NewSQLiteLeaseStore(filepath.Join(root, "herdforge.db"))
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
	if err := releaseCoordinationLeaseBounded(root, key, "coordinator-dispatch", first.Generation); err == nil {
		t.Fatal("stale generation cancellation released a later lease")
	}
	active, err := store.ActiveClaims(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Generation != second.Generation {
		t.Fatalf("active leases after stale cancellation = %+v, want generation %d", active, second.Generation)
	}
}
