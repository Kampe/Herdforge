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
	mu     sync.Mutex
	nextID int64
	rows   map[int64]*Lease
}

// NewInMemoryLeaseStore builds an empty InMemoryLeaseStore.
func NewInMemoryLeaseStore() *InMemoryLeaseStore {
	return &InMemoryLeaseStore{rows: make(map[int64]*Lease)}
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

func (s *InMemoryLeaseStore) Acquire(_ context.Context, key LeaseKey, ownerID, role, worktreePath string, now time.Time, ttl time.Duration) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing := s.activeLocked(key); existing != nil {
		if !existing.Expired(now) {
			return nil, &ClaimConflictError{Key: key, Lease: cloneLease(existing), Reason: "active and unexpired"}
		}
		existing.Status = StatusExpired
	}

	gen := s.latestGenerationLocked(key) + 1
	s.nextID++
	l := &Lease{
		ID: s.nextID, LeaseKey: key, OwnerID: ownerID, Role: role, WorktreePath: worktreePath,
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
	target.Status = StatusReleased
	t := now
	target.ReleasedAt = &t
	return cloneLease(target), true, nil
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

func (s *InMemoryLeaseStore) ExpireStale(_ context.Context, now time.Time) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Lease
	for _, l := range s.rows {
		if l.Status == StatusActive && !l.Held && !now.Before(l.ExpiresAt) {
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

func (s *InMemoryLeaseStore) PendingCapacityRelease(_ context.Context) ([]*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*Lease
	for _, l := range s.rows {
		if l.PendingCapacityRelease() {
			out = append(out, cloneLease(l))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *InMemoryLeaseStore) MarkCapacityReleased(_ context.Context, leaseID int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.rows[leaseID]
	if !ok {
		return fmt.Errorf("%w: lease id %d", ErrNotFound, leaseID)
	}
	if l.CapacityReleasedAt == nil {
		t := now
		l.CapacityReleasedAt = &t
	}
	return nil
}

var _ LeaseStore = (*InMemoryLeaseStore)(nil)
