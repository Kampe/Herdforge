package security

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LiveClaimRecord is the canonical FAC-147 active claim snapshot required
// before a task launch may bind a lease generation.
type LiveClaimRecord struct {
	TaskRef    string
	Generation int64
	OwnerID    string
	Role       string
	ExpiresAt  time.Time
	// AgentSessionID optional binding to live Herdr session when known.
	AgentSessionID string
}

// LiveClaimLookup resolves the exact active claim for a task ref.
// Production: ClaimManager/ActiveClaims adapter. Tests inject fakes.
type LiveClaimLookup interface {
	LookupActiveClaim(ctx context.Context, taskRef string) (*LiveClaimRecord, error)
}

// ErrLeaseNotLive is returned when provenance does not match a live claim.
var ErrLeaseNotLive = fmt.Errorf("%w: lease not live against FAC-147 claim/fence", ErrUnknownPolicy)

// ValidateLiveTaskLease proves taskRef+generation against the live claim record.
// standingOK permits standing:<name> without a claim store.
// A fabricated future generation (or ambient positive integer with no record) fails.
func ValidateLiveTaskLease(ctx context.Context, lookup LiveClaimLookup, taskRef, lease string, standingOK bool, ownerHint, sessionHint string) error {
	if err := ValidateTaskRef(taskRef); err != nil {
		return err
	}
	lease = strings.TrimSpace(lease)
	if strings.HasPrefix(lease, "standing:") {
		if !standingOK {
			return fmt.Errorf("%w: standing lease not allowed for task launch", ErrUnknownPolicy)
		}
		if strings.TrimSpace(strings.TrimPrefix(lease, "standing:")) == "" {
			return fmt.Errorf("%w: standing lease missing name", ErrUnknownPolicy)
		}
		return nil
	}
	// Task launches require live claim proof.
	gen, err := strconv.ParseInt(lease, 10, 64)
	if err != nil || gen <= 0 {
		return fmt.Errorf("%w: LeaseGeneration must be positive integer claim generation", ErrUnknownPolicy)
	}
	if lookup == nil {
		return fmt.Errorf("%w: LiveClaimLookup required for task lease validation (FAC-147)", ErrUnknownPolicy)
	}
	rec, err := lookup.LookupActiveClaim(ctx, taskRef)
	if err != nil || rec == nil {
		return fmt.Errorf("%w: no active claim for %s: %v", ErrLeaseNotLive, taskRef, err)
	}
	if rec.Generation != gen {
		return fmt.Errorf("%w: generation %d is not the active claim generation %d for %s",
			ErrLeaseNotLive, gen, rec.Generation, taskRef)
	}
	if !rec.ExpiresAt.IsZero() && time.Now().After(rec.ExpiresAt) {
		return fmt.Errorf("%w: active claim expired for %s", ErrLeaseNotLive, taskRef)
	}
	if ownerHint != "" && rec.OwnerID != "" && ownerHint != rec.OwnerID {
		return fmt.Errorf("%w: owner mismatch want %q got %q", ErrLeaseNotLive, rec.OwnerID, ownerHint)
	}
	if sessionHint != "" && rec.AgentSessionID != "" && sessionHint != rec.AgentSessionID {
		return fmt.Errorf("%w: agent_session mismatch", ErrLeaseNotLive)
	}
	return nil
}

// MapClaimLookup adapts a simple in-memory map for tests.
type MapClaimLookup map[string]LiveClaimRecord

func (m MapClaimLookup) LookupActiveClaim(_ context.Context, taskRef string) (*LiveClaimRecord, error) {
	if m == nil {
		return nil, ErrLeaseNotLive
	}
	rec, ok := m[taskRef]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrLeaseNotLive, taskRef)
	}
	cp := rec
	return &cp, nil
}

// AllowGen1Lookup is a test-only lookup that treats every task ref as live at generation 1.
// Production must never use this; it exists so Dispatch unit tests can bind LeaseGeneration: 1.
type AllowGen1Lookup struct{}

func (AllowGen1Lookup) LookupActiveClaim(_ context.Context, taskRef string) (*LiveClaimRecord, error) {
	return &LiveClaimRecord{
		TaskRef:    taskRef,
		Generation: 1,
		ExpiresAt:  time.Now().Add(time.Hour),
	}, nil
}
