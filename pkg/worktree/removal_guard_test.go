package worktree

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// FAC-453: a worktree path with zero lease history was previously treated
// identically to a path whose lease was legitimately acquired and later
// released -- both had zero *active* claims, so both passed the fence. A
// worktree that was never dispatched through herd (e.g. a manual
// `git worktree add`) has no basis for the fence to call it safe.
func TestRefuseRemovalWithLiveLease_UnregisteredPathRefused(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, ".herd", "herdforge.db")
	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	target := filepath.Join(root, "never-leased")
	err = RefuseRemovalWithLiveLease(context.Background(), root, target)
	if err == nil {
		t.Fatal("expected removal of a never-leased path to be refused")
	}
	if !strings.Contains(err.Error(), "no lease history") {
		t.Fatalf("expected a lease-history refusal, got: %v", err)
	}
}

// A path whose lease was acquired and then released is the normal, safe
// case: herd dispatched real work there and the lease lifecycle completed.
// The fence must still allow removal once no active lease remains.
func TestRefuseRemovalWithLiveLease_ReleasedLeaseAllowed(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, ".herd", "herdforge.db")
	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	target := filepath.Join(root, "dispatched-and-done")
	key := claim.LeaseKey{Repo: "r", Provider: "p", Project: "proj", TaskRef: "FAC-999"}
	lease, err := store.Acquire(context.Background(), key, "owner", "worker", target, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Release(context.Background(), key, "owner", lease.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := RefuseRemovalWithLiveLease(context.Background(), root, target); err != nil {
		t.Fatalf("expected removal of a released-lease path to be allowed, got: %v", err)
	}
}

// A path with a still-active lease must be refused, matching the pre-FAC-453
// behavior this fence already provided.
func TestRefuseRemovalWithLiveLease_ActiveLeaseRefused(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, ".herd", "herdforge.db")
	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	target := filepath.Join(root, "actively-leased")
	key := claim.LeaseKey{Repo: "r", Provider: "p", Project: "proj", TaskRef: "FAC-998"}
	if _, err := store.Acquire(context.Background(), key, "owner", "worker", target, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}

	err = RefuseRemovalWithLiveLease(context.Background(), root, target)
	if err == nil {
		t.Fatal("expected removal of an actively-leased path to be refused")
	}
	if !strings.Contains(err.Error(), "live lease") {
		t.Fatalf("expected a live-lease refusal, got: %v", err)
	}
}
