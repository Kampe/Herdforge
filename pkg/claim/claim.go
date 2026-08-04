// Package claim provides durable, fenced, cross-process leases over
// provider tasks: at most one owner can hold an active lease per
// (repo, provider, project, task_ref) at a time, even across restarts and
// concurrent Herdforge processes on the same box.
//
// # Verified scope (FAC-120) vs. residual (FAC-147)
//
// Everything in this package -- Acquire/Renew/Release/Hold/ExpireStale,
// the capacity-release delivery protocol, and the provider-transition
// safety design (ProviderCAS's fencing-token contract, AdvanceFence,
// DurableOutbox, BeginProviderTransition/CompleteProviderTransition/
// ReconcileProviderTransitions, and the store-level provider-lock
// exclusion that makes fence-advance a durable prerequisite for reclaim)
// -- is real, adversarially tested against deterministic race and
// crash-recovery scenarios, and safe to depend on as an internal
// package. fakeProviderCAS (test-only) proves the ProviderCAS contract
// is sound and enforceable end to end.
//
// What this package does NOT provide, and what no code outside pkg/claim
// currently calls WithProviderCAS/AdvanceFence/BeginProviderTransition/
// CompleteProviderTransition to obtain: a concrete ProviderCAS
// implementation against a real provider (Kaneo, GitHub, etc.), and its
// wiring into cmd/herd's production mutation paths (dispatch, review,
// approve, board-sync). Those paths call pkg/provider's TaskProvider
// interface directly today, with no revision-based CAS and no
// generation-fencing-token enforcement -- a real Kaneo card cannot yet
// be protected by anything this package builds. That is deliberately
// out of scope for FAC-120 (whose worktree was restricted from cmd/herd
// and pkg/lifecycle to avoid colliding with FAC-119, the durable
// lifecycle/event-log/outbox epic this is designed as the claim
// component of) and is tracked as FAC-147, blocked on both FAC-119 (the
// intended home for the mutation call sites) and this package (which
// supplies the contract FAC-147 implements against).
package claim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// ErrUnlabeledTask is returned when a task carries no matching role label;
// unlabeled tasks are never eligible to claim.
var ErrUnlabeledTask = errors.New("claim: task has no matching role label")

// ErrRoleMismatch is returned when the claiming worker's role does not
// exactly match the task's role label.
var ErrRoleMismatch = errors.New("claim: worker role does not match task role")

// CapacityCoordinator is the narrow hook ClaimManager calls when a lease is
// acquired or released, so a role's capacity pool stays in lockstep with
// its leases.
//
// idempotencyKey is stable per (lease ID, generation, intent) — see
// capacityKey — and is the same on every retry of the same logical
// operation, including a retry after a crash. pkg/claim's own delivery
// protocol (LeaseStore.ClaimCapacityRelease/AckCapacityRelease/
// FailCapacityRelease) already guarantees at most one live settler is
// ever calling Release for a given lease at a time — concurrent
// double-delivery is a store-level CAS claim, not something
// CapacityCoordinator needs to guard against itself. What
// CapacityCoordinator alone can guard against is redelivery across a
// crash: if a settler calls Release, the coordinator durably applies it,
// and the settler crashes before it can Ack, pkg/claim's claim goes
// stale and a later settler calls Release again with the *same*
// idempotencyKey. Implementations should keep an applied-keys ledger (or
// equivalent) and treat a repeat of the same key as a no-op, so that
// at-least-once, crash-safe delivery from pkg/claim becomes an
// effectively-exactly-once external effect. A coordinator that ignores
// idempotencyKey is only safe if Release/Reserve are already naturally
// idempotent operations (e.g. absolute pool-membership set/unset rather
// than an increment/decrement counter).
//
// FAC-120/FAC-119 boundary: no package in this repo implements capacity
// pools yet. Until FAC-119's durable lifecycle/outbox service exists to
// enroll Reserve/Release in the same transaction as the lease write, this
// hook runs as a best-effort step immediately after the durable lease
// commit (with a compensating Release on Reserve failure), not inside it.
// A no-op coordinator is used when none is supplied.
type CapacityCoordinator interface {
	Reserve(ctx context.Context, role string, idempotencyKey string) error
	Release(ctx context.Context, role string, idempotencyKey string) error
}

type noopCapacity struct{}

func (noopCapacity) Reserve(context.Context, string, string) error { return nil }
func (noopCapacity) Release(context.Context, string, string) error { return nil }

// capacityKey derives the stable idempotency key CapacityCoordinator sees
// for a given lease and intent ("reserve" or "release"). Bound to the
// lease's durable ID and its generation, per the review requirement, so
// it is identical across any number of retries/redeliveries of the same
// logical operation and distinct across a reclaim (new generation).
func capacityKey(intent string, l *Lease) string {
	return fmt.Sprintf("%s:%d:g%d", intent, l.ID, l.Generation)
}

// ClaimManager enforces role-matching and drives a LeaseStore to produce
// atomic cross-process leases with renewal, expiry, operator hold, and
// durable, retryable capacity settlement.
type ClaimManager struct {
	store       LeaseStore
	capacity    CapacityCoordinator
	outbox      OutboxRecorder
	provider    ProviderCAS
	outboxStore DurableOutbox
	now         func() time.Time
	ttl         time.Duration

	// settlerID identifies this ClaimManager instance as a capacity-
	// release settler for LeaseStore.ClaimCapacityRelease's claim
	// protocol. Unique per instance by default (see NewClaimManager);
	// override with WithSettlerID for deterministic test identities.
	settlerID string
	// capacityClaimTimeout bounds how long a ClaimCapacityRelease claim
	// is honored before another settler may reclaim it, i.e. how long a
	// crashed settler's in-flight release can block recovery.
	capacityClaimTimeout time.Duration
	// providerLockTimeout bounds how long CompleteProviderTransition's
	// AcquireProviderLock is honored before a different settler may
	// preempt it (crash recovery for a provider call that never returned).
	providerLockTimeout time.Duration
	holdReader          lifecycle.HoldReader
}

// Option configures a ClaimManager.
type Option func(*ClaimManager)

// WithClock overrides the time source (for tests).
func WithClock(now func() time.Time) Option { return func(m *ClaimManager) { m.now = now } }

// WithTTL sets the lease duration granted by Claim/Renew. Default 10m.
func WithTTL(ttl time.Duration) Option { return func(m *ClaimManager) { m.ttl = ttl } }

// WithCapacityCoordinator wires a role-capacity pool into claim/release.
func WithCapacityCoordinator(c CapacityCoordinator) Option {
	return func(m *ClaimManager) { m.capacity = c }
}

// WithOutboxRecorder wires a transactional-outbox recorder into
// claim/release intents; see integration.go. A no-op recorder is used
// when none is supplied — no package in this repo implements one yet.
func WithOutboxRecorder(o OutboxRecorder) Option {
	return func(m *ClaimManager) { m.outbox = o }
}

// WithSettlerID overrides the auto-generated settler identity this
// ClaimManager uses to claim capacity-release work. Tests that want two
// distinct, deterministic settlers (e.g. simulating two processes) should
// set this explicitly; production callers normally don't need to.
func WithSettlerID(id string) Option { return func(m *ClaimManager) { m.settlerID = id } }

// WithCapacityClaimTimeout overrides how long a capacity-release claim is
// honored before another settler may reclaim it (crash recovery bound).
// Default 5m.
func WithCapacityClaimTimeout(d time.Duration) Option {
	return func(m *ClaimManager) { m.capacityClaimTimeout = d }
}

// WithProviderLockTimeout overrides how long CompleteProviderTransition's
// provider-transition lock is honored before a different settler may
// preempt it (crash recovery bound for a provider call that never
// returned). Default 5m, matching providerLockStaleAfter (the fixed
// window Release/Acquire's reclaim path use to honor -- or stop honoring
// -- someone else's lock).
func WithProviderLockTimeout(d time.Duration) Option {
	return func(m *ClaimManager) { m.providerLockTimeout = d }
}

// WithHoldReader injects the canonical durable hold authority.
func WithHoldReader(r lifecycle.HoldReader) Option { return func(m *ClaimManager) { m.holdReader = r } }

// NewClaimManager builds a ClaimManager over store. store's lifetime is
// owned by the caller (Close it when the manager is no longer needed).
func NewClaimManager(store LeaseStore, opts ...Option) *ClaimManager {
	m := &ClaimManager{
		store: store, capacity: noopCapacity{}, outbox: noopOutbox{}, now: time.Now, ttl: 10 * time.Minute,
		capacityClaimTimeout: 5 * time.Minute, providerLockTimeout: 5 * time.Minute,
	}
	m.settlerID = fmt.Sprintf("pid%d-%p-%d", os.Getpid(), m, time.Now().UnixNano())
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// ClaimRequest describes an attempt to lease a task.
type ClaimRequest struct {
	Key          LeaseKey
	OwnerID      string
	Role         string // the claiming worker's role
	TaskRole     string // the role label read off the task by the caller; "" means unlabeled
	WorktreePath string
	// HoldIdentity is resolved before OwnerID is generated. OwnerID is a
	// lease provenance token and must never be used as hold authority identity.
	HoldIdentity   lifecycle.HoldIdentity
	HoldIdentities []lifecycle.HoldIdentity
}

func exactClaimComposite(req ClaimRequest) ([]lifecycle.HoldIdentity, error) {
	ids := req.HoldIdentities
	if len(ids) != 2 {
		return nil, fmt.Errorf("claim: exact lane/task hold identity composite is required")
	}
	var lane, task *lifecycle.HoldIdentity
	for i := range ids {
		id := ids[i]
		if !identityValid(id) {
			return nil, fmt.Errorf("claim: invalid hold identity composite")
		}
		if id.Scope == "lane" && id.Task == "" {
			if lane != nil {
				return nil, fmt.Errorf("claim: duplicate lane hold identity")
			}
			copy := id
			lane = &copy
		} else if id.Scope == "task" && id.Task != "" {
			if task != nil {
				return nil, fmt.Errorf("claim: duplicate task hold identity")
			}
			copy := id
			task = &copy
		} else {
			return nil, fmt.Errorf("claim: hold identity has invalid scope")
		}
	}
	if lane == nil || task == nil || lane.Repository != task.Repository || lane.Owner != task.Owner || lane.Lane != task.Lane || task.Task != req.Key.TaskRef {
		return nil, fmt.Errorf("claim: lane/task hold identities do not form the exact task composite")
	}
	return []lifecycle.HoldIdentity{*lane, *task}, nil
}

// Claim attempts to atomically acquire a lease. Role enforcement is exact:
// an unlabeled task (TaskRole == "") is never eligible, and a mismatched
// role is rejected even if some lease is otherwise free.
//
// If Acquire silently evicted a stale, expired lease to grant this claim,
// that evicted lease's capacity token becomes durably pending the instant
// Acquire's UPDATE flips it to Expired (see LeaseStore's doc comment) —
// Claim settles any capacity pending for this key before reserving new
// capacity, so an old, dead owner's token is durably released before (or,
// if settlement itself fails, at least queued durably ahead of) the new
// reservation, instead of the new claim silently reserving on top of an
// unreturned token. Settlement failure does not block the new claim
// (liveness: a stuck coordinator must not prevent reclaiming stale work
// forever) — the pending row remains durable and retryable via
// SettlePendingCapacity/ExpireStale/a future Release call.
func (m *ClaimManager) Claim(ctx context.Context, req ClaimRequest) (*Lease, error) {
	if req.TaskRole == "" {
		return nil, ErrUnlabeledTask
	}
	if req.Role == "" || req.Role != req.TaskRole {
		return nil, ErrRoleMismatch
	}
	if m.holdReader != nil {
		identities, err := exactClaimComposite(req)
		if err != nil {
			return nil, err
		}
		for _, identity := range identities {
			if !identityValid(identity) {
				return nil, fmt.Errorf("claim: ambiguous canonical hold identity")
			}
			source, ok := m.holdReader.(interface {
				CurrentGeneration(context.Context, lifecycle.HoldIdentity) (int64, error)
			})
			if !ok {
				return nil, fmt.Errorf("claim: hold generation source is required")
			}
			generation, err := source.CurrentGeneration(ctx, identity)
			if err != nil || generation <= 0 {
				if err != nil {
					return nil, fmt.Errorf("claim: hold generation: %w", err)
				}
				return nil, fmt.Errorf("claim: invalid hold generation %d", generation)
			}
			decision, err := m.holdReader.Check(ctx, identity, generation)
			if err != nil {
				return nil, fmt.Errorf("claim: hold authority: %w", err)
			}
			if decision.Held {
				return nil, fmt.Errorf("claim: held identity denied: %s (%s)", decision.Reason, decision.Code)
			}
		}
	}

	// If the current lease for this key is being kept alive only by a
	// now-stale provider-transition lock (its holder looks crashed, but a
	// call it made to the provider may still be in flight -- see
	// AcquireProviderLock/ProviderCAS's doc comments), superseding it
	// must not be allowed to proceed until the provider has durably been
	// told about the new generation. A best-effort, error-swallowed
	// AdvanceFence AFTER a local reclaim already happened is exactly the
	// gap an independent review caught: local ownership moved to
	// generation 2 while the provider never heard about it, so
	// generation 1's eventually-resumed call still succeeded. Store-level
	// Acquire/Release/ExpireStale no longer preempt a provider lock by
	// time alone at all -- ONLY preemptStaleProviderLock (here) and
	// preemptAllStaleProviderLocks (ExpireStale) may do it, and only
	// after a durably-confirmed fence advance. Leases that were never
	// provider-locked pay zero cost (PeekStaleProviderLock returns nil
	// immediately) and reclaim exactly as before.
	if err := m.preemptStaleProviderLock(ctx, req.Key); err != nil {
		return nil, err
	}

	var lease *Lease
	var err error
	if m.holdReader != nil {
		identities, compositeErr := exactClaimComposite(req)
		if compositeErr != nil {
			return nil, compositeErr
		}
		identity := identities[0]
		atomicStore, ok := m.store.(AtomicLeaseStore)
		if !ok {
			return nil, fmt.Errorf("claim: lease store cannot atomically persist canonical hold identity")
		}
		lease, err = atomicStore.AcquireWithIdentity(ctx, req.Key, req.OwnerID, req.Role, req.WorktreePath, identity.Repository, identity.Owner, identity.Lane, m.now(), m.ttl)
	} else {
		lease, err = m.store.Acquire(ctx, req.Key, req.OwnerID, req.Role, req.WorktreePath, m.now(), m.ttl)
	}
	if err != nil {
		// Even a lost race can have flipped a stale row to Expired (see
		// LeaseStore.Acquire) before ultimately losing the insert to a
		// concurrent winner; settle it regardless of our own outcome so
		// that eviction's capacity token doesn't wait for someone else's
		// sweep.
		_, _ = m.settlePendingCapacity(ctx, &req.Key)
		return nil, err
	}
	// Acquire durably evicts (Expires) any stale prior lease for this key
	// as part of winning the claim, which is exactly what makes that
	// lease's row claimable via ClaimCapacityRelease. Settle it now,
	// before reserving the new token, so the old owner's capacity is
	// released before (or, if settlement itself fails, at minimum durably
	// queued ahead of) the new reservation instead of the new claim
	// silently stacking on top of an unreturned token. Errors are
	// intentionally not fatal to the new claim; the pending row stays
	// durable and retryable regardless (see settlePendingCapacity).
	_, _ = m.settlePendingCapacity(ctx, &req.Key)

	if err := m.capacity.Reserve(ctx, req.Role, capacityKey("reserve", lease)); err != nil {
		// Compensate: don't strand a durable lease with no capacity behind it.
		_, _, _ = m.store.Release(ctx, req.Key, req.OwnerID, lease.Generation, m.now())
		_, _ = m.settlePendingCapacity(ctx, &req.Key)
		return nil, fmt.Errorf("claim: reserve capacity for role %s: %w", req.Role, err)
	}

	_ = m.outbox.Record(ctx, OutboxIntent{
		IdempotencyKey: fmt.Sprintf("claim:%s/%s/%s/%s:g%d", req.Key.Repo, req.Key.Provider, req.Key.Project, req.Key.TaskRef, lease.Generation),
		Kind:           "lease_claimed",
	})
	return lease, nil
}

func identityValid(identity lifecycle.HoldIdentity) bool {
	if identity.Repository == "" || identity.Owner == "" || identity.Lane == "" {
		return false
	}
	if identity.Scope == "lane" {
		return identity.Task == ""
	}
	return identity.Scope == "task" && identity.Task != ""
}

// Renew extends an active lease's expiry, fenced by generation. Renewing
// a generation whose TTL has already passed (ErrLeaseExpired) or that no
// longer matches the active lease (ErrStaleGeneration) is rejected rather
// than silently extended.
func (m *ClaimManager) Renew(ctx context.Context, key LeaseKey, ownerID string, generation int64) (*Lease, error) {
	return m.store.Renew(ctx, key, ownerID, generation, m.now(), m.ttl)
}

// Release idempotently releases a fenced lease and durably settles its
// capacity token. Unlike a naive "only release capacity on the call that
// flipped the row" scheme, this always attempts settlement for the key
// after the store transition — including on a redundant/idempotent
// replay call — so a capacity-coordinator failure on a prior Release
// leaves the token durably pending and this call (or ExpireStale, or
// SettlePendingCapacity) retries it instead of returning nil having
// silently skipped it.
func (m *ClaimManager) Release(ctx context.Context, key LeaseKey, ownerID string, generation int64) error {
	// See Claim's matching comment: Release no longer preempts a stale
	// provider lock by time alone at the store layer either, so a durable
	// fence-advance is required first here too, or a genuinely-crashed
	// owner's own Release call (or anyone else's, for that matter) could
	// otherwise be the bypass route that skips fencing entirely.
	if err := m.preemptStaleProviderLock(ctx, key); err != nil {
		return err
	}

	_, _, err := m.store.Release(ctx, key, ownerID, generation, m.now())
	if err != nil {
		return err
	}
	_, err = m.settlePendingCapacity(ctx, &key)
	_ = m.outbox.Record(ctx, OutboxIntent{
		IdempotencyKey: fmt.Sprintf("release:%s/%s/%s/%s:g%d", key.Repo, key.Provider, key.Project, key.TaskRef, generation),
		Kind:           "lease_released",
	})
	return err
}

// Hold sets or clears operator hold on the active lease for key, fenced
// by the caller's ownerID/generation exactly like Release: a stale or
// wrong-owner caller is rejected rather than able to hold/unhold a lease
// it does not currently own.
func (m *ClaimManager) Hold(ctx context.Context, key LeaseKey, ownerID string, generation int64, held bool) (*Lease, error) {
	return m.store.Hold(ctx, key, ownerID, generation, held, m.now())
}

// ExpireStale first durably preempts every stale provider-locked lease
// across every key (see preemptAllStaleProviderLocks -- same requirement
// as Claim/Release: no lease with a provider lock is evicted by time
// alone without a confirmed fence advance first), then sweeps expired
// leases, then settles all pending capacity release across every key
// (not just the ones it just expired), so it also self-heals any earlier
// Release/Claim call whose capacity settlement failed and was left
// durably pending. Callers (e.g. a daemon tick, or Reconcile) decide the
// schedule; ClaimManager does not run a background loop itself.
//
// A preemption failure for one lease does not stop the sweep for
// everything else: it's reported (via the returned error, never
// swallowed) but that lease is simply left locked, to be retried on a
// future call, exactly like Claim/Release leave it for their own retry.
func (m *ClaimManager) ExpireStale(ctx context.Context) ([]*Lease, error) {
	if m.holdReader != nil {
		return nil, fmt.Errorf("claim: exact per-lease WithUnheldTransition fence is required for recovery")
	}
	return m.expireStaleUnlocked(ctx)
}

func (m *ClaimManager) expireStaleUnlocked(ctx context.Context) ([]*Lease, error) {
	preemptErr := m.preemptAllStaleProviderLocks(ctx)

	expired, err := m.store.ExpireStale(ctx, m.now())
	if err != nil {
		return nil, err
	}
	if _, settleErr := m.settlePendingCapacity(ctx, nil); settleErr != nil {
		return expired, settleErr
	}
	if preemptErr != nil {
		return expired, preemptErr
	}
	return expired, nil
}

// preemptStaleProviderLock durably advances the provider fence for key's
// stale-provider-locked active lease (if any) and, only on success,
// force-clears the lock so the lease becomes normally evictable/
// releasable by the store's own (lock-oblivious-to-staleness)
// Acquire/Release/ExpireStale. A no-op if there is no stale-locked lease
// for key, or if no ProviderCAS is configured (nothing external to
// protect, so a local lock's staleness alone is a sufficient and correct
// signal -- this is the pre-FAC-120-review, pre-fencing behavior,
// preserved exactly for callers who never touch the provider). A
// fence-advance failure is returned -- never swallowed -- with the lock
// left in place, so the caller (Claim, Release) refuses to proceed
// rather than exposing new local state the provider was never told
// about; a future call for the same key (the natural retry path) tries
// again with the same durable idempotency key.
func (m *ClaimManager) preemptStaleProviderLock(ctx context.Context, key LeaseKey) error {
	if m.provider == nil {
		return nil
	}
	stale, err := m.store.PeekStaleProviderLock(ctx, key, m.now())
	if err != nil {
		return fmt.Errorf("claim: check provider lock staleness: %w", err)
	}
	if stale == nil {
		return nil
	}
	if err := m.durablyAdvanceFence(ctx, key.TaskRef, stale.Generation+1); err != nil {
		return fmt.Errorf("claim: cannot safely preempt stale provider lock for %s: %w", key.TaskRef, err)
	}
	if err := m.store.ForceReleaseProviderLock(ctx, key, stale.Generation); err != nil {
		return fmt.Errorf("claim: clear preempted provider lock for %s: %w", key.TaskRef, err)
	}
	return nil
}

// preemptAllStaleProviderLocks is preemptStaleProviderLock across every
// key, for ExpireStale's global sweep. A fence-advance failure for one
// lease does not stop the others; the first error encountered is
// returned (never swallowed) after every eligible lease has been
// attempted.
func (m *ClaimManager) preemptAllStaleProviderLocks(ctx context.Context) error {
	if m.provider == nil {
		return nil
	}
	stales, err := m.store.PeekAllStaleProviderLocks(ctx, m.now())
	if err != nil {
		return fmt.Errorf("claim: list stale provider locks: %w", err)
	}
	var firstErr error
	for _, l := range stales {
		if err := m.durablyAdvanceFence(ctx, l.TaskRef, l.Generation+1); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim: cannot safely preempt stale provider lock for %s: %w", l.TaskRef, err)
			}
			continue
		}
		if err := m.store.ForceReleaseProviderLock(ctx, l.LeaseKey, l.Generation); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim: clear preempted provider lock for %s: %w", l.TaskRef, err)
			}
		}
	}
	return firstErr
}

// SettlePendingCapacity retries CapacityCoordinator.Release for every
// lease across every key whose capacity token has not yet been durably
// marked returned. Exposed standalone (in addition to being called by
// Claim/Release/ExpireStale) so a Reconciler or operator tool can drain a
// backlog left by a coordinator outage without waiting for lease
// activity to trigger it.
func (m *ClaimManager) SettlePendingCapacity(ctx context.Context) ([]*Lease, error) {
	return m.settlePendingCapacity(ctx, nil)
}

// settlePendingCapacity is the single place ClaimManager calls
// CapacityCoordinator.Release. key == nil settles across every key;
// otherwise only that key's pending lease(s).
//
// This is the durable capacity-release delivery protocol: ClaimCapacity
// Release atomically claims the batch this call is responsible for (the
// store-level CAS claim, not a Go mutex, is what makes it impossible for
// two concurrent settlers to both be attempting Release for the same
// lease — see LeaseStore's doc comment), then for each claimed lease this
// calls the coordinator with a stable idempotency key and only Acks
// (durably marks done) on success. A synchronous failure immediately
// releases the claim back to pending via FailCapacityRelease so the next
// attempt doesn't have to wait out the stale-claim timeout; a crash here
// (this settler dies between the coordinator call succeeding and Ack)
// leaves the claim to go stale on its own, and a later settler retries
// with the SAME idempotency key — an at-least-once redelivery a
// conforming coordinator dedupes into an effectively-exactly-once effect.
func (m *ClaimManager) settlePendingCapacity(ctx context.Context, key *LeaseKey) ([]*Lease, error) {
	claimed, err := m.store.ClaimCapacityRelease(ctx, m.settlerID, m.capacityClaimTimeout, m.now(), key)
	if err != nil {
		return nil, fmt.Errorf("claim: claim capacity release batch: %w", err)
	}

	var settled []*Lease
	var firstErr error
	for _, l := range claimed {
		if err := m.capacity.Release(ctx, l.Role, capacityKey("release", l)); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim: release capacity for %s (lease %d, role %s): %w", l.TaskRef, l.ID, l.Role, err)
			}
			_ = m.store.FailCapacityRelease(ctx, l.ID, m.settlerID) // durable, retryable: back to pending now, not stranded until staleAfter.
			continue
		}
		if err := m.store.AckCapacityRelease(ctx, l.ID, m.settlerID, m.now()); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim: ack capacity release for lease %d: %w", l.ID, err)
			}
			continue
		}
		settled = append(settled, l)
	}
	return settled, firstErr
}

// ActiveClaims returns only live leases.
func (m *ClaimManager) ActiveClaims(ctx context.Context) ([]*Lease, error) {
	return m.store.ActiveClaims(ctx, m.now())
}

// IsClaimed reports whether key currently has a live lease.
func (m *ClaimManager) IsClaimed(ctx context.Context, key LeaseKey) (bool, error) {
	claims, err := m.ActiveClaims(ctx)
	if err != nil {
		return false, err
	}
	for _, l := range claims {
		if l.LeaseKey == key {
			return true, nil
		}
	}
	return false, nil
}

// Reconcile implements Reconciler using the primitives ClaimManager
// already exposes: sweep expiry and settle any pending capacity release.
// This is the concrete, minimal default FAC-119's periodic reconciliation
// sweep can drive directly; a fuller reconciliation that also cross-checks
// outbox/provider state is FAC-119's to build on top of this.
func (m *ClaimManager) Reconcile(ctx context.Context) error {
	_, err := m.ExpireStale(ctx)
	return err
}

var _ Reconciler = (*ClaimManager)(nil)
