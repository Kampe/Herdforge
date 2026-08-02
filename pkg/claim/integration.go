package claim

import "context"

// ProviderRevision is an opaque, provider-supplied compare-and-swap token
// (e.g. Kaneo's updatedAt, GitHub's ETag/SHA, Jira's version). pkg/claim
// does not interpret this string; it is plumbing for whoever mutates the
// provider board on the strength of a lease.
type ProviderRevision string

// ProviderCAS is the narrow hook a lifecycle/outbox integration supplies
// to make provider board mutations revision-checked (optimistic
// concurrency) instead of a blind read-then-write that can clobber a
// change made since the task was last read. pkg/claim deliberately does
// not call a provider itself (see LeaseStore's boundary comment on why
// pkg/provider was decoupled from this package) — this interface exists
// so FAC-119's lifecycle service and pkg/claim agree on the contract
// shape ahead of that wiring landing, not because anything here invokes
// it yet.
//
// FAC-120 does not implement or call ProviderCAS: defining the interface
// without wiring it is a deliberate, incomplete step toward FAC-119
// integration, not a claim that provider CAS is handled.
type ProviderCAS interface {
	// CompareAndSwap applies mutate only if the task's current revision
	// still equals expected; returns the task's new revision on success.
	// Implementations should make a revision mismatch distinguishable
	// (e.g. a sentinel error) so callers can re-read and retry instead of
	// silently overwriting a concurrent change.
	CompareAndSwap(ctx context.Context, taskID string, expected ProviderRevision, mutate func(ctx context.Context) error) (ProviderRevision, error)
}

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
