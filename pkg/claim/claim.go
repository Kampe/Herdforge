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
	"strings"
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
// protocol (LeaseStore.ClaimCapacityReleaseExact/AckCapacityRelease/
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
	// release settler for LeaseStore.ClaimCapacityReleaseExact's claim
	// protocol. Unique per instance by default (see NewClaimManager);
	// override with WithSettlerID for deterministic test identities.
	settlerID string
	// capacityClaimTimeout bounds how long a ClaimCapacityReleaseExact claim
	// is honored before another settler may reclaim it, i.e. how long a
	// crashed settler's in-flight release can block recovery.
	capacityClaimTimeout time.Duration
	holdReader           lifecycle.HoldReader
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
// Empty or whitespace-padded identities are rejected before any settlement
// or provider-transition side effect.
func WithSettlerID(id string) Option { return func(m *ClaimManager) { m.settlerID = id } }

// WithCapacityClaimTimeout overrides how long a capacity-release claim is
// honored before another settler may reclaim it (crash recovery bound).
// Default 5m. Non-positive values are rejected before any claim mutation.
func WithCapacityClaimTimeout(d time.Duration) Option {
	return func(m *ClaimManager) { m.capacityClaimTimeout = d }
}

// WithHoldReader injects the canonical durable hold authority.
func WithHoldReader(r lifecycle.HoldReader) Option { return func(m *ClaimManager) { m.holdReader = r } }

// NewClaimManager builds a ClaimManager over store. store's lifetime is
// owned by the caller (Close it when the manager is no longer needed).
func NewClaimManager(store LeaseStore, opts ...Option) *ClaimManager {
	m := &ClaimManager{
		store: store, capacity: noopCapacity{}, outbox: noopOutbox{}, now: time.Now, ttl: 10 * time.Minute,
		capacityClaimTimeout: 5 * time.Minute,
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
// If Acquire silently evicted a stale, expired incumbent, Claim settles its
// capacity before reserving new capacity. If incumbent settlement fails, the
// replacement is atomically aborted as never-reserved and the new claim is
// blocked; only the incumbent remains durable and retryable via
// SettlePendingCapacity or a future Release call.
func (m *ClaimManager) Claim(ctx context.Context, req ClaimRequest) (*Lease, error) {
	if req.TaskRole == "" {
		return nil, ErrUnlabeledTask
	}
	if req.Role == "" || req.Role != req.TaskRole {
		return nil, ErrRoleMismatch
	}
	if m.holdReader == nil {
		return nil, fmt.Errorf("claim: lifecycle hold authority is required")
	}
	identities, err := exactClaimComposite(req)
	if err != nil {
		return nil, err
	}
	fencer, ok := m.holdReader.(interface {
		WithUnheldTransition(context.Context, []lifecycle.HoldIdentity, func() error) error
	})
	if !ok {
		return nil, fmt.Errorf("claim: lifecycle transition fencer is required")
	}
	snap, ok := m.store.(LeaseSnapshotStore)
	if !ok {
		return nil, fmt.Errorf("claim: historical lease snapshot store is required")
	}
	incumbent, err := snap.CurrentLease(ctx, req.Key)
	if err != nil {
		return nil, err
	}
	if incumbent != nil {
		oldIDs, err := recoveryHoldIdentities(incumbent)
		if err != nil {
			return nil, err
		}
		identities = append(identities, oldIDs...)
	}
	identities = uniqueHoldIdentities(identities)
	var lease *Lease
	err = fencer.WithUnheldTransition(ctx, identities, func() error {
		current, err := snap.CurrentLease(ctx, req.Key)
		if err != nil {
			return err
		}
		if incumbent == nil && current != nil {
			return fmt.Errorf("%w: incumbent appeared after snapshot", ErrAlreadyClaimed)
		}
		if incumbent != nil && (current == nil || !sameLeaseImmutable(current, incumbent) || current.Status != incumbent.Status) {
			return fmt.Errorf("%w: incumbent changed after snapshot", ErrStaleGeneration)
		}
		if err := m.recoverStaleProviderLock(ctx, req.Key); err != nil {
			return err
		}
		atomicStore, ok := m.store.(AtomicLeaseStore)
		if !ok {
			return fmt.Errorf("claim: lease store cannot atomically persist canonical hold identity")
		}
		if _, ok := m.store.(UnreservedAbortStore); !ok {
			return fmt.Errorf("claim: lease store cannot atomically abort an unreserved replacement")
		}
		lease, err = atomicStore.AcquireWithIdentity(ctx, req.Key, req.OwnerID, req.Role, req.WorktreePath, identities[0].Repository, identities[0].Owner, identities[0].Lane, m.now(), m.ttl)
		if err != nil {
			return err
		}
		if incumbent != nil && incumbent.ID != lease.ID {
			if _, err := m.settleCapacityExact(ctx, incumbent); err != nil {
				// AcquireWithIdentity has already committed the replacement. Do
				// not strand that active lease without a reservation or claim
				// intent when delivery of the incumbent's release fails.
				abort, ok := m.store.(UnreservedAbortStore)
				if !ok {
					return errors.Join(err, fmt.Errorf("claim: exact unreserved abort store is required"))
				}
				aborted, changed, abortErr := abort.AbortUnreservedLease(ctx, lease, m.now())
				if abortErr == nil && (!changed || aborted == nil || !sameLeaseImmutable(aborted, lease) || aborted.Status != StatusReleased || aborted.CapacityReleaseState != "cancelled" || aborted.ReleasedAt == nil || aborted.CapacityReleasedAt != nil) {
					abortErr = fmt.Errorf("%w: invalid atomic abort receipt", ErrCapacityReleaseStale)
				}
				return errors.Join(err, abortErr)
			}
		}
		if err := m.capacity.Reserve(ctx, req.Role, capacityKey("reserve", lease)); err != nil {
			_, _, releaseErr := m.store.Release(ctx, req.Key, req.OwnerID, lease.Generation, m.now())
			// Reserve errors are ambiguous: an external coordinator may have
			// applied the reservation before returning the error. Keep the
			// released row pending so the stable compensating Release is
			// delivered and retried idempotently.
			return errors.Join(err, releaseErr)
		}
		if err := m.outbox.Record(ctx, OutboxIntent{IdempotencyKey: fmt.Sprintf("claim:%s/%s/%s/%s:g%d", req.Key.Repo, req.Key.Provider, req.Key.Project, req.Key.TaskRef, lease.Generation), Kind: "lease_claimed"}); err != nil {
			_, _, releaseErr := m.store.Release(ctx, req.Key, req.OwnerID, lease.Generation, m.now())
			_, settleErr := m.settleCapacityExact(ctx, lease)
			return errors.Join(err, releaseErr, settleErr)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	return lease, nil
}

func uniqueHoldIdentities(ids []lifecycle.HoldIdentity) []lifecycle.HoldIdentity {
	out := make([]lifecycle.HoldIdentity, 0, len(ids))
	for _, id := range ids {
		seen := false
		for _, prior := range out {
			if prior == id {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, id)
		}
	}
	return out
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
// leaves the token durably pending and this call (or recovery, or
// SettlePendingCapacity) retries it instead of returning nil having
// silently skipped it.
func (m *ClaimManager) Release(ctx context.Context, key LeaseKey, ownerID string, generation int64) error {
	if m.holdReader == nil {
		return fmt.Errorf("claim: lifecycle hold authority is required")
	}
	fencer, ok := m.holdReader.(interface {
		WithUnheldTransition(context.Context, []lifecycle.HoldIdentity, func() error) error
	})
	if !ok {
		return fmt.Errorf("claim: lifecycle transition fencer is required")
	}
	snap, ok := m.store.(LeaseSnapshotStore)
	if !ok {
		return fmt.Errorf("claim: historical lease snapshot store is required")
	}
	incumbent, err := snap.LeaseByGeneration(ctx, key, ownerID, generation)
	if err != nil {
		return err
	}
	if incumbent == nil {
		return fmt.Errorf("%w: release target is not current", ErrStaleGeneration)
	}
	if incumbent.Status != StatusActive && incumbent.Status != StatusReleased {
		return fmt.Errorf("%w: release target is %s", ErrStaleGeneration, incumbent.Status)
	}
	ids, err := recoveryHoldIdentities(incumbent)
	if err != nil {
		return err
	}
	return fencer.WithUnheldTransition(ctx, ids, func() error {
		current, err := snap.LeaseByGeneration(ctx, key, ownerID, generation)
		if err != nil || current == nil {
			if err == nil {
				err = fmt.Errorf("%w: release row disappeared", ErrStaleGeneration)
			}
			return err
		}
		if !sameLeaseImmutable(current, incumbent) {
			return fmt.Errorf("claim: release identity changed inside fence")
		}
		if err := m.recoverStaleProviderLock(ctx, key); err != nil {
			return err
		}
		if current.Status == StatusActive {
			if _, _, err := m.store.Release(ctx, key, ownerID, generation, m.now()); err != nil {
				return err
			}
		}
		if current.CapacityReleasedAt == nil {
			if _, err := m.settleCapacityExact(ctx, current); err != nil {
				return err
			}
		}
		if err := m.outbox.Record(ctx, OutboxIntent{IdempotencyKey: fmt.Sprintf("release:%s/%s/%s/%s:g%d", key.Repo, key.Provider, key.Project, key.TaskRef, generation), Kind: "lease_released"}); err != nil {
			return err
		}
		return nil
	})
}

// Hold sets or clears operator hold on the active lease for key, fenced
// by the caller's ownerID/generation exactly like Release: a stale or
// wrong-owner caller is rejected rather than able to hold/unhold a lease
// it does not currently own.
func (m *ClaimManager) Hold(ctx context.Context, key LeaseKey, ownerID string, generation int64, held bool) (*Lease, error) {
	return nil, ErrLegacyLeaseHoldDisabled
}

// ExpireStale snapshots and fences each expired candidate independently.
// Provider recovery, lease CAS, and exact capacity settlement all occur
// inside that candidate's authority transition. A failure is reported while
// unrelated candidates continue.
func (m *ClaimManager) ExpireStale(ctx context.Context) ([]*Lease, error) {
	if m.holdReader == nil {
		return nil, fmt.Errorf("claim: lifecycle hold authority is required for recovery")
	}
	fencer, fenced := m.holdReader.(interface {
		WithUnheldTransition(context.Context, []lifecycle.HoldIdentity, func() error) error
	})
	if !fenced {
		return nil, fmt.Errorf("claim: hold authority transition fencer is required")
	}
	snapshotNow := m.now()
	recovery, ok := m.store.(RecoveryStore)
	if !ok {
		return nil, fmt.Errorf("claim: per-lease recovery store is required")
	}
	candidates, err := recovery.SnapshotExpiredLeases(ctx, snapshotNow)
	if err != nil {
		return nil, err
	}
	validated := make([][]lifecycle.HoldIdentity, len(candidates))
	for i, candidate := range candidates {
		validated[i], err = recoveryHoldIdentities(candidate)
		if err != nil {
			return nil, err
		}
	}
	var expired []*Lease
	var sweepErr error
	for i, candidate := range candidates {
		identities := validated[i]
		transition := func() error {
			if m.provider != nil {
				stale, staleErr := recovery.ObserveStaleProviderLock(ctx, candidate.LeaseKey, snapshotNow)
				if staleErr != nil {
					return staleErr
				}
				if stale != nil {
					claimed, err := claimRecoveryObservation(ctx, recovery, candidate.LeaseKey, *stale, snapshotNow)
					if err != nil {
						return err
					}
					if err := m.durablyAdvanceFence(ctx, candidate.TaskRef, candidate.Generation+1); err != nil {
						return err
					}
					finalized, err := recovery.FinalizeProviderLockCAS(ctx, *claimed)
					if err != nil {
						return err
					}
					if !finalized {
						return fmt.Errorf("%w: provider recovery finalize lost", ErrProviderLockStale)
					}
				}
			}
			lease, changed, casErr := recovery.ExpireLeaseCAS(ctx, candidate.ID, candidate.Generation, snapshotNow)
			if casErr != nil {
				return casErr
			}
			if changed {
				expired = append(expired, lease)
				if _, err := m.settleCapacityExact(ctx, lease); err != nil {
					return err
				}
			}
			return nil
		}
		if err := fencer.WithUnheldTransition(ctx, identities, transition); err != nil {
			if errors.Is(err, lifecycle.ErrHoldDenied) {
				continue
			}
			sweepErr = errors.Join(sweepErr, err)
			continue
		}
	}
	return expired, sweepErr
}

type capacitySettlementOutcome uint8

const (
	capacitySettlementAlreadySettled capacitySettlementOutcome = iota
	capacitySettlementNewlySettled
)

// settleCapacityExact performs one exact, idempotent capacity settlement.
// The outcome distinguishes a durable Ack performed by this call from a
// replay that observed an already-acked historical row. Callers that report
// newly completed work must use the former only.
func (m *ClaimManager) settleCapacityExact(ctx context.Context, lease *Lease) (capacitySettlementOutcome, error) {
	if err := validateAttributableID(m.settlerID, "settler identity"); err != nil {
		return capacitySettlementAlreadySettled, err
	}
	if err := validatePositiveDuration(m.capacityClaimTimeout, "capacity claim timeout"); err != nil {
		return capacitySettlementAlreadySettled, err
	}
	if lease == nil {
		return capacitySettlementAlreadySettled, fmt.Errorf("%w: nil lease", ErrCapacityReleaseStale)
	}
	if lease.CapacityReleasedAt != nil {
		return capacitySettlementAlreadySettled, nil
	}
	if lease.CapacityReleaseState == "cancelled" {
		return capacitySettlementAlreadySettled, nil
	}
	store, ok := m.store.(ExactCapacityReleaseStore)
	if !ok {
		return capacitySettlementAlreadySettled, fmt.Errorf("claim: exact capacity release store is required")
	}
	claimed, changed, err := store.ClaimCapacityReleaseExact(ctx, lease.ID, lease.Generation, m.settlerID, m.capacityClaimTimeout, m.now())
	if err != nil {
		return capacitySettlementAlreadySettled, err
	}
	if !changed {
		if claimed != nil && claimed.CapacityReleaseState == "cancelled" {
			return capacitySettlementAlreadySettled, nil
		}
		if snap, ok := m.store.(LeaseSnapshotStore); ok {
			current, readErr := snap.LeaseByGeneration(ctx, lease.LeaseKey, lease.OwnerID, lease.Generation)
			if readErr == nil && current != nil && current.CapacityReleasedAt != nil && current.ID == lease.ID {
				return capacitySettlementAlreadySettled, nil
			}
		}
		return capacitySettlementAlreadySettled, fmt.Errorf("%w: lease %d generation %d", ErrCapacityReleaseStale, lease.ID, lease.Generation)
	}
	if claimed == nil || !sameLeaseImmutable(claimed, lease) || (claimed.Status != StatusReleased && claimed.Status != StatusExpired) {
		return capacitySettlementAlreadySettled, fmt.Errorf("%w: claimed capacity row identity mismatch", ErrCapacityReleaseStale)
	}
	if err := m.capacity.Release(ctx, claimed.Role, capacityKey("release", claimed)); err != nil {
		if failErr := m.store.FailCapacityRelease(ctx, claimed.ID, m.settlerID); failErr != nil {
			return capacitySettlementAlreadySettled, errors.Join(err, failErr)
		}
		return capacitySettlementAlreadySettled, err
	}
	if err := m.store.AckCapacityRelease(ctx, claimed.ID, m.settlerID, m.now()); err != nil {
		return capacitySettlementAlreadySettled, err
	}
	return capacitySettlementNewlySettled, nil
}

func validateAttributableID(value, label string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("claim: %s must be nonempty and canonical", label)
	}
	return nil
}

func validatePositiveDuration(value time.Duration, label string) error {
	if value <= 0 {
		return fmt.Errorf("claim: %s must be positive", label)
	}
	return nil
}

func sameLeaseImmutable(a, b *Lease) bool {
	return a != nil && b != nil && a.ID == b.ID && a.LeaseKey == b.LeaseKey && a.OwnerID == b.OwnerID && a.Role == b.Role && a.HoldRepository == b.HoldRepository && a.HoldOwner == b.HoldOwner && a.HoldLane == b.HoldLane && a.WorktreePath == b.WorktreePath && a.Generation == b.Generation && a.ClaimedAt.Equal(b.ClaimedAt)
}

func recoveryHoldIdentities(lease *Lease) ([]lifecycle.HoldIdentity, error) {
	if lease == nil || strings.TrimSpace(lease.Repo) != lease.Repo || strings.TrimSpace(lease.Provider) != lease.Provider || strings.TrimSpace(lease.Project) != lease.Project || strings.TrimSpace(lease.TaskRef) != lease.TaskRef || strings.TrimSpace(lease.HoldRepository) != lease.HoldRepository || strings.TrimSpace(lease.HoldOwner) != lease.HoldOwner || strings.TrimSpace(lease.HoldLane) != lease.HoldLane || strings.TrimSpace(lease.Repo) == "" || strings.TrimSpace(lease.Provider) == "" || strings.TrimSpace(lease.Project) == "" || strings.TrimSpace(lease.TaskRef) == "" || strings.TrimSpace(lease.HoldRepository) == "" || strings.TrimSpace(lease.HoldOwner) == "" || strings.TrimSpace(lease.HoldLane) == "" {
		return nil, fmt.Errorf("claim: expired lease has missing or malformed canonical hold identity")
	}
	if lease.HoldRepository != lease.Repo {
		return nil, fmt.Errorf("claim: hold repository does not match lease repository")
	}
	return []lifecycle.HoldIdentity{
		{Repository: lease.HoldRepository, Owner: lease.HoldOwner, Lane: lease.HoldLane, Scope: "lane"},
		{Repository: lease.HoldRepository, Owner: lease.HoldOwner, Lane: lease.HoldLane, Task: lease.TaskRef, Scope: "task"},
	}, nil
}

func (m *ClaimManager) recoverStaleProviderLock(ctx context.Context, key LeaseKey) error {
	if m.provider == nil {
		return nil
	}
	recovery, ok := m.store.(RecoveryStore)
	if !ok {
		return fmt.Errorf("claim: typed provider recovery store is required")
	}
	obs, err := recovery.ObserveStaleProviderLock(ctx, key, m.now())
	if err != nil || obs == nil {
		return err
	}
	claimed, err := claimRecoveryObservation(ctx, recovery, key, *obs, m.now())
	if err != nil {
		return err
	}
	if err := m.durablyAdvanceFence(ctx, key.TaskRef, claimed.Generation+1); err != nil {
		return err
	}
	finalized, err := recovery.FinalizeProviderLockCAS(ctx, *claimed)
	if err != nil {
		return err
	}
	if !finalized {
		return fmt.Errorf("%w: provider recovery finalize lost", ErrProviderLockStale)
	}
	return nil
}

func claimRecoveryObservation(ctx context.Context, recovery RecoveryStore, key LeaseKey, observed ProviderLockObservation, now time.Time) (*ProviderLockObservation, error) {
	claimed, err := recovery.ClaimProviderLockCAS(ctx, observed)
	if err != nil {
		return nil, err
	}
	current, readErr := recovery.ObserveStaleProviderLock(ctx, key, now)
	if readErr != nil {
		return nil, readErr
	}
	if current == nil || current.LeaseID != observed.LeaseID || current.Generation != observed.Generation || current.Owner != recoveryOwnerFor(observed.LeaseID, observed.Generation) || current.RecoveryOwner != recoveryOwnerFor(observed.LeaseID, observed.Generation) || current.LockedAt.IsZero() || (observed.Recovery && !current.LockedAt.Equal(observed.LockedAt)) {
		if !claimed {
			return nil, fmt.Errorf("%w: provider recovery claim lost", ErrProviderLockStale)
		}
		return nil, fmt.Errorf("%w: post-claim recovery observation missing or mismatched", ErrProviderLockStale)
	}
	return current, nil
}

// SettlePendingCapacity retries CapacityCoordinator.Release for every
// lease across every key whose capacity token has not yet been durably
// marked returned. Exposed standalone (in addition to being called by
// Claim/Release/recovery) so a Reconciler or operator tool can drain a
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
	if err := validateAttributableID(m.settlerID, "settler identity"); err != nil {
		return nil, err
	}
	if err := validatePositiveDuration(m.capacityClaimTimeout, "capacity claim timeout"); err != nil {
		return nil, err
	}
	if m.holdReader == nil {
		return nil, fmt.Errorf("claim: lifecycle hold authority is required for capacity settlement")
	}
	if m.holdReader != nil {
		if key != nil {
			return nil, fmt.Errorf("claim: keyed settlement must use the exact transition path")
		}
		pending, ok := m.store.(PendingCapacityStore)
		if !ok {
			return nil, fmt.Errorf("claim: pending capacity store is required")
		}
		fencer, ok := m.holdReader.(interface {
			WithUnheldTransition(context.Context, []lifecycle.HoldIdentity, func() error) error
		})
		if !ok {
			return nil, fmt.Errorf("claim: lifecycle transition fencer is required")
		}
		leases, err := pending.PendingCapacityReleases(ctx)
		if err != nil {
			return nil, err
		}
		var settled []*Lease
		var firstErr error
		for _, lease := range leases {
			ids, err := recoveryHoldIdentities(lease)
			if err != nil {
				firstErr = errors.Join(firstErr, err)
				continue
			}
			err = fencer.WithUnheldTransition(ctx, ids, func() error {
				outcome, err := m.settleCapacityExact(ctx, lease)
				if err != nil {
					return err
				}
				if outcome == capacitySettlementNewlySettled {
					settled = append(settled, lease)
				}
				return nil
			})
			if errors.Is(err, lifecycle.ErrHoldDenied) {
				continue
			}
			if err != nil {
				firstErr = errors.Join(firstErr, err)
			}
		}
		return settled, firstErr
	}
	return nil, fmt.Errorf("claim: exact fenced capacity settlement path unavailable")
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
