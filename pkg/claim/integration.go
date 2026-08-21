package claim

import (
	"context"
	"errors"
)

// ProviderRevision is an opaque, provider-supplied compare-and-swap token
// (e.g. Kaneo's updatedAt, GitHub's ETag/SHA, Jira's version). pkg/claim
// does not interpret this string; it is plumbing for whoever mutates the
// provider board on the strength of a lease.
type ProviderRevision string

// ErrProviderFenceRejected is the sentinel a ProviderCAS implementation
// should wrap when it rejects a CompareAndSwap call because fenceToken is
// stale (lower than the highest fence token it has already accepted or
// been advanced to for that taskID) -- distinct from
// ErrProviderRevisionStale, which is about the task's own business-state
// revision, not lease ownership.
var ErrProviderFenceRejected = errors.New("claim: provider rejected a stale fencing token")

// ProviderCAS is the narrow hook a lifecycle/outbox integration supplies
// to make provider board mutations revision-checked (optimistic
// concurrency) instead of a blind read-then-write that can clobber a
// change made since the task was last read. pkg/claim deliberately does
// not call a provider itself (see LeaseStore's boundary comment on why
// pkg/provider was decoupled from this package) — this interface exists
// so FAC-119's lifecycle service and pkg/claim agree on the contract
// shape ahead of that wiring landing.
//
// # Why a fencing token, not just a local lock
//
// pkg/claim's AcquireProviderLock/ReleaseProviderLock give a lease
// exclusive local rights to attempt a provider mutation, but a LOCAL
// lock -- even one with a staleness timeout for crash recovery -- can
// never prove an in-flight external CompareAndSwap call has actually
// stopped. A settler that appears crashed (its lock timed out) may still
// have a request in flight to the provider, arbitrarily delayed by the
// network or a GC pause, that lands after a new generation has already
// reclaimed the lease. A local timeout is a liveness bound (the LEASE
// becomes reclaimable again), never a safety proof (the OLD call is
// done) -- those are different properties and conflating them is exactly
// the classic distributed-lock-with-timeout hazard (see e.g. Kleppmann's
// "How to do distributed locking").
//
// The only sound fix is fencing at the resource itself: fenceToken must
// be a value that only increases across the lifetime of a taskID (pkg/
// claim always passes the lease generation). Implementations MUST
// durably record the highest fenceToken ever accepted or advanced to
// for taskID, and MUST reject -- without calling mutate, without
// applying any effect -- any CompareAndSwap whose fenceToken is lower
// than that recorded value, wrapping ErrProviderFenceRejected. This is
// what makes a stale call from a superseded generation harmless no
// matter how late it arrives: rejection happens at the provider, not by
// pkg/claim guessing whether enough time has passed.
//
// FAC-120 does not implement a real ProviderCAS against Kaneo/GitHub/etc
// -- that's still FAC-119's to build -- but pkg/claim's own test double
// (fakeProviderCAS) implements this contract for real and is what the
// package's fencing-safety tests exercise.
type ProviderCAS interface {
	// CompareAndSwap applies mutate only if BOTH the task's current
	// revision equals expected AND fenceToken is not stale (see above).
	// opID is a stable idempotency key for this logical mutation (must
	// include generation and operation kind/payload identity). A prior
	// successful apply of the same opID MUST return success without
	// re-invoking mutate (crash-safe retry). Returns the task's new
	// revision on success.
	CompareAndSwap(ctx context.Context, taskID string, expected ProviderRevision, fenceToken int64, opID string, mutate func(ctx context.Context) error) (ProviderRevision, error)

	// AdvanceFence durably records fenceToken as the new minimum accepted
	// fence for taskID if fenceToken is greater than what's currently
	// recorded; a no-op (not an error) if it's not greater. Applies no
	// business-state mutation. pkg/claim calls this whenever a lease
	// reclaims to a new generation (see ClaimManager.Claim), specifically
	// so that a superseded generation's CompareAndSwap is rejected even
	// if the NEW generation never itself calls CompareAndSwap -- without
	// this, a fence check alone cannot protect a reclaim that the new
	// owner never engages the provider for.
	AdvanceFence(ctx context.Context, taskID string, fenceToken int64) error
}

// ErrProviderAmbiguous means the provider mutation may have succeeded but
// local confirmation (post-mutate revision read / applied mark) failed.
// Callers must reconcile by opID / readback before retrying blindly.
var ErrProviderAmbiguous = errors.New("claim: provider mutation outcome is ambiguous; reconcile before retry")

// ErrFenceInfrastructure is a PRE-CONDITION failure: the mutation was refused
// before any remote call, so nothing changed and no reconciliation is needed.
//
// FAC-571: the missing-fence-broker refusal used ErrProviderAmbiguous, which
// tells an operator the write MAY have landed and must be reconciled before any
// retry. It had not landed -- the check runs before any remote or readback side
// effect -- so a correct operator reading that message stopped and escalated
// instead of starting the broker and retrying. Misclassifying a clean refusal as
// ambiguous is expensive precisely because the ambiguity contract is honored.
var ErrFenceInfrastructure = errors.New("claim: refused before any provider call; required fence infrastructure is not running")

// OutboxIntent is one durable, idempotent side effect a lease transition
// wants applied to an external system (provider board update, git push,
// chat notification, etc). IdempotencyKey must be stable for retries of
// the same logical intent (e.g. "claim:<key>:g<generation>") so a
// transactional outbox can dedupe redelivery.
type OutboxIntent struct {
	IdempotencyKey string
	Kind           string
	Payload        []byte
}

// OutboxRecorder is the narrow hook ClaimManager calls to enroll an
// OutboxIntent alongside a lease transition, so "lease committed" and
// "side effect will eventually be applied" become one recorded fact
// instead of two independently-fallible ones. No package implements this
// yet — a no-op recorder is used when none is configured, matching
// CapacityCoordinator's boundary: lease durability does not currently
// depend on outbox delivery, and true same-transaction atomicity with the
// lease write is FAC-119's transactional outbox to provide (this hook
// runs as a best-effort step alongside the lease commit, not inside it).
type OutboxRecorder interface {
	Record(ctx context.Context, intent OutboxIntent) error
}

type noopOutbox struct{}

func (noopOutbox) Record(context.Context, OutboxIntent) error { return nil }

// Reconciler is the narrow interface for a periodic safety sweep that
// reconciles leases against outbox/provider state after a crash or
// partial transition (FAC-119's reconciliation sweep, per the FAC-119
// card's "Reconcile incomplete transitions on startup and on a periodic
// safety sweep" acceptance criterion). ClaimManager satisfies this using
// the primitives it already exposes (ExpireStale, SettlePendingCapacity)
// — see ClaimManager.Reconcile — but a full reconciliation sweep that
// also cross-checks outbox/provider state is FAC-119's to build; this
// interface only fixes the shape a scheduler can drive.
type Reconciler interface {
	Reconcile(ctx context.Context) error
}
