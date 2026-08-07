package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// AuthoritativeReceiver is the Kaneo-compatible fence/op acceptance
// boundary (FAC-147). Upstream Kaneo CLI/API do not natively fence; every
// production mutate must pass through Execute, which:
//
//  1. holds per-task exclusive lock
//  2. short-circuits pure applied retries
//  3. reconciles in_progress/ambiguous ops via effectMet (no blind re-mutate)
//  4. durably records in_progress BEFORE the remote backend runs
//  5. runs backend, then MarkApplied with revision evidence when available
//
// A crash after remote success and before MarkApplied leaves in_progress;
// restart effectMet for the SAME op sees the board effect and commits
// applied without a second backend call — closing provider-success/local-failure.
//
// If no receiver is attached, production Kaneo must fail closed.
type AuthoritativeReceiver interface {
	// Execute is the sole mutate entrypoint. backend is the remote
	// Kaneo CLI/HTTP call. effectMet is LIVE provider readback only
	// (never local receipt substitution): Present / Absent / Unknown.
	Execute(
		ctx context.Context,
		taskID string,
		fenceToken int64,
		opID string,
		expStatus, expComment string,
		backend func(ctx context.Context) error,
		effectMet func(ctx context.Context) (EffectState, error),
	) error
}

// AuthBroker is the production authoritative receiver over FenceStore.
type AuthBroker struct {
	store FenceStore
	// CrashAt is an optional injected test seam (never ambient env).
	// Production is always nil. Test builds install via claim.TestSeams.
	// Values: "before-remote" | "after-remote". May os.Exit / panic.
	// after-remote fires AFTER remote backend, BEFORE local revision persist.
	CrashAt func(phase string)
	// RevisionOf optional live revision after a successful backend write
	// (e.g. task.UpdatedAt). Bound into the applied receipt for evidence.
	RevisionOf func(ctx context.Context, taskID string) (string, error)
	// StatusOpEvidence is LIVE provider readback of op-bound status evidence
	// for THIS op. Client HMAC/description receipts are NOT valid production
	// evidence (audit con62fkm). Prefer ServerOpDedupe re-submit when the
	// remote atomically dedupes op IDs with status.
	StatusOpEvidence func(ctx context.Context, taskID, opID, expStatus string) (bool, error)
	// ServerOpDedupe when true: empty-rev Present without StatusOpEvidence
	// re-submits the same op (server must no-op if already applied) instead
	// of fail-closed settle or forgeable client receipts.
	ServerOpDedupe bool
}

// NewAuthBroker builds a broker over the shared ClaimStack FenceStore.
func NewAuthBroker(store FenceStore) *AuthBroker {
	b := &AuthBroker{store: store}
	if s := claim.CurrentTestSeams(); s != nil && s.CrashAt != nil {
		b.CrashAt = s.CrashAt
	}
	return b
}

// BindRevisionReader wires live task revision into applied receipts.
// Required for status mutations so ambiguous recovery can bind exact evidence.
// Uses EncodeRevision so AuthBroker and FencedCAS share one revision format.
func (b *AuthBroker) BindRevisionReader(get func(ctx context.Context, taskID string) (*Task, error)) *AuthBroker {
	if b == nil || get == nil {
		return b
	}
	b.RevisionOf = func(ctx context.Context, taskID string) (string, error) {
		t, err := get(ctx, taskID)
		if err != nil || t == nil {
			return "", err
		}
		rev := string(EncodeRevision(t))
		if rev == "" || strings.HasSuffix(rev, "||") {
			return "", fmt.Errorf("empty revision for task %s", taskID)
		}
		return rev, nil
	}
	return b
}

// NewLocalAuthReceiver keeps the prior name as an alias for tests.
func NewLocalAuthReceiver(store FenceStore) *AuthBroker {
	return NewAuthBroker(store)
}

func (b *AuthBroker) Execute(
	ctx context.Context,
	taskID string,
	fenceToken int64,
	opID string,
	expStatus, expComment string,
	backend func(ctx context.Context) error,
	effectMet func(ctx context.Context) (EffectState, error),
) error {
	if b == nil || b.store == nil {
		return fmt.Errorf("auth broker: nil store (authoritative receiver required)")
	}
	if opID == "" {
		return fmt.Errorf("auth broker: opID required")
	}
	if backend == nil {
		return fmt.Errorf("auth broker: backend required")
	}
	run := func(ctx context.Context) error {
		return b.executeLocked(ctx, taskID, fenceToken, opID, expStatus, expComment, backend, effectMet)
	}
	// Avoid re-entrant WithExclusive deadlock when FencedCAS already holds the lock.
	if exclusiveHeldTask(ctx) == taskID {
		return run(ctx)
	}
	return b.store.WithExclusive(ctx, taskID, run)
}

// EffectState is live-provider readback for recovery (FAC-147 hold).
// UNKNOWN must never be treated as ABSENT (would re-mutate blindly).
type EffectState int

const (
	// EffectAbsent: live read succeeded and the expected effect is not present.
	EffectAbsent EffectState = iota
	// EffectPresent: live read succeeded and the expected effect is present
	// for THIS op (status+revision or op-tagged comment).
	EffectPresent
	// EffectUnknown: live read failed/unavailable — stay ambiguous, never re-mutate.
	EffectUnknown
)

func (b *AuthBroker) executeLocked(
	ctx context.Context,
	taskID string,
	fenceToken int64,
	opID string,
	expStatus, expComment string,
	backend func(ctx context.Context) error,
	effectMet func(ctx context.Context) (EffectState, error),
) error {
	// Newer fence ALWAYS dominates — check high-water before any recovery
	// re-mutate of an old ambiguous receipt (gen2-preempts-gen1 / 6min race).
	high, err := b.store.Highest(ctx, taskID)
	if err != nil {
		return err
	}
	if fenceToken < high {
		return fmt.Errorf("%w: fence token %d is behind %d for %s (stale ambiguous replay refused)",
			claim.ErrProviderFenceRejected, fenceToken, high, taskID)
	}

	rec, err := b.store.LookupApplied(ctx, opID)
	if err != nil {
		return err
	}
	if rec != nil {
		if rec.TaskID != taskID || rec.FenceToken != fenceToken {
			return fmt.Errorf("%w: op %s bound to task=%s fence=%d, request task=%s fence=%d",
				claim.ErrProviderFenceRejected, opID, rec.TaskID, rec.FenceToken, taskID, fenceToken)
		}
		if !rec.Ambiguous {
			return nil // pure applied retry — never re-invoke backend
		}
		// in_progress / ambiguous: LIVE effectMet only — never local receipt.
		// effectMet must bind to this op (comment op tag / status match for same op).
		if effectMet == nil {
			return fmt.Errorf("%w: ambiguous op %s has no live effectMet", claim.ErrProviderAmbiguous, opID)
		}
		state, eerr := effectMet(ctx)
		if eerr != nil {
			return fmt.Errorf("%w: effect reconcile: %v", claim.ErrProviderAmbiguous, eerr)
		}
		switch state {
		case EffectPresent:
			// Status recovery across true remote-success/pre-local-persist window:
			// empty local revision + Present is only settled when provider-bound
			// status op evidence exists for THIS opID (not bare status match).
			if expComment == "" && rec.ExpectedComment == "" {
				liveRev, rerr := b.revisionStrict(ctx, taskID)
				if rerr != nil {
					return fmt.Errorf("%w: ambiguous status recovery revision: %v", claim.ErrProviderAmbiguous, rerr)
				}
				if rec.Revision != "" {
					if liveRev != rec.Revision {
						return fmt.Errorf("%w: ambiguous op %s revision mismatch want=%q live=%q",
							claim.ErrProviderAmbiguous, opID, rec.Revision, liveRev)
					}
				} else if rec.BaseRevision != "" && liveRev == rec.BaseRevision {
					// No board advance — crash-before-remote with pre-set status.
					break
				} else {
					// Empty post-remote rev: require provider-bound status evidence.
					if b.StatusOpEvidence == nil {
						return fmt.Errorf("%w: refuse empty-rev Present for op %s without StatusOpEvidence probe (base=%q live=%q)",
							claim.ErrProviderAmbiguous, opID, rec.BaseRevision, liveRev)
					}
					has, herr := b.StatusOpEvidence(ctx, taskID, opID, expStatus)
					if herr != nil {
						return fmt.Errorf("%w: status op evidence: %v", claim.ErrProviderAmbiguous, herr)
					}
					if !has {
						if b.ServerOpDedupe {
							// Server enforces op-id dedupe with status: re-submit
							// same op rather than trust bare status or client HMAC.
							break
						}
						// Direct client / other host advanced status without our op.
						return fmt.Errorf("%w: refuse empty-rev Present for op %s — no provider-bound status evidence (competing same-status)",
							claim.ErrProviderAmbiguous, opID)
					}
					// Exactly-once: evidence proves our remote succeeded; bind live rev.
					rec.Revision = liveRev
				}
			}
			rec.Ambiguous = false
			if rec.ExpectedStatus == "" {
				rec.ExpectedStatus = expStatus
			}
			if rec.ExpectedComment == "" {
				rec.ExpectedComment = expComment
			}
			if err := b.store.MarkApplied(ctx, *rec); err != nil {
				return err
			}
			return nil
		case EffectUnknown:
			// Timeout/unavailable live state: stay ambiguous forever, no backend.
			return fmt.Errorf("%w: live effect UNKNOWN for op %s (refuse re-mutate)", claim.ErrProviderAmbiguous, opID)
		case EffectAbsent:
			// Live list/status succeeded and effect is gone. Under a still-
			// current fence this is crash-before-remote recovery: re-attempt
			// once below. Stale fence already rejected above.
		default:
			return fmt.Errorf("%w: unknown effect state %d", claim.ErrProviderAmbiguous, state)
		}
	} else {
		// New op: advance high-water if needed, then durable in_progress BEFORE remote.
		// Capture BaseRevision so recovery can detect revision advance (pt5t7 #3).
		if fenceToken > high {
			if _, err := b.store.Advance(ctx, taskID, fenceToken); err != nil {
				return err
			}
		}
		baseRev := ""
		if expStatus != "" {
			// Pre-mutate revision is authoritative evidence — fail closed.
			r, rerr := b.revisionStrict(ctx, taskID)
			if rerr != nil {
				return fmt.Errorf("%w: pre-mutate base revision: %v", claim.ErrProviderAmbiguous, rerr)
			}
			baseRev = r
		}
		pending := OpReceipt{
			OpID:            opID,
			TaskID:          taskID,
			FenceToken:      fenceToken,
			BaseRevision:    baseRev,
			ExpectedStatus:  expStatus,
			ExpectedComment: expComment,
			Ambiguous:       true,
		}
		if err := b.store.MarkAmbiguous(ctx, pending); err != nil {
			return fmt.Errorf("auth broker: mark in_progress before remote: %w", err)
		}
		if b.CrashAt != nil {
			b.CrashAt("before-remote")
		}
	}

	if err := backend(ctx); err != nil {
		return err
	}
	// TRUE provider-success / pre-local-persist window: remote (incl. op-bound
	// status evidence) has committed; local revision/applied not yet durable.
	if b.CrashAt != nil {
		b.CrashAt("after-remote")
	}
	// Capture revision AFTER remote onto in_progress, then MarkApplied.
	rev := ""
	baseRev := ""
	rec, lerr := b.store.LookupApplied(ctx, opID)
	if lerr != nil {
		return fmt.Errorf("%w: post-remote LookupApplied: %v", claim.ErrProviderAmbiguous, lerr)
	}
	if rec != nil {
		baseRev = rec.BaseRevision
	}
	if expStatus != "" {
		var rerr error
		rev, rerr = b.revisionStrict(ctx, taskID)
		if rerr != nil || rev == "" {
			return fmt.Errorf("%w: post-remote revision evidence unavailable for op %s: %v",
				claim.ErrProviderAmbiguous, opID, rerr)
		}
		withRev := OpReceipt{
			OpID:            opID,
			TaskID:          taskID,
			FenceToken:      fenceToken,
			BaseRevision:    baseRev,
			ExpectedStatus:  expStatus,
			ExpectedComment: expComment,
			Revision:        rev,
			Ambiguous:       true,
		}
		if err := b.store.MarkAmbiguous(ctx, withRev); err != nil {
			return fmt.Errorf("%w: persist revision after remote: %v", claim.ErrProviderAmbiguous, err)
		}
	}
	applied := OpReceipt{
		OpID:            opID,
		TaskID:          taskID,
		FenceToken:      fenceToken,
		BaseRevision:    baseRev,
		ExpectedStatus:  expStatus,
		ExpectedComment: expComment,
		Revision:        rev,
	}
	if err := b.store.MarkApplied(ctx, applied); err != nil {
		return fmt.Errorf("%w: auth broker MarkApplied after remote: %v", claim.ErrProviderAmbiguous, err)
	}
	return nil
}

// revisionStrict returns live revision or an error (never silent empty).
func (b *AuthBroker) revisionStrict(ctx context.Context, taskID string) (string, error) {
	if b == nil || b.RevisionOf == nil {
		return "", fmt.Errorf("auth broker: RevisionOf not configured")
	}
	r, err := b.RevisionOf(ctx, taskID)
	if err != nil {
		return "", err
	}
	if r == "" {
		return "", fmt.Errorf("empty live revision for task %s", taskID)
	}
	return r, nil
}

// AttachAuthoritativeReceiver wires AuthBroker + RequireCASMeta on Kaneo.
func AttachAuthoritativeReceiver(tp TaskProvider, store FenceStore) {
	if tp == nil || store == nil {
		return
	}
	switch t := tp.(type) {
	case *KaneoProvider:
		broker := NewAuthBroker(store)
		broker.BindRevisionReader(t.GetTask)
		// Server-native op evidence via FenceBroker when configured.
		// Re-submit (ServerOpDedupe) only when broker can dedupe the same op.
		broker.ServerOpDedupe = t.FenceBroker != nil || t.AtomicFenceServer
		broker.StatusOpEvidence = func(ctx context.Context, taskID, opID, expStatus string) (bool, error) {
			if t.FenceBroker == nil {
				return false, nil
			}
			return t.FenceBroker.OpApplied(ctx, opID, taskID, expStatus)
		}
		t.Receiver = broker
		t.RequireCASMeta = true
	case *BoundClient:
		AttachAuthoritativeReceiver(t.Inner, store)
	}
}

// UnwrapTaskProvider returns the innermost non-BoundClient provider.
func UnwrapTaskProvider(tp TaskProvider) TaskProvider {
	for {
		b, ok := tp.(*BoundClient)
		if !ok || b == nil || b.Inner == nil {
			return tp
		}
		tp = b.Inner
	}
}
