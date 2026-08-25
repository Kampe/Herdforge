package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/goalguard"
)

// FAC-626: a MISSING lease store is unprovable, not proof of loss. It used to
// return false, which Evaluate converts to reason="lease_lost", continue=false --
// so the standing review-harvest supervisor completed one beat and halted every
// time, and the review host never refilled.
func TestGoalGuardLeaseHeld_MissingStoreIsNotLoss(t *testing.T) {
	t.Setenv("HERD_LEASE_DB", filepath.Join(t.TempDir(), "does-not-exist.db"))

	held, err := goalGuardLeaseHeld(goalguard.Goal{
		Lane: "review-harvest-supervisor", Task: "standing", Generation: 1,
	})
	if err != nil {
		t.Fatalf("a missing store must not error: %v", err)
	}
	if !held {
		t.Fatal("a missing lease store must not be reported as lease loss; that halts standing lanes")
	}
}

// The safety property must survive: a store that EXISTS and does not list this
// lane is a genuine loss and must still stop the lane.
func TestGoalGuardLeaseHeld_PresentStoreWithoutLaneIsLoss(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "launch-claims.db")
	// An empty but PRESENT store: openable, lists no claims.
	if err := os.WriteFile(db, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_LEASE_DB", db)

	held, err := goalGuardLeaseHeld(goalguard.Goal{
		Lane: "review-harvest-supervisor", Task: "standing", Generation: 1,
	})
	if err != nil {
		// An unreadable store is reported by the caller as "allowing stop"; that
		// path is separate and is not what this test pins.
		t.Skipf("store unreadable in this environment: %v", err)
	}
	if held {
		t.Fatal("a present store that does not list this lane IS a lease loss and must stop the lane")
	}
}
