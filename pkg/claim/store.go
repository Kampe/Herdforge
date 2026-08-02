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
	// ErrLeaseExpired means the caller's generation is still the fenced
	// owner of record, but the lease's TTL has already passed; Renew must
	// reject this rather than silently extending an already-dead lease.
	ErrLeaseExpired = errors.New("claim: lease already expired")
)

// LeaseStore is the narrow durable-persistence port ClaimManager depends
// on. All methods must be safe to call from multiple OS processes against
// the same backing store concurrently; exactly one caller may win a
// contended Acquire.
//
// Capacity accounting is NOT tied to a single Release/ExpireStale call
// returning "this is the one true transition" boolean anymore: a lease
// entering Released or Expired only records that its lease lifecycle
// ended. Whether its capacity token has been durably given back is a
// separate fact (Lease.CapacityReleasedAt), settled via
// PendingCapacityRelease/MarkCapacityReleased so a crash or coordinator
// failure between "lease ended" and "capacity returned" leaves a durable,
// retryable marker instead of silently losing the token or double-freeing
// it. See ClaimManager.settlePendingCapacity.
//
// FAC-120/FAC-119 boundary: this package ships SQLiteLeaseStore, a
// self-contained implementation, so cross-process leases work standalone
// today. FAC-119's durable lifecycle/event-log/outbox service is the
// intended long-term home for lease persistence, so lease transitions
// share one transactional outbox with other lifecycle events instead of
// committing to a private database. Integrating that means supplying a
// different LeaseStore implementation to NewClaimManager (or having the
// lifecycle service wrap SQLiteLeaseStore) — ClaimManager's contract with
// LeaseStore does not need to change for that swap. See also
// ProviderCAS/OutboxRecorder/Reconciler in integration.go for the other
// narrow interfaces that boundary needs.
type LeaseStore interface {
	// Acquire atomically creates a new active lease for key if none is
	// currently active and unexpired. An active-but-expired lease is
	// transitioned to Expired as part of winning the acquire (which makes
	// it eligible for PendingCapacityRelease — see above). Generation is
	// monotonically increasing per key. On conflict with a live lease,
	// returns a *ClaimConflictError wrapping ErrAlreadyClaimed.
	Acquire(ctx context.Context, key LeaseKey, ownerID, role, worktreePath string, now time.Time, ttl time.Duration) (*Lease, error)

	// Renew extends an active lease's expiry, fenced by generation. A
	// generation that no longer matches the active lease returns
	// ErrStaleGeneration; a matching generation whose TTL has already
	// passed (but which no other Acquire/ExpireStale has evicted yet)
	// returns ErrLeaseExpired rather than silently extending it; a lease
	// gone entirely returns ErrNotFound.
	Renew(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time, ttl time.Duration) (*Lease, error)

	// Release marks the fenced lease released. Idempotent: releasing an
	// already-released lease at the same generation returns the lease
	// with transitioned=false and a nil error — callers must not treat
	// transitioned=false as "capacity was already handled"; consult
	// PendingCapacityRelease/Lease.CapacityReleasedAt for that.
	Release(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time) (lease *Lease, transitioned bool, err error)

	// Hold sets or clears the operator-hold flag on the active lease for
	// key, fenced by ownerID/generation exactly like Renew/Release: a
	// caller without the current owner+generation is rejected rather than
	// being able to hold/unhold a lease it does not own.
	Hold(ctx context.Context, key LeaseKey, ownerID string, generation int64, held bool, now time.Time) (*Lease, error)

	// ExpireStale transitions active-but-expired (and not held) leases to
	// Expired and returns exactly the ones this call transitioned. The
	// held/expiry check must be re-evaluated at the moment of the actual
	// row transition (not just an earlier candidate SELECT), so a Renew
	// or Hold that lands between candidate selection and transition wins
	// the race instead of being silently overridden.
	ExpireStale(ctx context.Context, now time.Time) ([]*Lease, error)

	// ActiveClaims returns only live (Active, unexpired) leases.
	ActiveClaims(ctx context.Context, now time.Time) ([]*Lease, error)

	// PendingCapacityRelease returns every Released/Expired lease whose
	// capacity token has not yet been durably marked returned
	// (CapacityReleasedAt == nil), across all keys. Callers retry
	// CapacityCoordinator.Release for each and then call
	// MarkCapacityReleased on success; a coordinator failure must leave
	// the row untouched so the next call retries it.
	PendingCapacityRelease(ctx context.Context) ([]*Lease, error)

	// MarkCapacityReleased durably records that leaseID's capacity token
	// has been returned. Idempotent: marking an already-marked lease is a
	// no-op, not an error.
	MarkCapacityReleased(ctx context.Context, leaseID int64, now time.Time) error

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
