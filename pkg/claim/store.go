package claim

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrAlreadyClaimed is wrapped by ClaimConflictError; kept as a
	// standalone sentinel for errors.Is checks that don't need the detail.
	ErrAlreadyClaimed = errors.New("claim: key already actively leased")
	// ErrStaleGeneration means the caller's fencing token no longer
	// matches the active lease (superseded by a later claim).
	ErrStaleGeneration = errors.New("claim: fencing rejected stale generation")
	// ErrNotFound means no lease exists for the given key/owner/generation.
	ErrNotFound = errors.New("claim: lease not found")
)

// LeaseStore is the narrow durable-persistence port ClaimManager depends
// on. All methods must be safe to call from multiple OS processes against
// the same backing store concurrently; exactly one caller may win a
// contended Acquire, and Release/ExpireStale transitions must each fire
// at most once per lease so capacity accounting stays exactly-once.
//
// FAC-120/FAC-119 boundary: this package ships SQLiteLeaseStore, a
// self-contained implementation, so cross-process leases work standalone
// today. FAC-119's durable lifecycle/event-log/outbox service is the
// intended long-term home for lease persistence, so lease transitions
// share one transactional outbox with other lifecycle events instead of
// committing to a private database. Integrating that means supplying a
// different LeaseStore implementation to NewClaimManager (or having the
// lifecycle service wrap SQLiteLeaseStore) — ClaimManager's contract with
// LeaseStore does not need to change for that swap.
type LeaseStore interface {
	// Acquire atomically creates a new active lease for key if none is
	// currently active and unexpired. An active-but-expired lease is
	// transitioned to Expired as part of winning the acquire. Generation
	// is monotonically increasing per key. On conflict with a live lease,
	// returns a *ClaimConflictError wrapping ErrAlreadyClaimed.
	Acquire(ctx context.Context, key LeaseKey, ownerID, role, worktreePath string, now time.Time, ttl time.Duration) (*Lease, error)

	// Renew extends an active lease's expiry, fenced by generation. A
	// generation that no longer matches the active lease returns
	// ErrStaleGeneration (or ErrNotFound if the lease is gone entirely).
	Renew(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time, ttl time.Duration) (*Lease, error)

	// Release marks the fenced lease released. Idempotent: releasing an
	// already-released lease at the same generation returns the lease
	// with transitioned=false and a nil error. transitioned=true means
	// this call performed the active->released flip and the caller must
	// release capacity for it exactly once.
	Release(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time) (lease *Lease, transitioned bool, err error)

	// Hold sets or clears the operator-hold flag on the active lease for
	// key, exempting it from expiry-based reclaim while held.
	Hold(ctx context.Context, key LeaseKey, held bool, now time.Time) (*Lease, error)

	// ExpireStale transitions active-but-expired (and not held) leases to
	// Expired and returns exactly the ones this call transitioned, so
	// callers release capacity exactly once per lease.
	ExpireStale(ctx context.Context, now time.Time) ([]*Lease, error)

	// ActiveClaims returns only live (Active, unexpired) leases.
	ActiveClaims(ctx context.Context, now time.Time) ([]*Lease, error)

	Close() error
}

// ClaimConflictError reports why an Acquire lost the race, with enough
// detail for callers/dashboards to explain the block: current owner,
// generation, and expiry.
type ClaimConflictError struct {
	Key    LeaseKey
	Lease  *Lease
	Reason string
}

func (e *ClaimConflictError) Error() string {
	return fmt.Sprintf("claim: %s held by %s (generation %d, expires %s): %s",
		e.Key.TaskRef, e.Lease.OwnerID, e.Lease.Generation, e.Lease.ExpiresAt.Format(time.RFC3339), e.Reason)
}

func (e *ClaimConflictError) Unwrap() error { return ErrAlreadyClaimed }
