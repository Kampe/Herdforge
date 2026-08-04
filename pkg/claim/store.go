package claim

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	ErrProviderLockStale            = errors.New("claim: provider lock missing or stale")
	ErrCapacityReleaseStale         = errors.New("claim: capacity release claim missing or stale")
	ErrLegacyLeaseHoldDisabled      = errors.New("claim: legacy lease-held authority is disabled; use lifecycle hold authority")
	ErrLegacyClaimDisabled          = errors.New("claim: legacy ClaimTask is disabled; use exact canonical ClaimRequest")
)

const (
	providerLockKindOrdinary = ""
	providerLockKindRecovery = "recovery"
)

func recoveryOwnerFor(leaseID, generation int64) string {
	return fmt.Sprintf("herd-provider-recovery:%d:%d", leaseID, generation)
}

func parseRecoveryOwner(owner string) (leaseID, generation int64, reserved bool, err error) {
	const prefix = "herd-provider-recovery:"
	if !strings.HasPrefix(owner, prefix) {
		return 0, 0, false, nil
	}
	parts := strings.Split(strings.TrimPrefix(owner, prefix), ":")
	if len(parts) != 2 {
		return 0, 0, true, fmt.Errorf("malformed reserved recovery owner")
	}
	leaseID, idErr := strconv.ParseInt(parts[0], 10, 64)
	generation, genErr := strconv.ParseInt(parts[1], 10, 64)
	if idErr != nil || genErr != nil || leaseID <= 0 || generation <= 0 {
		return 0, 0, true, fmt.Errorf("malformed reserved recovery owner")
	}
	return leaseID, generation, true, nil
}

func isReservedRecoveryOwner(owner string) bool {
	_, _, reserved, _ := parseRecoveryOwner(owner)
	return reserved
}

func isRecoveryOwnerFor(owner string, leaseID, generation int64) bool {
	id, gen, reserved, err := parseRecoveryOwner(owner)
	return reserved && err == nil && id == leaseID && gen == generation
}

func normalizeProviderLockTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
func providerLockTimeText(t time.Time) string {
	return normalizeProviderLockTime(t).Format(time.RFC3339Nano)
}

// LeaseStore is the narrow durable-persistence port ClaimManager depends
// on. All methods must be safe to call from multiple OS processes against
// the same backing store concurrently; exactly one caller may win a
// contended Acquire.
//
// Capacity accounting is NOT tied to a single Release/recovery call
// returning "this is the one true transition" boolean: a lease entering
// Released or Expired only records that its lease lifecycle ended.
// Whether its capacity token has been durably given back is tracked
// separately as a small exact-ID claim/ack protocol
// (ClaimCapacityReleaseExact / AckCapacityRelease / FailCapacityRelease)
// so that:
//
//   - Two settlers (two processes, two goroutines, whatever) can never
//     both be attempting the external CapacityCoordinator.Release call for
//     the same lease at the same time -- the exact-ID atomic claim is the
//     mutual-exclusion boundary, not a Go-level mutex or an
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
	// passed (but which no other Acquire/recovery transition has evicted yet)
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
	// lock already holds it. This method never preempts a lock by time;
	// staleAfter is retained only for compatibility. Lifecycle recovery
	// alone observes its fixed threshold, claims a typed recovery record,
	// advances the provider fence, and finalizes it.
	AcquireProviderLock(ctx context.Context, key LeaseKey, ownerID string, generation int64, lockOwner string, staleAfter time.Duration, now time.Time) (*Lease, error)

	// ReleaseProviderLock clears exactly lockOwner's ordinary provider
	// transition lock. A mismatched or absent owner is an error; it is not
	// an idempotent no-op and it never clears recovery state.
	ReleaseProviderLock(ctx context.Context, key LeaseKey, generation int64, lockOwner string) error

	// PeekStaleProviderLock returns an immutable observation for lifecycle
	// recovery. It never mutates a provider lock.
	PeekStaleProviderLock(ctx context.Context, key LeaseKey, now time.Time) (*Lease, error)

	// PeekAllStaleProviderLocks is the read-only sweep counterpart used to
	// discover candidates for lifecycle recovery.
	PeekAllStaleProviderLocks(ctx context.Context, now time.Time) ([]*Lease, error)

	// ActiveClaims returns only live (Active, unexpired) leases.
	ActiveClaims(ctx context.Context, now time.Time) ([]*Lease, error)

	// AckCapacityRelease durably marks leaseID's capacity token
	// returned, completing settlerID's claim. A foreign, stale, or zero-row
	// ack is an error and preserves the durable evidence.
	AckCapacityRelease(ctx context.Context, leaseID int64, settlerID string, now time.Time) error

	// FailCapacityRelease releases settlerID's claim on leaseID
	// immediately back to pending (instead of waiting out staleAfter),
	// for a synchronous, non-crash failure the settler itself observed.
	// A foreign or stale claim is an error.
	FailCapacityRelease(ctx context.Context, leaseID int64, settlerID string) error

	Close() error
}

type AtomicLeaseStore interface {
	AcquireWithIdentity(context.Context, LeaseKey, string, string, string, string, string, string, time.Time, time.Duration) (*Lease, error)
}

type RecoveryStore interface {
	SnapshotExpiredLeases(context.Context, time.Time) ([]*Lease, error)
	ExpireLeaseCAS(context.Context, int64, int64, time.Time) (*Lease, bool, error)
	ObserveStaleProviderLock(context.Context, LeaseKey, time.Time) (*ProviderLockObservation, error)
	ClaimProviderLockCAS(context.Context, ProviderLockObservation) (bool, error)
	FinalizeProviderLockCAS(context.Context, ProviderLockObservation) (bool, error)
}

type ExactCapacityReleaseStore interface {
	ClaimCapacityReleaseExact(context.Context, int64, int64, string, time.Duration, time.Time) (*Lease, bool, error)
}

type UnreservedAbortStore interface {
	AbortUnreservedLease(context.Context, *Lease, time.Time) (*Lease, bool, error)
}

type PendingCapacityStore interface {
	PendingCapacityReleases(context.Context) ([]*Lease, error)
}

// LeaseSnapshotStore reads the durable historical row without applying
// active/unexpired filtering. Mutating callers use it before entering the
// authority fence and re-read/CAS inside that fence.
type LeaseSnapshotStore interface {
	CurrentLease(context.Context, LeaseKey) (*Lease, error)
	LeaseByGeneration(context.Context, LeaseKey, string, int64) (*Lease, error)
}

type ProviderLockObservation struct {
	LeaseID       int64
	Generation    int64
	Owner         string
	LockedAt      time.Time
	ObservedAt    time.Time
	RecoveryOwner string
	Recovery      bool
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
