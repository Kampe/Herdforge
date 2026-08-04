package claim

import (
	"context"
	"errors"
	"fmt"
)

// ErrProviderRevisionStale means ProviderCAS.CompareAndSwap observed a
// current revision different from what the caller expected -- something
// else changed the task since it was last read.
var ErrProviderRevisionStale = errors.New("claim: provider revision is stale")

// ErrProviderNotConfigured / ErrOutboxNotConfigured are returned by the
// provider-transition methods when the corresponding WithProviderCAS /
// WithDurableOutbox option was never supplied.
var (
	ErrProviderNotConfigured = errors.New("claim: no ProviderCAS configured (see WithProviderCAS)")
	ErrOutboxNotConfigured   = errors.New("claim: no DurableOutbox configured (see WithDurableOutbox)")
)

// ErrLeaseNotCurrent is returned by BeginProviderTransition/
// CompleteProviderTransition when the caller's (key, ownerID, generation)
// does not match the currently active lease: no active lease at all
// (released, or expired and not yet reclaimed), a wrong owner, or a
// generation superseded by a reclaim. A provider mutation must never be
// attempted on behalf of a lease the caller no longer (or never did)
// hold.
var ErrLeaseNotCurrent = errors.New("claim: key/owner/generation is not the current active lease")

// durablyAdvanceFence enqueues a durable record of the intent to advance
// taskID's provider fence to fenceToken (idempotency key "fence:<taskID>:
// g<fenceToken>"), then attempts it, and returns any failure -- never
// swallowed -- so the caller (Claim, before allowing a reclaim to
// supersede a stale provider-locked lease) can refuse to proceed rather
// than expose new local ownership without the provider having been told.
//
// Unlike CompleteProviderTransition's provider_mutation intents, this
// needs no exclusive claim/lock step: AdvanceFence has no side effect
// beyond bumping a monotonic floor, so calling it concurrently or
// redundantly from multiple settlers is safe by construction (the
// highest value wins regardless of call order). The durable record exists
// so a FAILED attempt is not lost: it stays Pending, and the next Claim
// call for the same key (the natural retry path -- a daemon loop, an
// operator retry, whatever keeps calling Claim for pending work) replays
// the same idempotency key and tries again, until it succeeds. Once
// Applied, replays short-circuit without calling the provider again.
func (m *ClaimManager) durablyAdvanceFence(ctx context.Context, taskID string, fenceToken int64) error {
	if m.outboxStore == nil {
		// No durable outbox configured: nowhere to durably record the
		// intent, but the failure is still surfaced to the caller, never
		// swallowed -- degraded (no cross-process retry tracking), not
		// silently unsafe.
		return m.provider.AdvanceFence(ctx, taskID, fenceToken)
	}

	idempotencyKey := fmt.Sprintf("fence:%s:g%d", taskID, fenceToken)
	rec, err := m.outboxStore.Enqueue(ctx, OutboxIntent{IdempotencyKey: idempotencyKey, Kind: "fence_advance"})
	if err != nil {
		return fmt.Errorf("claim: durably record fence advance intent: %w", err)
	}
	if rec.Status == OutboxApplied {
		return nil // already durably advanced by a prior successful attempt.
	}
	if err := m.provider.AdvanceFence(ctx, taskID, fenceToken); err != nil {
		return fmt.Errorf("claim: advance provider fence for %s to generation %d: %w", taskID, fenceToken, err)
	}
	if err := m.outboxStore.ForceMarkApplied(ctx, idempotencyKey, m.now()); err != nil {
		return fmt.Errorf("claim: mark fence advance applied: %w", err)
	}
	return nil
}

// verifyCurrentLease fences a provider-transition attempt against live
// lease state: only the exact current owner at the exact current
// generation may enqueue or complete a provider mutation for key. Reads
// via ActiveClaims, so it naturally rejects released and (once expired
// and evicted by an Acquire/ExpireStale) expired leases the same way it
// rejects a wrong owner or a stale generation -- all of those simply
// mean "the given identity is not in the active-claims set right now".
func (m *ClaimManager) verifyCurrentLease(ctx context.Context, key LeaseKey, ownerID string, generation int64) error {
	claims, err := m.store.ActiveClaims(ctx, m.now())
	if err != nil {
		return fmt.Errorf("claim: verify current lease: %w", err)
	}
	for _, l := range claims {
		if l.LeaseKey != key {
			continue
		}
		if l.OwnerID != ownerID {
			return fmt.Errorf("%w: %s is held by %s, not %s", ErrLeaseNotCurrent, key.TaskRef, l.OwnerID, ownerID)
		}
		if l.Generation != generation {
			return fmt.Errorf("%w: %s active generation is %d, caller had %d", ErrLeaseNotCurrent, key.TaskRef, l.Generation, generation)
		}
		return nil
	}
	return fmt.Errorf("%w: no active lease for %s", ErrLeaseNotCurrent, key.TaskRef)
}

// ProviderTransitionVerifier checks whether the provider mutation an
// OutboxRecord represents has already taken effect, so
// ReconcileProviderTransitions can close out a record left ambiguous by
// a crash (the provider mutation succeeded, but the local outbox mark
// never landed) without needing to persist and safely replay an
// arbitrary mutate closure -- only the caller knows how to verify its
// own mutation's target state in the provider.
type ProviderTransitionVerifier func(ctx context.Context, rec *OutboxRecord) (applied bool, err error)

// WithProviderCAS wires a revision-fenced provider board mutator into
// BeginProviderTransition/CompleteProviderTransition. Not configuring one
// makes those methods return ErrProviderNotConfigured; nothing in this
// repo implements ProviderCAS yet (see its doc comment in integration.go).
func WithProviderCAS(p ProviderCAS) Option { return func(m *ClaimManager) { m.provider = p } }

// WithDurableOutbox wires the durable outbox provider-transition intents
// are recorded in before being attempted. Not configuring one makes
// BeginProviderTransition/CompleteProviderTransition/
// ReconcileProviderTransitions return ErrOutboxNotConfigured.
func WithDurableOutbox(o DurableOutbox) Option { return func(m *ClaimManager) { m.outboxStore = o } }

func providerIntentKey(key LeaseKey, generation int64) string {
	return fmt.Sprintf("provider:%s/%s/%s/%s:g%d", key.Repo, key.Provider, key.Project, key.TaskRef, generation)
}

// BeginProviderTransition durably records the intent to mutate the
// provider board for an already-claimed lease, before attempting the
// mutation. ownerID/generation must match the CURRENT active lease for
// key exactly -- a released, expired-and-reclaimed (superseded
// generation), wrong-owner, or entirely absent lease is rejected with
// ErrLeaseNotCurrent and never reaches the outbox, so a stale caller can
// never even record intent to mutate the provider, let alone attempt it.
//
// Idempotent for a valid caller: calling it again for the same
// lease/generation returns the existing record unchanged -- a retry
// after a crash before the mutation was ever attempted just re-reads the
// same pending intent instead of creating a duplicate.
func (m *ClaimManager) BeginProviderTransition(ctx context.Context, key LeaseKey, ownerID string, generation int64, kind string) (*OutboxRecord, error) {
	if m.outboxStore == nil {
		return nil, ErrOutboxNotConfigured
	}
	if err := m.verifyCurrentLease(ctx, key, ownerID, generation); err != nil {
		return nil, err
	}
	return m.outboxStore.Enqueue(ctx, OutboxIntent{IdempotencyKey: providerIntentKey(key, generation), Kind: kind})
}

// CompleteProviderTransition attempts the provider mutation for an
// already-begun intent (see BeginProviderTransition), fenced by
// expectedRevision via ProviderCAS. Safe to call more than once for the
// same idempotencyKey, including concurrently from different
// ClaimManagers: it claims the intent first using the same atomic-claim
// primitive as capacity-release settlement (DurableOutbox.Claim), so two
// concurrent callers can never both be mid-CompareAndSwap for the same
// intent -- one gets the claim and proceeds, the other sees rec == nil
// and just returns the current record. A crash between CompareAndSwap
// succeeding and MarkApplied leaves the claim to go stale for a later
// caller to reclaim; ReconcileProviderTransitions closes that window
// without re-invoking CompareAndSwap.
//
// A stale expectedRevision (or any other CompareAndSwap failure) marks
// the record Failed with the error recorded, and does NOT touch the
// underlying lease -- the caller still holds it and may retry with a
// freshly-read revision.
//
// ownerID/generation are checked twice: verifyCurrentLease is a cheap
// read-only fast-fail before the outbox is even touched, then
// LeaseStore.AcquireProviderLock verifies AND locks the lease against
// Release/reclaim in a single atomic store operation, held for the
// ProviderCAS call's duration via a deferred ReleaseProviderLock.
//
// IMPORTANT: the lock alone is a LIVENESS mechanism, not a safety proof.
// It stops Release and a live reclaim attempt from racing a live
// in-flight call, and it stops wasteful concurrent CompareAndSwap
// attempts -- but its staleness timeout (crash recovery, so a dead
// settler cannot block a lease forever) is explicitly NOT trusted to
// prove the old call has actually stopped: a settler that looks crashed
// may still have a request in flight to the provider. Safety against
// THAT case -- a stale call from a superseded generation eventually
// landing -- comes from generation as an unforgeable fencing token
// passed to ProviderCAS.CompareAndSwap, which conforming implementations
// must durably reject once superseded (see ProviderCAS's doc comment),
// combined with ClaimManager.Claim calling AdvanceFence on every reclaim
// so the provider knows a generation is superseded even if the new
// owner never itself calls CompareAndSwap. Either check failing before
// this point returns ErrLeaseNotCurrent and guarantees zero ProviderCAS
// calls; a lock-acquisition failure also marks the outbox record Failed
// so it does not sit claimed and orphaned.
func (m *ClaimManager) CompleteProviderTransition(ctx context.Context, key LeaseKey, ownerID string, generation int64, taskID string, expectedRevision ProviderRevision, mutate func(ctx context.Context) error) (*OutboxRecord, error) {
	if err := validateAttributableID(m.settlerID, "settler identity"); err != nil {
		return nil, err
	}
	if err := validatePositiveDuration(m.capacityClaimTimeout, "capacity claim timeout"); err != nil {
		return nil, err
	}
	if m.provider == nil {
		return nil, ErrProviderNotConfigured
	}
	if m.outboxStore == nil {
		return nil, ErrOutboxNotConfigured
	}
	if err := m.verifyCurrentLease(ctx, key, ownerID, generation); err != nil {
		return nil, err
	}

	idempotencyKey := providerIntentKey(key, generation)
	rec, err := m.outboxStore.Claim(ctx, idempotencyKey, m.settlerID, m.capacityClaimTimeout, m.now())
	if err != nil {
		return nil, fmt.Errorf("claim: claim provider transition: %w", err)
	}
	if rec == nil {
		// Not ours to attempt right now (already Applied, or another
		// settler currently owns the claim) -- report current state.
		return m.outboxStore.Get(ctx, idempotencyKey)
	}

	if _, err := m.store.AcquireProviderLock(ctx, key, ownerID, generation, m.settlerID, providerLockStaleAfter, m.now()); err != nil {
		markErr := m.outboxStore.MarkFailed(ctx, idempotencyKey, m.settlerID, err.Error(), m.now())
		return nil, errors.Join(fmt.Errorf("%w: %w", ErrLeaseNotCurrent, err), markErr)
	}
	release := func(rec *OutboxRecord, primary error) (*OutboxRecord, error) {
		return rec, errors.Join(primary, m.store.ReleaseProviderLock(ctx, key, generation, m.settlerID))
	}

	if completeProviderTransitionTestHook != nil {
		// Fires with the lock already held, immediately before the
		// external call -- exactly the window the review's probe
		// exploited. A test-injected Release/reclaim attempted here must
		// fail (blocked by the lock), proving mutual exclusion actually
		// prevents the race rather than merely re-checking after it.
		completeProviderTransitionTestHook()
	}

	if _, casErr := m.provider.CompareAndSwap(ctx, taskID, expectedRevision, generation, mutate); casErr != nil {
		markErr := m.outboxStore.MarkFailed(ctx, idempotencyKey, m.settlerID, casErr.Error(), m.now())
		rec, getErr := m.outboxStore.Get(ctx, idempotencyKey)
		return release(rec, errors.Join(casErr, markErr, getErr))
	}
	if err := m.outboxStore.MarkApplied(ctx, idempotencyKey, m.settlerID, m.now()); err != nil {
		rec, getErr := m.outboxStore.Get(ctx, idempotencyKey)
		return release(rec, errors.Join(fmt.Errorf("claim: mark provider transition applied: %w", err), getErr))
	}
	rec, getErr := m.outboxStore.Get(ctx, idempotencyKey)
	return release(rec, getErr)
}

// completeProviderTransitionTestHook, when non-nil, runs once per
// CompleteProviderTransition call, with the provider-transition lock
// already held, immediately before the ProviderCAS call -- letting a
// test deterministically attempt a concurrent Release/reclaim in exactly
// the window the review's probe exploited, and assert that attempt fails
// (blocked by the lock) rather than merely getting caught by a second
// read that runs too late. Always nil outside tests.
var completeProviderTransitionTestHook func()

// ReconcileProviderTransitions scans every non-Applied provider_mutation
// outbox record and asks verify whether the provider mutation it
// represents has already taken effect.
//
//   - verify == true: the record is closed via ForceMarkApplied WITHOUT
//     re-invoking CompareAndSwap and WITHOUT touching the underlying
//     lease. This is what closes the provider-success/local-failure gap
//     (the mutation landed in the provider, but the process crashed
//     before recording that locally) -- no double-claim, no
//     double-release, just a local bookkeeping correction against
//     independently-verified ground truth.
//   - verify == false: the record is left as Pending/Failed/stale-claimed
//     for a future CompleteProviderTransition retry (with the real
//     mutate closure, which only the driving caller has -- reconciliation
//     cannot fabricate one). This surfaces the local-success/
//     provider-failure backlog (an intent whose provider mutation never
//     actually landed) via stillPending rather than silently dropping it.
func (m *ClaimManager) ReconcileProviderTransitions(ctx context.Context, verify ProviderTransitionVerifier) (closed, stillPending int, err error) {
	if m.outboxStore == nil {
		return 0, 0, ErrOutboxNotConfigured
	}
	pending, err := m.outboxStore.Pending(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("claim: list pending provider transitions: %w", err)
	}

	var firstErr error
	for _, rec := range pending {
		if rec.Kind != "provider_mutation" {
			continue
		}
		applied, verr := verify(ctx, rec)
		if verr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim: verify provider transition %s: %w", rec.IdempotencyKey, verr)
			}
			stillPending++
			continue
		}
		if !applied {
			stillPending++
			continue
		}
		if err := m.outboxStore.ForceMarkApplied(ctx, rec.IdempotencyKey, m.now()); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("claim: force mark applied %s: %w", rec.IdempotencyKey, err)
			}
			stillPending++
			continue
		}
		closed++
	}
	return closed, stillPending, firstErr
}
