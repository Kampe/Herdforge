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
// returning "this is the one true transition" boolean: a lease entering
// Released or Expired only records that its lease lifecycle ended.
// Whether its capacity token has been durably given back is tracked
// separately as a small claim/ack protocol (ClaimCapacityRelease /
// AckCapacityRelease / FailCapacityRelease) so that:
//
//   - Two settlers (two processes, two goroutines, whatever) can never
//     both be attempting the external CapacityCoordinator.Release call for
//     the same lease at the same time -- ClaimCapacityRelease's atomic
//     claim is the mutual-exclusion boundary, not a Go-level mutex or an
//     assumption that the coordinator itself dedupes concurrent calls.
//   - A crash between the external call succeeding and the local Ack
//     leaves the claim stale (its claimed_at ages past staleAfter) rather
//     than lost, so a later settler reclaims and retries it -- using the
//     same stable idempotency key bound to the lease's ID/generation, so
//     a conforming CapacityCoordinator can dedupe that at-least-once
//     redelivery into an effectively-exactly-once external effect.
//
// See ClaimManager.settlePendingCapacity and CapacityCoordinator's doc
// comment for the full protocol.
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

	// ClaimCapacityRelease atomically claims a batch of leases whose
	// capacity token still needs releasing -- either never attempted
	// (pending) or a prior attempt looks crashed (its claim's
	// claimed_at is older than staleAfter) -- for settlerID. If key is
	// non-nil, only that key's leases are eligible; nil claims across
	// every key. Implementations must make this a single atomic
	// operation (e.g. one UPDATE...RETURNING under the store's
	// single-writer lock) so that under concurrent callers, each
	// eligible lease is claimed by at most one of them.
	ClaimCapacityRelease(ctx context.Context, settlerID string, staleAfter time.Duration, now time.Time, key *LeaseKey) ([]*Lease, error)

	// AckCapacityRelease durably marks leaseID's capacity token
	// returned, completing settlerID's claim. A no-op (not an error) if
	// settlerID no longer holds the claim -- e.g. it went stale and a
	// different settler reclaimed leaseID in the meantime, in which case
	// that settler is responsible for acking it now.
	AckCapacityRelease(ctx context.Context, leaseID int64, settlerID string, now time.Time) error

	// FailCapacityRelease releases settlerID's claim on leaseID
	// immediately back to pending (instead of waiting out staleAfter),
	// for a synchronous, non-crash failure the settler itself observed.
	// A no-op if settlerID no longer holds the claim.
	FailCapacityRelease(ctx context.Context, leaseID int64, settlerID string) error

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
