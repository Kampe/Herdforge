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
// FAC-120/FAC-119 boundary: no package in this repo implements capacity
// pools yet. Until FAC-119's durable lifecycle/outbox service exists to
// enroll Reserve/Release in the same transaction as the lease write,
// this hook runs as a best-effort step immediately after the durable
// lease commit (with a compensating Release on Reserve failure), not
// inside it. A no-op coordinator is used when none is supplied.
type CapacityCoordinator interface {
	Reserve(ctx context.Context, role string) error
	Release(ctx context.Context, role string) error
}

type noopCapacity struct{}

func (noopCapacity) Reserve(context.Context, string) error { return nil }
func (noopCapacity) Release(context.Context, string) error { return nil }

// ClaimManager enforces role-matching and drives a LeaseStore to produce
// atomic cross-process leases with renewal, expiry, operator hold, and
// idempotent, exactly-once-capacity release.
type ClaimManager struct {
	store    LeaseStore
	capacity CapacityCoordinator
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

// NewClaimManager builds a ClaimManager over store. store's lifetime is
// owned by the caller (Close it when the manager is no longer needed).
func NewClaimManager(store LeaseStore, opts ...Option) *ClaimManager {
	m := &ClaimManager{store: store, capacity: noopCapacity{}, now: time.Now, ttl: 10 * time.Minute}
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
func (m *ClaimManager) Claim(ctx context.Context, req ClaimRequest) (*Lease, error) {
	if req.TaskRole == "" {
		return nil, ErrUnlabeledTask
	}
	if req.Role == "" || req.Role != req.TaskRole {
		return nil, ErrRoleMismatch
	}

	lease, err := m.store.Acquire(ctx, req.Key, req.OwnerID, req.Role, req.WorktreePath, m.now(), m.ttl)
	if err != nil {
		return nil, err
	}

	if err := m.capacity.Reserve(ctx, req.Role); err != nil {
		// Compensate: don't strand a durable lease with no capacity behind it.
		_, _, _ = m.store.Release(ctx, req.Key, req.OwnerID, lease.Generation, m.now())
		return nil, fmt.Errorf("claim: reserve capacity for role %s: %w", req.Role, err)
	}
	return lease, nil
}

// Renew extends an active lease's expiry, fenced by generation.
func (m *ClaimManager) Renew(ctx context.Context, key LeaseKey, ownerID string, generation int64) (*Lease, error) {
	return m.store.Renew(ctx, key, ownerID, generation, m.now(), m.ttl)
}

// Release idempotently releases a fenced lease. Capacity is released
// exactly once: a second Release for the same (key, owner, generation)
// after success returns nil without calling the coordinator again.
func (m *ClaimManager) Release(ctx context.Context, key LeaseKey, ownerID string, generation int64) error {
	lease, transitioned, err := m.store.Release(ctx, key, ownerID, generation, m.now())
	if err != nil {
		return err
	}
	if transitioned {
		return m.capacity.Release(ctx, lease.Role)
	}
	return nil
}

// Hold sets or clears operator hold on the active lease for key.
func (m *ClaimManager) Hold(ctx context.Context, key LeaseKey, held bool) (*Lease, error) {
	return m.store.Hold(ctx, key, held, m.now())
}

// ExpireStale sweeps expired leases, releasing capacity exactly once per
// lease it transitions. Callers (e.g. a daemon tick) decide the schedule;
// ClaimManager does not run a background loop itself.
func (m *ClaimManager) ExpireStale(ctx context.Context) ([]*Lease, error) {
	expired, err := m.store.ExpireStale(ctx, m.now())
	if err != nil {
		return nil, err
	}
	for _, l := range expired {
		if err := m.capacity.Release(ctx, l.Role); err != nil {
			return expired, fmt.Errorf("claim: release capacity for expired %s: %w", l.TaskRef, err)
		}
	}
	return expired, nil
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
