package claim

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// InMemoryLeaseStore is a process-local LeaseStore: it has exactly the
// same cross-process limitation the pre-FAC-120 ClaimManager had (correct
// only within one OS process, via a single mutex). It exists to back
// NewInMemoryClaimManager, the migration path for callers not (yet)
// adopting SQLiteLeaseStore's cross-process durability — see compat.go.
type InMemoryLeaseStore struct {
	mu       sync.Mutex
	nextID   int64
	rows     map[int64]*Lease
	capRel   map[int64]*capacityReleaseClaim
	provLock map[int64]*providerLock
}

// capacityReleaseClaim mirrors the SQLite store's
// capacity_release_state/owner/claimed_at columns for the same claim/ack
// protocol, kept out of the public Lease struct since it's private
// bookkeeping.
type capacityReleaseClaim struct {
	state     string // pending | in_progress | done
	owner     string
	claimedAt time.Time
}

// providerLock mirrors the SQLite store's provider_lock_owner/
// provider_lock_at columns.
type providerLock struct {
	owner    string
	lockedAt time.Time
}

// NewInMemoryLeaseStore builds an empty InMemoryLeaseStore.
func NewInMemoryLeaseStore() *InMemoryLeaseStore {
	return &InMemoryLeaseStore{
		rows: make(map[int64]*Lease), capRel: make(map[int64]*capacityReleaseClaim), provLock: make(map[int64]*providerLock),
	}
}

// providerLockedLocked reports whether id has a live (non-stale as of
// now) provider-transition lock held by anyone other than lockOwner.
func (s *InMemoryLeaseStore) providerLockedLocked(id int64, lockOwner string, staleBefore time.Time) bool {
	pl, ok := s.provLock[id]
	if !ok || pl.owner == "" || pl.owner == lockOwner {
		return false
	}
	return !pl.lockedAt.Before(staleBefore) // locked by someone else and not stale
}

// providerLockHeldAtAllLocked reports whether id has ANY provider-
// transition lock held, regardless of staleness -- what Acquire, Release,
// and ExpireStale use, deliberately not staleness-aware (see their
// comments and PeekStaleProviderLock's).
func (s *InMemoryLeaseStore) providerLockHeldAtAllLocked(id int64) bool {
	pl, ok := s.provLock[id]
	return ok && pl.owner != ""
}

func (s *InMemoryLeaseStore) Close() error { return nil }

func cloneLease(l *Lease) *Lease {
	c := *l
	return &c
}

func (s *InMemoryLeaseStore) activeLocked(key LeaseKey) *Lease {
	for _, l := range s.rows {
		if l.LeaseKey == key && l.Status == StatusActive {
			return l
		}
	}
	return nil
}

func (s *InMemoryLeaseStore) latestGenerationLocked(key LeaseKey) int64 {
	var max int64
	for _, l := range s.rows {
		if l.LeaseKey == key && l.Generation > max {
			max = l.Generation
		}
	}
	return max
}

func (s *InMemoryLeaseStore) byGenerationLocked(key LeaseKey, ownerID string, generation int64) *Lease {
	var found *Lease
	for _, l := range s.rows {
		if l.LeaseKey == key && l.OwnerID == ownerID && l.Generation == generation {
			if found == nil || l.ID > found.ID {
				found = l
			}
		}
	}
	return found
}

func (s *InMemoryLeaseStore) fencingErrorLocked(key LeaseKey, ownerID string, generation int64) error {
	if active := s.activeLocked(key); active != nil {
		return fmt.Errorf("%w: active generation is %d, caller had %d", ErrStaleGeneration, active.Generation, generation)
	}
	return fmt.Errorf("%w: no lease for %s owned by %s at generation %d", ErrNotFound, key.TaskRef, ownerID, generation)
}

func (s *InMemoryLeaseStore) Acquire(ctx context.Context, key LeaseKey, ownerID, role, worktreePath string, now time.Time, ttl time.Duration) (*Lease, error) {
	return s.acquire(ctx, key, ownerID, role, worktreePath, "", "", "", now, ttl)
}

func (s *InMemoryLeaseStore) AcquireWithIdentity(ctx context.Context, key LeaseKey, ownerID, role, worktreePath, holdRepository, holdOwner, holdLane string, now time.Time, ttl time.Duration) (*Lease, error) {
	return s.acquire(ctx, key, ownerID, role, worktreePath, holdRepository, holdOwner, holdLane, now, ttl)
}

func (s *InMemoryLeaseStore) acquire(_ context.Context, key LeaseKey, ownerID, role, worktreePath, holdRepository, holdOwner, holdLane string, now time.Time, ttl time.Duration) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing := s.activeLocked(key); existing != nil {
		locked := s.providerLockHeldAtAllLocked(existing.ID)
		if !existing.Expired(now) || locked {
			reason := "active and unexpired"
			if existing.Expired(now) {
				reason = "expired but blocked by an in-progress provider transition"
			}
			return nil, &ClaimConflictError{Key: key, Lease: cloneLease(existing), Reason: reason}
		}
		existing.Status = StatusExpired
	}

	gen := s.latestGenerationLocked(key) + 1
	s.nextID++
	l := &Lease{
		ID: s.nextID, LeaseKey: key, OwnerID: ownerID, Role: role, HoldRepository: holdRepository, HoldOwner: holdOwner, HoldLane: holdLane, WorktreePath: worktreePath,
		Generation: gen, Status: StatusActive, ClaimedAt: now, RenewedAt: now, ExpiresAt: now.Add(ttl),
	}
	s.rows[l.ID] = l
	return cloneLease(l), nil
}

func (s *InMemoryLeaseStore) Renew(_ context.Context, key LeaseKey, ownerID string, generation int64, now time.Time, ttl time.Duration) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := s.activeLocked(key)
	if l == nil || l.OwnerID != ownerID || l.Generation != generation {
		return nil, s.fencingErrorLocked(key, ownerID, generation)
	}
	if !l.Held && !now.Before(l.ExpiresAt) {
		return nil, fmt.Errorf("%w: lease expired at %s", ErrLeaseExpired, l.ExpiresAt.Format(time.RFC3339))
	}
	l.RenewedAt = now
	l.ExpiresAt = now.Add(ttl)
	return cloneLease(l), nil
}

func (s *InMemoryLeaseStore) Release(_ context.Context, key LeaseKey, ownerID string, generation int64, now time.Time) (*Lease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	target := s.byGenerationLocked(key, ownerID, generation)
	if target == nil {
		return nil, false, s.fencingErrorLocked(key, ownerID, generation)
	}
	if target.Status == StatusReleased {
		return cloneLease(target), false, nil
	}
	if target.Status != StatusActive {
		return nil, false, fmt.Errorf("release: %w: generation %d is %s, not active", ErrStaleGeneration, generation, target.Status)
	}
	if s.providerLockHeldAtAllLocked(target.ID) {
		return nil, false, fmt.Errorf("%w: %s generation %d", ErrProviderTransitionInProgress, key.TaskRef, generation)
	}
	target.Status = StatusReleased
	t := now
	target.ReleasedAt = &t
	return cloneLease(target), true, nil
}

// AcquireProviderLock implements LeaseStore. Runs entirely under s.mu, so
// (matching the SQLite store's single-writer-lock atomicity) the fencing
// check and the lock acquisition cannot be interleaved by a concurrent
// Release/Acquire.
func (s *InMemoryLeaseStore) AcquireProviderLock(_ context.Context, key LeaseKey, ownerID string, generation int64, lockOwner string, staleAfter time.Duration, now time.Time) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	target := s.byGenerationLocked(key, ownerID, generation)
	if target == nil || target.Status != StatusActive {
		return nil, s.fencingErrorLocked(key, ownerID, generation)
	}
	if s.providerLockedLocked(target.ID, lockOwner, now.Add(-staleAfter)) {
		return nil, fmt.Errorf("%w: %s generation %d", ErrProviderTransitionInProgress, key.TaskRef, generation)
	}
	s.provLock[target.ID] = &providerLock{owner: lockOwner, lockedAt: now}
	return cloneLease(target), nil
}

// ReleaseProviderLock implements LeaseStore.
func (s *InMemoryLeaseStore) ReleaseProviderLock(_ context.Context, key LeaseKey, generation int64, lockOwner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, l := range s.rows {
		if l.LeaseKey != key || l.Generation != generation {
			continue
		}
		if pl, ok := s.provLock[id]; ok && pl.owner == lockOwner {
			delete(s.provLock, id)
		}
	}
	return nil
}

// PeekStaleProviderLock implements LeaseStore.
func (s *InMemoryLeaseStore) PeekStaleProviderLock(_ context.Context, key LeaseKey, now time.Time) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := s.activeLocked(key)
	if l == nil {
		return nil, nil
	}
	pl, ok := s.provLock[l.ID]
	if !ok || pl.owner == "" {
		return nil, nil
	}
	staleBefore := now.Add(-providerLockStaleAfter)
	if pl.lockedAt.Before(staleBefore) {
		return cloneLease(l), nil
	}
	return nil, nil
}

// PeekAllStaleProviderLocks implements LeaseStore.
func (s *InMemoryLeaseStore) PeekAllStaleProviderLocks(_ context.Context, now time.Time) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	staleBefore := now.Add(-providerLockStaleAfter)
	var out []*Lease
	for id, l := range s.rows {
		if l.Status != StatusActive {
			continue
		}
		pl, ok := s.provLock[id]
		if !ok || pl.owner == "" || !pl.lockedAt.Before(staleBefore) {
			continue
		}
		out = append(out, cloneLease(l))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ForceReleaseProviderLock implements LeaseStore.
func (s *InMemoryLeaseStore) ForceReleaseProviderLock(_ context.Context, key LeaseKey, generation int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, l := range s.rows {
		if l.LeaseKey == key && l.Generation == generation {
			delete(s.provLock, id)
		}
	}
	return nil
}

func (s *InMemoryLeaseStore) Hold(_ context.Context, key LeaseKey, ownerID string, generation int64, held bool, now time.Time) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := s.activeLocked(key)
	if l == nil || l.OwnerID != ownerID || l.Generation != generation {
		return nil, s.fencingErrorLocked(key, ownerID, generation)
	}
	l.Held = held
	l.RenewedAt = now
	return cloneLease(l), nil
}

// ExpireStale never preempts a provider lock by time alone -- see the
// matching comment on SQLiteLeaseStore.ExpireStale.
func (s *InMemoryLeaseStore) ExpireStale(_ context.Context, now time.Time) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Lease
	for id, l := range s.rows {
		if l.Status == StatusActive && !l.Held && !now.Before(l.ExpiresAt) && !s.providerLockHeldAtAllLocked(id) {
			l.Status = StatusExpired
			out = append(out, cloneLease(l))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *InMemoryLeaseStore) ActiveClaims(_ context.Context, now time.Time) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Lease
	for _, l := range s.rows {
		if l.Status == StatusActive && (l.Held || now.Before(l.ExpiresAt)) {
			out = append(out, cloneLease(l))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClaimedAt.Before(out[j].ClaimedAt) })
	return out, nil
}

func (s *InMemoryLeaseStore) capClaimLocked(id int64) *capacityReleaseClaim {
	c, ok := s.capRel[id]
	if !ok {
		c = &capacityReleaseClaim{state: "pending"}
		s.capRel[id] = c
	}
	return c
}

// ClaimCapacityRelease implements LeaseStore. The whole scan-and-claim
// runs under s.mu, so (matching the SQLite store's single-writer-lock
// atomicity) no other call can observe or claim a row mid-scan: exactly
// one caller's claim can ever win a given lease at a time.
func (s *InMemoryLeaseStore) ClaimCapacityRelease(_ context.Context, settlerID string, staleAfter time.Duration, now time.Time, key *LeaseKey) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	staleBefore := now.Add(-staleAfter)
	var claimed []*Lease
	for id, l := range s.rows {
		if !l.PendingCapacityRelease() {
			continue
		}
		if key != nil && l.LeaseKey != *key {
			continue
		}
		c := s.capClaimLocked(id)
		eligible := c.state == "pending" || (c.state == "in_progress" && c.claimedAt.Before(staleBefore))
		if !eligible {
			continue
		}
		c.state = "in_progress"
		c.owner = settlerID
		c.claimedAt = now
		claimed = append(claimed, cloneLease(l))
	}
	sort.Slice(claimed, func(i, j int) bool { return claimed[i].ID < claimed[j].ID })
	return claimed, nil
}

func (s *InMemoryLeaseStore) AckCapacityRelease(_ context.Context, leaseID int64, settlerID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.capRel[leaseID]
	if !ok || c.state != "in_progress" || c.owner != settlerID {
		return nil // stale/foreign claim: no-op, matching SQLite's guarded UPDATE.
	}
	c.state = "done"
	if l, ok := s.rows[leaseID]; ok && l.CapacityReleasedAt == nil {
		t := now
		l.CapacityReleasedAt = &t
	}
	return nil
}

func (s *InMemoryLeaseStore) FailCapacityRelease(_ context.Context, leaseID int64, settlerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.capRel[leaseID]
	if !ok || c.state != "in_progress" || c.owner != settlerID {
		return nil
	}
	c.state = "pending"
	c.owner = ""
	return nil
}

var _ LeaseStore = (*InMemoryLeaseStore)(nil)
