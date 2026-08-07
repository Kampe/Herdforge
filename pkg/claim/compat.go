package claim

import (
	"context"
	"time"
)

// legacyRole is the fixed pseudo-role the ClaimTask compatibility path
// uses internally so it can call the strict, role-aware Claim without
// requiring pre-FAC-120 callers (which had no concept of roles) to supply
// one. If a CapacityCoordinator is configured, legacy claims reserve
// capacity under this pseudo-role.
const legacyRole = "legacy"

func legacyKey(taskRef string) LeaseKey { return LeaseKey{TaskRef: taskRef} }

// ClaimRecord is the pre-FAC-120 claim record shape, preserved for
// ClaimTask callers migrating incrementally.
//
// Deprecated: use Claim and Lease for new code.
type ClaimRecord struct {
	TaskRef      string    `json:"task_ref"`
	WorkerID     string    `json:"worker_id"`
	ClaimedAt    time.Time `json:"claimed_at"`
	WorktreePath string    `json:"worktree_path"`
}

// NewInMemoryClaimManager preserves the pre-FAC-120 ClaimManager's
// ergonomics and its exact cross-process limitation (correct only within
// one OS process): swap claim.NewClaimManager() for
// claim.NewInMemoryClaimManager() and ClaimTask/ReleaseClaim call sites
// work unchanged. This is the deliberate migration path for callers not
// ready to adopt SQLiteLeaseStore's cross-process durability — use
// NewClaimManager(NewSQLiteLeaseStore(path)) for that instead.
//
// Deprecated: prefer NewClaimManager(NewSQLiteLeaseStore(path)) for new
// code; it is durable across process restarts and safe across OS
// processes, which this is not.
func NewInMemoryClaimManager(opts ...Option) *ClaimManager {
	return NewClaimManager(NewInMemoryLeaseStore(), opts...)
}

// ClaimTask preserves the pre-FAC-120 ClaimManager.ClaimTask signature and
// first-come-first-served-by-taskRef behavior on top of the new durable,
// role-aware Claim. The second argument is accepted for source
// compatibility and unused, exactly as it was before FAC-120 (callers
// historically passed a TaskProvider). Typed as any so pkg/claim does not
// import pkg/provider — FAC-147's production ProviderCAS lives in
// pkg/provider and must import claim without a cycle.
//
// Always returns ErrLegacyClaimDisabled; use exact canonical ClaimRequest.
//
// Deprecated: migrate to Claim(ctx, ClaimRequest{...}) with a real Role.
func (m *ClaimManager) ClaimTask(ctx context.Context, _ any, taskRef, workerID, worktreePath string) (*ClaimRecord, error) {
	return nil, ErrLegacyClaimDisabled
}

// ReleaseClaim preserves the pre-FAC-120 ClaimManager.ReleaseClaim
// behavior: release whichever claim currently holds taskRef, regardless
// of caller identity (the old map-based implementation had no owner
// fencing, just `delete(activeClaims, taskRef)`). Errors are swallowed,
// matching the old signature, which returned nothing.
//
// Deprecated: migrate to Release(ctx, key, ownerID, generation) error.
func (m *ClaimManager) ReleaseClaim(taskRef string) {
	// The legacy signature has no canonical repository/owner/lane identity;
	// it is intentionally report-only and cannot release a lease.
}

// IsClaimedLegacy preserves the pre-FAC-120 boolean IsClaimed(taskRef)
// check. It could not keep the exact old name: ClaimManager already
// defines IsClaimed(ctx, LeaseKey) (bool, error) as part of the FAC-120
// API, and Go does not allow two methods of the same name with different
// signatures on one type.
//
// Deprecated: migrate to IsClaimed(ctx, key) (bool, error).
func (m *ClaimManager) IsClaimedLegacy(taskRef string) bool {
	claimed, _ := m.IsClaimed(context.Background(), legacyKey(taskRef))
	return claimed
}
