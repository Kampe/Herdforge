package provider

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// newOpUUID mints an immutable logical operation id. Status/body-hash
// kinds alone are not unique: two legitimate same-state transitions in
// one generation must not falsely dedupe at the authoritative broker.
func newOpUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("op-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// FencedBoard is the production consumer of FencedCAS: lease-guarded
// board mutations go through Begin/Complete + CompareAndSwap with a
// stable per-operation idempotency key (FAC-147).
type FencedBoard struct {
	CAS *FencedCAS
	TP  TaskProvider
}

func NewFencedBoard(cas *FencedCAS, tp TaskProvider) (*FencedBoard, error) {
	if cas == nil {
		return nil, fmt.Errorf("provider: NewFencedBoard requires FencedCAS")
	}
	if tp == nil {
		return nil, fmt.Errorf("provider: NewFencedBoard requires TaskProvider")
	}
	return &FencedBoard{CAS: cas, TP: tp}, nil
}

// UpdateStatus is a CAS-only helper for tests. Production status writes
// use MutateStatus (Begin/Complete). Each call mints a fresh op UUID so
// two same-state writes are distinct logical operations at the broker.
func (b *FencedBoard) UpdateStatus(ctx context.Context, taskID string, fenceToken int64, status string) (claim.ProviderRevision, error) {
	if b == nil {
		return "", fmt.Errorf("provider: nil FencedBoard")
	}
	expected, err := b.CAS.ReadRevision(ctx, taskID)
	if err != nil {
		return "", err
	}
	ctx = WithCASExpectation(ctx, status, "")
	opID := "status:" + newOpUUID()
	return b.CAS.CompareAndSwap(ctx, taskID, expected, fenceToken, opID, func(ctx context.Context) error {
		return b.TP.UpdateStatus(ctx, taskID, status)
	})
}

// ClaimTask is CAS-only helper; production uses MutateClaim.
func (b *FencedBoard) ClaimTask(ctx context.Context, taskID string, fenceToken int64, role string) (claim.ProviderRevision, error) {
	if b == nil {
		return "", fmt.Errorf("provider: nil FencedBoard")
	}
	expected, err := b.CAS.ReadRevision(ctx, taskID)
	if err != nil {
		return "", err
	}
	ctx = WithCASExpectation(ctx, StatusInProgress, "")
	opID := "claim:" + newOpUUID()
	return b.CAS.CompareAndSwap(ctx, taskID, expected, fenceToken, opID, func(ctx context.Context) error {
		return b.TP.ClaimTask(ctx, taskID, role)
	})
}

// AddComment is CAS-only helper; production uses MutateComment.
func (b *FencedBoard) AddComment(ctx context.Context, taskID string, fenceToken int64, body string) (claim.ProviderRevision, error) {
	if b == nil {
		return "", fmt.Errorf("provider: nil FencedBoard")
	}
	expected, err := b.CAS.ReadRevision(ctx, taskID)
	if err != nil {
		return "", err
	}
	ctx = WithCASExpectation(ctx, "", body)
	opID := "comment:" + newOpUUID()
	return b.CAS.CompareAndSwap(ctx, taskID, expected, fenceToken, opID, func(ctx context.Context) error {
		return b.TP.AddComment(ctx, taskID, body)
	})
}

func (b *FencedBoard) AdvanceFence(ctx context.Context, taskID string, fenceToken int64) error {
	if b == nil || b.CAS == nil {
		return fmt.Errorf("provider: nil FencedBoard")
	}
	return b.CAS.AdvanceFence(ctx, taskID, fenceToken)
}

func OpenFencedBoard(fenceDBPath string, tp TaskProvider) (*FencedBoard, error) {
	cas, err := OpenFencedCAS(fenceDBPath, tp)
	if err != nil {
		return nil, err
	}
	return NewFencedBoard(cas, tp)
}

func (b *FencedBoard) ClaimOptions() []claim.Option {
	if b == nil || b.CAS == nil {
		return nil
	}
	return []claim.Option{claim.WithProviderCAS(b.CAS)}
}

func transitionResult(rec *claim.OutboxRecord, err error) error {
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("provider: nil outbox record after transition")
	}
	if rec.Status == claim.OutboxApplied {
		return nil
	}
	if rec.Status == claim.OutboxFailed {
		msg := rec.LastError
		if msg == "" {
			msg = "provider transition failed"
		}
		if strings.Contains(msg, claim.ErrProviderFenceRejected.Error()) {
			return fmt.Errorf("%w: %s", claim.ErrProviderFenceRejected, msg)
		}
		if strings.Contains(msg, claim.ErrProviderRevisionStale.Error()) {
			return fmt.Errorf("%w: %s", claim.ErrProviderRevisionStale, msg)
		}
		if strings.Contains(msg, claim.ErrProviderAmbiguous.Error()) {
			return fmt.Errorf("%w: %s", claim.ErrProviderAmbiguous, msg)
		}
		return fmt.Errorf("provider: transition failed: %s", msg)
	}
	return fmt.Errorf("provider: transition not applied (status=%s)", rec.Status)
}

// OperationKindStatus builds a stable Begin/Complete kind for a status write.
func OperationKindStatus(status string) string {
	return "status:" + NormalizeStatus(status)
}

// OperationKindClaim is the kind for ClaimTask transitions.
const OperationKindClaim = "claim"

// OperationKindComment builds a stable kind for a comment body.
func OperationKindComment(body string) string {
	sum := sha256.Sum256([]byte(body))
	return fmt.Sprintf("comment:%x", sum[:8])
}

// MutateStatus is Begin + Complete with kind status:<canonical>.
func (b *FencedBoard) MutateStatus(
	ctx context.Context,
	mgr *claim.ClaimManager,
	key claim.LeaseKey,
	ownerID string,
	generation int64,
	taskID, status string,
) error {
	if b == nil || b.TP == nil {
		return fmt.Errorf("provider: nil FencedBoard")
	}
	if mgr == nil {
		return fmt.Errorf("provider: MutateStatus requires ClaimManager")
	}
	kind := OperationKindStatus(status)
	if _, err := mgr.BeginProviderTransition(ctx, key, ownerID, generation, kind); err != nil {
		return err
	}
	expected, err := b.CAS.ReadRevision(ctx, taskID)
	if err != nil {
		return err
	}
	ctx = WithCASExpectation(ctx, status, "")
	rec, err := mgr.CompleteProviderTransition(ctx, key, ownerID, generation, taskID, kind, expected,
		func(ctx context.Context) error {
			return b.TP.UpdateStatus(ctx, taskID, status)
		})
	return transitionResult(rec, err)
}

// MutateClaim is Begin + Complete with kind claim.
func (b *FencedBoard) MutateClaim(
	ctx context.Context,
	mgr *claim.ClaimManager,
	key claim.LeaseKey,
	ownerID string,
	generation int64,
	taskID, role string,
) error {
	if b == nil || b.TP == nil {
		return fmt.Errorf("provider: nil FencedBoard")
	}
	if mgr == nil {
		return fmt.Errorf("provider: MutateClaim requires ClaimManager")
	}
	kind := OperationKindClaim
	if _, err := mgr.BeginProviderTransition(ctx, key, ownerID, generation, kind); err != nil {
		return err
	}
	expected, err := b.CAS.ReadRevision(ctx, taskID)
	if err != nil {
		return err
	}
	ctx = WithCASExpectation(ctx, StatusInProgress, "")
	rec, err := mgr.CompleteProviderTransition(ctx, key, ownerID, generation, taskID, kind, expected,
		func(ctx context.Context) error {
			return b.TP.ClaimTask(ctx, taskID, role)
		})
	return transitionResult(rec, err)
}

// MutateComment is Begin + Complete with kind comment:<body-hash>.
func (b *FencedBoard) MutateComment(
	ctx context.Context,
	mgr *claim.ClaimManager,
	key claim.LeaseKey,
	ownerID string,
	generation int64,
	taskID, body string,
) error {
	if b == nil || b.TP == nil {
		return fmt.Errorf("provider: nil FencedBoard")
	}
	if mgr == nil {
		return fmt.Errorf("provider: MutateComment requires ClaimManager")
	}
	kind := OperationKindComment(body)
	if _, err := mgr.BeginProviderTransition(ctx, key, ownerID, generation, kind); err != nil {
		return err
	}
	expected, err := b.CAS.ReadRevision(ctx, taskID)
	if err != nil {
		return err
	}
	ctx = WithCASExpectation(ctx, "", body)
	rec, err := mgr.CompleteProviderTransition(ctx, key, ownerID, generation, taskID, kind, expected,
		func(ctx context.Context) error {
			// Op-bound body for live readback identity (all TaskProviders).
			return b.TP.AddComment(ctx, taskID, CommentOpTaggedBody(body, casOpID(ctx)))
		})
	return transitionResult(rec, err)
}
