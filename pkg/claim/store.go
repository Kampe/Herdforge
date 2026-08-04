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
	// ErrProviderTransitionInProgress means a Release or a reclaiming
	// Acquire was blocked by an active (non-stale) provider-transition
	// lock (see AcquireProviderLock/ReleaseProviderLock) -- the lease is
	// still genuinely current, just busy with an in-flight provider
	// mutation. Distinct from ErrStaleGeneration/ErrAlreadyClaimed, which
	// mean the lease has actually moved on.
	ErrProviderTransitionInProgress = errors.New("claim: provider transition in progress for this lease")
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
	// it eligible for PendingCapacityRelease — see above) UNLESS it is
	// held by an active, non-stale provider-transition lock (see
	// AcquireProviderLock), in which case reclaim is blocked until the
	// lock clears or goes stale, and this returns a *ClaimConflictError
	// same as an unexpired lease would. Generation is monotonically
	// increasing per key. On conflict with a live lease, returns a
	// *ClaimConflictError wrapping ErrAlreadyClaimed.
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
	// PendingCapacityRelease/Lease.CapacityReleasedAt for that. Blocked
	// (returns ErrProviderTransitionInProgress) while an active, non-stale
	// provider-transition lock is held on the lease -- see
	// AcquireProviderLock.
	Release(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time) (lease *Lease, transitioned bool, err error)

	// AcquireProviderLock atomically verifies that (key, ownerID,
	// generation) is still the current active lease AND locks it against
	// Release/reclaim in the SAME statement -- the verification and the
	// lock acquisition are not two separate steps a concurrent
	// Release/Acquire could interleave between, which is what makes this
	// different from (and safe where) a second plain read-only check is
	// not: checking here IS locking. Fails with ErrLeaseNotCurrent-class
	// errors (via the same fencing/not-found errors Release/Renew/Hold
	// use) if the lease has moved on, or with
	// ErrProviderTransitionInProgress if it's still current but another
	// live (non-stale) lock already holds it. A stale lock (older than
	// staleAfter, e.g. its holder crashed) is preempted rather than
	// blocking forever.
	AcquireProviderLock(ctx context.Context, key LeaseKey, ownerID string, generation int64, lockOwner string, staleAfter time.Duration, now time.Time) (*Lease, error)

	// ReleaseProviderLock clears lockOwner's provider-transition lock,
	// allowing Release/reclaim to proceed again. Idempotent: a no-op if
	// lockOwner does not currently hold the lock (e.g. it already went
	// stale and was preempted).
	ReleaseProviderLock(ctx context.Context, key LeaseKey, generation int64, lockOwner string) error

	// PeekStaleProviderLock returns the current active lease for key if
	// it is held by a provider-transition lock that is active but stale.
	// Returns nil (not an error) if there is no active lease, it has no
	// provider lock, or its lock is not yet stale. Release, Acquire, and
	// ExpireStale never preempt a provider lock by time alone -- they
	// unconditionally require provider_lock_owner = '' -- specifically so
	// that whether a stale lock is safe to clear can only be decided by
	// ClaimManager (which alone knows if a ProviderCAS is configured, and
	// so whether a durable fence-advance is needed first). See
	// PeekAllStaleProviderLocks (ExpireStale's sweep-scale counterpart)
	// and ForceReleaseProviderLock.
	PeekStaleProviderLock(ctx context.Context, key LeaseKey, now time.Time) (*Lease, error)

	// PeekAllStaleProviderLocks is PeekStaleProviderLock across every
	// key, for ClaimManager.ExpireStale's global sweep to durably
	// preempt (fence-advance, then ForceReleaseProviderLock) every
	// stale-locked lease before calling the store's own ExpireStale.
	PeekAllStaleProviderLocks(ctx context.Context, now time.Time) ([]*Lease, error)

	// ForceReleaseProviderLock clears key/generation's provider lock
	// unconditionally, without requiring the caller to be its current
	// owner. Reserved for ClaimManager's orchestration layer and MUST
	// only be called immediately after a durably-confirmed provider fence
	// advance for the lease's next generation -- never as a substitute
	// for that confirmation, which is what actually makes the preemption
	// safe.
	ForceReleaseProviderLock(ctx context.Context, key LeaseKey, generation int64) error

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

type AtomicLeaseStore interface {
	AcquireWithIdentity(context.Context, LeaseKey, string, string, string, string, string, string, time.Time, time.Duration) (*Lease, error)
}

type RecoveryStore interface {
	SnapshotExpiredLeases(context.Context, time.Time) ([]*Lease, error)
	ExpireLeaseCAS(context.Context, int64, int64, time.Time) (*Lease, bool, error)
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
