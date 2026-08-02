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
// provider board for an already-claimed lease (key/generation), before
// attempting the mutation. Idempotent: calling it again for the same
// lease/generation returns the existing record unchanged -- a retry
// after a crash before the mutation was ever attempted just re-reads the
// same pending intent instead of creating a duplicate.
func (m *ClaimManager) BeginProviderTransition(ctx context.Context, key LeaseKey, generation int64, kind string) (*OutboxRecord, error) {
	if m.outboxStore == nil {
		return nil, ErrOutboxNotConfigured
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
func (m *ClaimManager) CompleteProviderTransition(ctx context.Context, idempotencyKey, taskID string, expectedRevision ProviderRevision, mutate func(ctx context.Context) error) (*OutboxRecord, error) {
	if m.provider == nil {
		return nil, ErrProviderNotConfigured
	}
	if m.outboxStore == nil {
		return nil, ErrOutboxNotConfigured
	}

	rec, err := m.outboxStore.Claim(ctx, idempotencyKey, m.settlerID, m.capacityClaimTimeout, m.now())
	if err != nil {
		return nil, fmt.Errorf("claim: claim provider transition: %w", err)
	}
	if rec == nil {
		// Not ours to attempt right now (already Applied, or another
		// settler currently owns the claim) -- report current state.
		return m.outboxStore.Get(ctx, idempotencyKey)
	}

	if _, casErr := m.provider.CompareAndSwap(ctx, taskID, expectedRevision, mutate); casErr != nil {
		_ = m.outboxStore.MarkFailed(ctx, idempotencyKey, m.settlerID, casErr.Error(), m.now())
		return m.outboxStore.Get(ctx, idempotencyKey)
	}
	if err := m.outboxStore.MarkApplied(ctx, idempotencyKey, m.settlerID, m.now()); err != nil {
		return nil, fmt.Errorf("claim: mark provider transition applied: %w", err)
	}
	return m.outboxStore.Get(ctx, idempotencyKey)
}

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
