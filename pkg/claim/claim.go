// Package claim provides durable, fenced, cross-process leases over
// provider tasks: at most one owner can hold an active lease per
// (repo, provider, project, task_ref) at a time, even across restarts and
// concurrent Herdforge processes on the same box.
package claim

import (
	"context"
	"errors"
	"fmt"
	"time"
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
// Release must be idempotent: ClaimManager's durable settlement mechanism
// (see settlePendingCapacity) guarantees it will call Release again for
// the same lease if a prior attempt failed or if it cannot itself prove
// exactly one settler won a race, so implementations must treat repeated
// Release(role) calls for the same underlying token as safe. This is a
// deliberate tradeoff: pkg/claim guarantees at-least-once, durable,
// crash-safe delivery of the release call (never "forgets" a token, never
// reports success without the coordinator having accepted it), and asks
// the coordinator to make redundant delivery a no-op rather than building
// a distributed exactly-once executor inside a lease package.
//
// FAC-120/FAC-119 boundary: no package in this repo implements capacity
// pools yet. Until FAC-119's durable lifecycle/outbox service exists to
// enroll Reserve/Release in the same transaction as the lease write, this
// hook runs as a best-effort step immediately after the durable lease
// commit (with a compensating Release on Reserve failure), not inside it.
// A no-op coordinator is used when none is supplied.
type CapacityCoordinator interface {
	Reserve(ctx context.Context, role string) error
	Release(ctx context.Context, role string) error
}

type noopCapacity struct{}

func (noopCapacity) Reserve(context.Context, string) error { return nil }
func (noopCapacity) Release(context.Context, string) error { return nil }

// ClaimManager enforces role-matching and drives a LeaseStore to produce
// atomic cross-process leases with renewal, expiry, operator hold, and
// durable, retryable capacity settlement.
type ClaimManager struct {
	store    LeaseStore
	capacity CapacityCoordinator
	outbox   OutboxRecorder
	now      func() time.Time
	ttl      time.Duration
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

// NewClaimManager builds a ClaimManager over store. store's lifetime is
// owned by the caller (Close it when the manager is no longer needed).
func NewClaimManager(store LeaseStore, opts ...Option) *ClaimManager {
	m := &ClaimManager{store: store, capacity: noopCapacity{}, outbox: noopOutbox{}, now: time.Now, ttl: 10 * time.Minute}
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

	lease, err := m.store.Acquire(ctx, req.Key, req.OwnerID, req.Role, req.WorktreePath, m.now(), m.ttl)
	if err != nil {
		// Even a lost race can have flipped a stale row to Expired (see
		// LeaseStore.Acquire) before ultimately losing the insert to a
		// concurrent winner; settle it regardless of our own outcome so
		// that eviction's capacity token doesn't wait for someone else's
		// sweep.
		_, _ = m.settlePendingCapacity(ctx, func(k LeaseKey) bool { return k == req.Key })
		return nil, err
	}

	// Acquire durably evicts (Expires) any stale prior lease for this key
	// as part of winning the claim, which is exactly what makes that
	// lease's row show up in PendingCapacityRelease. Settle it now, before
	// reserving the new token, so the old owner's capacity is released
	// before (or, if settlement itself fails, at minimum durably queued
	// ahead of) the new reservation instead of the new claim silently
	// stacking on top of an unreturned token. Errors are intentionally not
	// fatal to the new claim; the pending row stays durable and retryable
	// regardless (see settlePendingCapacity).
	_, _ = m.settlePendingCapacity(ctx, func(k LeaseKey) bool { return k == req.Key })

	if err := m.capacity.Reserve(ctx, req.Role); err != nil {
		// Compensate: don't strand a durable lease with no capacity behind it.
		_, _, _ = m.store.Release(ctx, req.Key, req.OwnerID, lease.Generation, m.now())
		_, _ = m.settlePendingCapacity(ctx, func(k LeaseKey) bool { return k == req.Key })
		return nil, fmt.Errorf("claim: reserve capacity for role %s: %w", req.Role, err)
	}

	_ = m.outbox.Record(ctx, OutboxIntent{
		IdempotencyKey: fmt.Sprintf("claim:%s/%s/%s/%s:g%d", req.Key.Repo, req.Key.Provider, req.Key.Project, req.Key.TaskRef, lease.Generation),
		Kind:           "lease_claimed",
	})
	return lease, nil
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
	_, _, err := m.store.Release(ctx, key, ownerID, generation, m.now())
	if err != nil {
		return err
	}
	_, err = m.settlePendingCapacity(ctx, func(k LeaseKey) bool { return k == key })
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

// ExpireStale sweeps expired leases and then settles all pending capacity
// release across every key (not just the ones it just expired), so it
// also self-heals any earlier Release/Claim call whose capacity
// settlement failed and was left durably pending. Callers (e.g. a daemon
// tick, or Reconcile) decide the schedule; ClaimManager does not run a
// background loop itself.
func (m *ClaimManager) ExpireStale(ctx context.Context) ([]*Lease, error) {
	expired, err := m.store.ExpireStale(ctx, m.now())
	if err != nil {
		return nil, err
	}
	if _, settleErr := m.settlePendingCapacity(ctx, nil); settleErr != nil {
		return expired, settleErr
	}
	return expired, nil
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
// CapacityCoordinator.Release. filter == nil settles every pending lease;
// otherwise only leases whose key matches filter. A coordinator failure
// for one lease does not stop settlement of the others, but is reported
// via the returned error (the first one encountered) after all pending
// leases have been attempted.
func (m *ClaimManager) settlePendingCapacity(ctx context.Context, filter func(LeaseKey) bool) ([]*Lease, error) {
	pending, err := m.store.PendingCapacityRelease(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim: list pending capacity release: %w", err)
	}

	var settled []*Lease
	var firstErr error
	for _, l := range pending {
		if filter != nil && !filter(l.LeaseKey) {
			continue
		}
		if err := m.capacity.Release(ctx, l.Role); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim: release capacity for %s (lease %d, role %s): %w", l.TaskRef, l.ID, l.Role, err)
			}
			continue // leave pending: durable, retryable, never marked done on failure.
		}
		if err := m.store.MarkCapacityReleased(ctx, l.ID, m.now()); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim: mark capacity released for lease %d: %w", l.ID, err)
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
