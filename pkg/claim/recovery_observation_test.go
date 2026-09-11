package claim

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestWithSQLiteLeaseObservationSerializesCompetingRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.sqlite")
	store, err := NewSQLiteLeaseStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := LeaseKey{Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-788:recovery"}
	lease, err := store.Acquire(context.Background(), key, "coordinator-recovery", "recovery", "", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	err = WithSQLiteLeaseObservation(context.Background(), path, key, lease.ID, func(observed *Lease) error {
		if observed == nil || observed.ID != lease.ID || observed.Generation != lease.Generation {
			t.Fatalf("observation = %+v, want exact lease %+v", observed, lease)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, _, releaseErr := store.Release(ctx, key, lease.OwnerID, lease.Generation, time.Now()); releaseErr == nil {
			t.Fatal("competing release crossed the recovery publication lock")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.CurrentLease(context.Background(), key)
	if err != nil || current == nil || current.Status != StatusActive {
		t.Fatalf("observation altered lease state: %v lease=%+v", err, current)
	}
}
