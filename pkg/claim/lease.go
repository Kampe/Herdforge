package claim

import "time"

// LeaseKey identifies the unique claimable unit of work: one task, in one
// project, on one provider, for one repo.
type LeaseKey struct {
	Repo     string
	Provider string
	Project  string
	TaskRef  string
}

// LeaseStatus is the lifecycle state of a Lease row.
type LeaseStatus string

const (
	StatusActive   LeaseStatus = "active"
	StatusReleased LeaseStatus = "released"
	StatusExpired  LeaseStatus = "expired"
)

// Lease is a durable, fenced claim on a LeaseKey. Generation increases
// monotonically each time a key is reclaimed after expiry or release, so a
// caller holding a stale generation can be rejected on Renew/Release
// (fencing) instead of silently acting on a claim it no longer owns.
type Lease struct {
	ID           int64
	LeaseKey
	OwnerID      string
	Role         string
	WorktreePath string
	Generation   int64
	Status       LeaseStatus
	Held         bool
	ClaimedAt    time.Time
	RenewedAt    time.Time
	ExpiresAt    time.Time
	ReleasedAt   *time.Time
}

// Age reports how long the lease has been held as of now.
func (l *Lease) Age(now time.Time) time.Duration {
	return now.Sub(l.ClaimedAt)
}

// Expired reports whether the lease's TTL has passed. Held leases (operator
// hold) never expire.
func (l *Lease) Expired(now time.Time) bool {
	return !l.Held && now.After(l.ExpiresAt)
}
