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
	kind     string
	owner    string
	lockedAt time.Time
}

func (s *InMemoryLeaseStore) validateProviderLockLocked(id int64) error {
	p, ok := s.provLock[id]
	if !ok {
		return nil
	}
	l := s.rows[id]
	switch p.kind {
	case providerLockKindOrdinary:
		if isReservedRecoveryOwner(p.owner) || (p.owner == "") != p.lockedAt.IsZero() {
			return fmt.Errorf("incoherent ordinary provider lock")
		}
	case providerLockKindRecovery:
		if l == nil || !isRecoveryOwnerFor(p.owner, l.ID, l.Generation) || p.lockedAt.IsZero() {
			return fmt.Errorf("incoherent recovery provider lock")
		}
	default:
		return fmt.Errorf("unknown provider lock kind %q", p.kind)
	}
	return nil
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
	if !ok {
		return false
	}
	if pl.kind != providerLockKindOrdinary && pl.kind != providerLockKindRecovery {
		return true
	}
	if pl.owner == "" || pl.owner == lockOwner {
		return false
	}
	return !pl.lockedAt.Before(staleBefore) // locked by someone else and not stale
}

// providerLockHeldAtAllLocked reports whether id has ANY provider-
// transition lock held, regardless of staleness -- what Acquire and Release
// use, deliberately not staleness-aware (see their
// comments and PeekStaleProviderLock's).
func (s *InMemoryLeaseStore) providerLockHeldAtAllLocked(id int64) bool {
	pl, ok := s.provLock[id]
	return ok && (pl.kind != providerLockKindOrdinary || pl.owner != "")
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

func (s *InMemoryLeaseStore) CurrentLease(_ context.Context, key LeaseKey) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest *Lease
	for _, l := range s.rows {
		if l.LeaseKey == key && (latest == nil || l.ID > latest.ID) {
			latest = l
		}
	}
	if latest == nil {
		return nil, nil
	}
	return cloneLease(latest), nil
}

func (s *InMemoryLeaseStore) LeaseByGeneration(_ context.Context, key LeaseKey, ownerID string, generation int64) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.byGenerationLocked(key, ownerID, generation)
	if l == nil {
		return nil, nil
	}
	return cloneLease(l), nil
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

func (s *InMemoryLeaseStore) SnapshotExpiredLeases(_ context.Context, now time.Time) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Lease
	for _, l := range s.rows {
		if l.Status == StatusActive && !l.Held && !now.Before(l.ExpiresAt) {
			out = append(out, cloneLease(l))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		if a.TaskRef != b.TaskRef {
			return a.TaskRef < b.TaskRef
		}
		return a.ID < b.ID
	})
	return out, nil
}

func (s *InMemoryLeaseStore) ExpireLeaseCAS(_ context.Context, id, generation int64, now time.Time) (*Lease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.rows[id]
	if err := s.validateProviderLockLocked(id); err != nil {
		return nil, false, err
	}
	if l == nil || l.ID != id || l.Generation != generation || l.Status != StatusActive || l.Held || s.providerLockHeldAtAllLocked(l.ID) || now.Before(l.ExpiresAt) {
		return nil, false, nil
	}
	l.Status = StatusExpired
	return cloneLease(l), true, nil
}

func (s *InMemoryLeaseStore) ObserveStaleProviderLock(_ context.Context, key LeaseKey, now time.Time) (*ProviderLockObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.activeLocked(key)
	if l == nil {
		return nil, nil
	}
	p := s.provLock[l.ID]
	if err := s.validateProviderLockLocked(l.ID); err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	if p.kind != providerLockKindOrdinary && p.kind != providerLockKindRecovery {
		return nil, fmt.Errorf("unknown provider lock kind %q", p.kind)
	}
	if p.owner == "" || p.lockedAt.IsZero() {
		return nil, fmt.Errorf("incoherent provider lock")
	}
	if p.kind == providerLockKindOrdinary && !p.lockedAt.Before(now.Add(-5*time.Minute)) {
		return nil, nil
	}
	recoveryOwner := recoveryOwnerFor(l.ID, l.Generation)
	recovery := false
	if p.kind == providerLockKindRecovery {
		recovery = true
		if !isRecoveryOwnerFor(p.owner, l.ID, l.Generation) {
			return nil, fmt.Errorf("incoherent recovery provider lock owner")
		}
		recoveryOwner = p.owner
	}
	return &ProviderLockObservation{LeaseID: l.ID, Generation: l.Generation, Owner: p.owner, LockedAt: p.lockedAt, ObservedAt: now, RecoveryOwner: recoveryOwner, Recovery: recovery}, nil
}

func (s *InMemoryLeaseStore) ClaimProviderLockCAS(_ context.Context, o ProviderLockObservation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.rows[o.LeaseID]
	p := s.provLock[o.LeaseID]
	if err := s.validateProviderLockLocked(o.LeaseID); err != nil {
		return false, err
	}
	if l == nil || p == nil || l.Generation != o.Generation || l.Status != StatusActive || p.owner != o.Owner || !p.lockedAt.Equal(o.LockedAt) || (p.kind == providerLockKindOrdinary && !p.lockedAt.Before(o.ObservedAt.Add(-5*time.Minute))) || (p.kind != providerLockKindOrdinary && p.kind != providerLockKindRecovery) {
		return false, nil
	}
	if p.kind == providerLockKindRecovery && p.owner != o.RecoveryOwner {
		return false, nil
	}
	if o.Recovery && (p.kind != providerLockKindRecovery || p.owner != o.RecoveryOwner) {
		return false, nil
	}
	if !isRecoveryOwnerFor(o.RecoveryOwner, o.LeaseID, o.Generation) {
		return false, fmt.Errorf("invalid recovery provider lock owner")
	}
	if !o.Recovery {
		p.kind = providerLockKindRecovery
		p.owner = o.RecoveryOwner
		p.lockedAt = normalizeProviderLockTime(o.ObservedAt)
	}
	return true, nil
}

func (s *InMemoryLeaseStore) FinalizeProviderLockCAS(_ context.Context, o ProviderLockObservation) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.provLock[o.LeaseID]
	if err := s.validateProviderLockLocked(o.LeaseID); err != nil {
		return false, err
	}
	if p == nil || p.kind != providerLockKindRecovery || p.owner != o.RecoveryOwner || !p.lockedAt.Equal(o.LockedAt) {
		return false, nil
	}
	delete(s.provLock, o.LeaseID)
	return true, nil
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
	now = normalizeProviderLockTime(now)
	if err := validateAttributableID(lockOwner, "provider lock owner"); err != nil {
		return nil, err
	}
	if isReservedRecoveryOwner(lockOwner) {
		return nil, fmt.Errorf("reserved recovery lock owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	target := s.byGenerationLocked(key, ownerID, generation)
	if target == nil || target.Status != StatusActive {
		return nil, s.fencingErrorLocked(key, ownerID, generation)
	}
	if err := s.validateProviderLockLocked(target.ID); err != nil {
		return nil, err
	}
	if s.providerLockHeldAtAllLocked(target.ID) {
		return nil, fmt.Errorf("%w: %s generation %d", ErrProviderTransitionInProgress, key.TaskRef, generation)
	}
	s.provLock[target.ID] = &providerLock{kind: providerLockKindOrdinary, owner: lockOwner, lockedAt: now}
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
		if err := s.validateProviderLockLocked(id); err != nil {
			return err
		}
		if pl, ok := s.provLock[id]; ok && pl.kind == providerLockKindRecovery {
			return fmt.Errorf("%w: recovery claim", ErrProviderTransitionInProgress)
		} else if ok && pl.kind != providerLockKindOrdinary {
			return fmt.Errorf("unknown provider lock kind %q", pl.kind)
		} else if ok && pl.owner == lockOwner {
			delete(s.provLock, id)
			return nil
		}
		return fmt.Errorf("%w: release did not match", ErrProviderLockStale)
	}
	return fmt.Errorf("%w: release did not find lease", ErrProviderLockStale)
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
	if err := s.validateProviderLockLocked(l.ID); err != nil {
		return nil, err
	}
	if !ok || pl.owner == "" || pl.kind != providerLockKindOrdinary {
		if ok && pl.kind != providerLockKindOrdinary && pl.kind != providerLockKindRecovery {
			return nil, fmt.Errorf("unknown provider lock kind %q", pl.kind)
		}
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
		if err := s.validateProviderLockLocked(id); err != nil {
			return nil, err
		}
		if !ok || pl.owner == "" || pl.kind != providerLockKindOrdinary || !pl.lockedAt.Before(staleBefore) {
			if ok && pl.kind != providerLockKindOrdinary && pl.kind != providerLockKindRecovery {
				return nil, fmt.Errorf("unknown provider lock kind %q", pl.kind)
			}
			continue
		}
		out = append(out, cloneLease(l))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ForceReleaseProviderLock is retained only as a typed refusal for legacy callers.
func (s *InMemoryLeaseStore) ForceReleaseProviderLock(_ context.Context, _ LeaseKey, _ int64) error {
	return fmt.Errorf("claim: unfenced provider-lock force release is disabled; use lifecycle recovery")
}

func (s *InMemoryLeaseStore) Hold(_ context.Context, key LeaseKey, ownerID string, generation int64, held bool, now time.Time) (*Lease, error) {
	return nil, ErrLegacyLeaseHoldDisabled
}

// ExpireStale is retained only as a typed refusal for legacy callers;
// lifecycle recovery uses ExpireLeaseCAS.
func (s *InMemoryLeaseStore) ExpireStale(_ context.Context, _ time.Time) ([]*Lease, error) {
	return nil, fmt.Errorf("claim: unfenced store expiry is disabled; use lifecycle recovery")
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

// ClaimCapacityRelease is a hard-disabled compatibility symbol. Batch
// capacity mutation is forbidden outside the lifecycle authority fence.
func (s *InMemoryLeaseStore) ClaimCapacityRelease(_ context.Context, settlerID string, staleAfter time.Duration, now time.Time, key *LeaseKey) ([]*Lease, error) {
	return nil, fmt.Errorf("claim: raw batch capacity mutation is disabled; use fenced exact settlement")
}

func (s *InMemoryLeaseStore) ClaimCapacityReleaseExact(_ context.Context, leaseID, generation int64, settlerID string, staleAfter time.Duration, now time.Time) (*Lease, bool, error) {
	if err := validateAttributableID(settlerID, "settler identity"); err != nil {
		return nil, false, err
	}
	if err := validatePositiveDuration(staleAfter, "capacity claim timeout"); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.rows[leaseID]
	if l == nil || l.Generation != generation || !l.PendingCapacityRelease() {
		return nil, false, nil
	}
	c := s.capClaimLocked(leaseID)
	if c.state == "cancelled" {
		copy := cloneLease(l)
		copy.CapacityReleaseState = "cancelled"
		return copy, false, nil
	}
	if c.state != "pending" && !(c.state == "in_progress" && c.claimedAt.Before(now.Add(-staleAfter))) {
		return nil, false, nil
	}
	c.state, c.owner, c.claimedAt = "in_progress", settlerID, now
	return cloneLease(l), true, nil
}

func (s *InMemoryLeaseStore) PendingCapacityReleases(_ context.Context) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Lease
	for _, l := range s.rows {
		c := s.capRel[l.ID]
		if l.PendingCapacityRelease() && (c == nil || c.state == "pending" || c.state == "in_progress") {
			out = append(out, cloneLease(l))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *InMemoryLeaseStore) AbortUnreservedLease(_ context.Context, snapshot *Lease, now time.Time) (*Lease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot == nil {
		return nil, false, fmt.Errorf("%w: abort unreserved nil lease", ErrCapacityReleaseStale)
	}
	l := s.rows[snapshot.ID]
	if l == nil || l.ID != snapshot.ID || l.LeaseKey != snapshot.LeaseKey || l.Generation != snapshot.Generation || l.OwnerID != snapshot.OwnerID || l.Role != snapshot.Role || l.HoldRepository != snapshot.HoldRepository || l.HoldOwner != snapshot.HoldOwner || l.HoldLane != snapshot.HoldLane || l.WorktreePath != snapshot.WorktreePath || !l.ClaimedAt.Equal(snapshot.ClaimedAt) || l.TaskRef != snapshot.TaskRef || l.Status != StatusActive || l.CapacityReleasedAt != nil {
		return nil, false, fmt.Errorf("%w: abort unreserved identity mismatch", ErrCapacityReleaseStale)
	}
	if s.providerLockHeldAtAllLocked(l.ID) {
		return nil, false, fmt.Errorf("%w: abort provider lock", ErrProviderTransitionInProgress)
	}
	c := s.capClaimLocked(snapshot.ID)
	if c.state != "pending" {
		return nil, false, fmt.Errorf("%w: abort capacity state", ErrCapacityReleaseStale)
	}
	c.state = "cancelled"
	c.owner = ""
	c.claimedAt = time.Time{}
	l.Status = StatusReleased
	t := now
	l.ReleasedAt = &t
	copy := cloneLease(l)
	copy.CapacityReleaseState = "cancelled"
	return copy, true, nil
}

func (s *InMemoryLeaseStore) AckCapacityRelease(_ context.Context, leaseID int64, settlerID string, now time.Time) error {
	if err := validateAttributableID(settlerID, "settler identity"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.capRel[leaseID]
	if !ok || c.state != "in_progress" || c.owner != settlerID {
		return fmt.Errorf("%w: ack", ErrCapacityReleaseStale)
	}
	c.state = "done"
	if l, ok := s.rows[leaseID]; ok && l.CapacityReleasedAt == nil {
		t := now
		l.CapacityReleasedAt = &t
	}
	return nil
}

func (s *InMemoryLeaseStore) FailCapacityRelease(_ context.Context, leaseID int64, settlerID string) error {
	if err := validateAttributableID(settlerID, "settler identity"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	c, ok := s.capRel[leaseID]
	if !ok || c.state != "in_progress" || c.owner != settlerID {
		return fmt.Errorf("%w: fail", ErrCapacityReleaseStale)
	}
	c.state = "pending"
	c.owner = ""
	return nil
}

var _ LeaseStore = (*InMemoryLeaseStore)(nil)
