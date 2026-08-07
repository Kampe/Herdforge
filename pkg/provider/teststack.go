package provider

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// NewTestStack opens an isolated ClaimStack under t.TempDir() and
// registers Close. Use this in fixtures so every production fail-closed
// path is exercised with a real stack, never a nil Claims fallback.
func NewTestStack(t testing.TB, tp TaskProvider) *ClaimStack {
	t.Helper()
	stack, err := OpenClaimStack(t.TempDir(), tp)
	if err != nil {
		t.Fatalf("provider.NewTestStack: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close() })
	return stack
}

// NewTestStackWithBusy builds a ClaimStack whose fence exclusive lock
// uses a short busy_timeout (contention / timeout tests).
func NewTestStackWithBusy(t testing.TB, tp TaskProvider, busy time.Duration) *ClaimStack {
	t.Helper()
	dir := t.TempDir()
	leasePath := filepath.Join(dir, "leases.db")
	outboxPath := filepath.Join(dir, "outbox.db")
	fencePath := filepath.Join(dir, "fences.db")

	leases, err := claim.NewSQLiteLeaseStore(leasePath)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := claim.NewSQLiteOutbox(outboxPath)
	if err != nil {
		_ = leases.Close()
		t.Fatal(err)
	}
	fences, err := NewSQLiteFenceStoreWithBusy(fencePath, busy)
	if err != nil {
		_ = outbox.Close()
		_ = leases.Close()
		t.Fatal(err)
	}
	AttachAuthoritativeReceiver(tp, fences)
	cas, err := NewFencedCAS(fences, tp)
	if err != nil {
		_ = fences.Close()
		_ = outbox.Close()
		_ = leases.Close()
		t.Fatal(err)
	}
	board, err := NewFencedBoard(cas, tp)
	if err != nil {
		_ = cas.Close()
		_ = outbox.Close()
		_ = leases.Close()
		t.Fatal(err)
	}
	mgr := claim.NewClaimManager(leases,
		claim.WithProviderCAS(cas),
		claim.WithDurableOutbox(outbox),
	)
	stack := &ClaimStack{
		Dir: dir, Leases: leases, Outbox: outbox,
		Fences: fences, CAS: cas, Board: board, Manager: mgr, TP: tp,
	}
	t.Cleanup(func() { _ = stack.Close() })
	return stack
}

// MustAcquireLease claims key and advances the board fence for taskID.

// HoldIdentitiesFor builds the exact lane/task composite Claim requires.
func HoldIdentitiesFor(key claim.LeaseKey, role string) []lifecycle.HoldIdentity {
	if role == "" {
		role = "worker"
	}
	return []lifecycle.HoldIdentity{
		{Repository: key.Repo, Owner: role, Lane: role, Scope: "lane"},
		{Repository: key.Repo, Owner: role, Lane: role, Task: key.TaskRef, Scope: "task"},
	}
}

// ClaimRequestFor is the production-shaped ClaimRequest for tests.
func ClaimRequestFor(key claim.LeaseKey, ownerID, role string) claim.ClaimRequest {
	if role == "" {
		role = "worker"
	}
	return claim.ClaimRequest{
		Key: key, OwnerID: ownerID, Role: role, TaskRole: role,
		HoldIdentities: HoldIdentitiesFor(key, role),
	}
}

func MustAcquireLease(t testing.TB, stack *ClaimStack, key claim.LeaseKey, owner, role, taskID string) *claim.Lease {
	t.Helper()
	lease, err := stack.AcquireLease(context.Background(), key, owner, role, role)
	if err != nil {
		t.Fatalf("MustAcquireLease: %v", err)
	}
	if err := stack.CAS.AdvanceFence(context.Background(), taskID, lease.Generation); err != nil {
		t.Fatalf("MustAcquireLease AdvanceFence: %v", err)
	}
	return lease
}
