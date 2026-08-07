package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// TestAuthBroker_AmbiguousEmptyRev_RefusesWithoutStatusEvidence.
func TestAuthBroker_AmbiguousEmptyRev_RefusesWithoutStatusEvidence(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "ar1", Ref: "FAC-AR", Status: StatusDone, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})

	broker := NewAuthBroker(store).BindRevisionReader(mp.GetTask)
	broker.StatusOpEvidence = func(ctx context.Context, taskID, opID, expStatus string) (bool, error) {
		return false, nil
	}
	if err := store.MarkAmbiguous(ctx, OpReceipt{
		OpID: "op-no-rev", TaskID: "ar1", FenceToken: 1,
		ExpectedStatus: StatusDone, BaseRevision: "old-base-not-live", Ambiguous: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(ctx, "ar1", 1); err != nil {
		t.Fatal(err)
	}

	calls := 0
	err := broker.Execute(ctx, "ar1", 1, "op-no-rev", StatusDone, "",
		func(ctx context.Context) error {
			calls++
			return mp.UpdateStatus(ctx, "ar1", StatusDone)
		},
		func(ctx context.Context) (EffectState, error) {
			return EffectPresent, nil
		},
	)
	if err == nil {
		t.Fatal("empty-rev Present without status op evidence must refuse")
	}
	if calls != 0 {
		t.Fatalf("must not re-mutate (calls=%d)", calls)
	}
	rec, _ := store.LookupApplied(ctx, "op-no-rev")
	if rec != nil && !rec.Ambiguous {
		t.Fatal("must not MarkApplied")
	}
}

// TestAuthBroker_AmbiguousRevisionMismatch_Refuses: stored revision that
// does not match live fails closed (no silent MarkApplied).
func TestAuthBroker_AmbiguousRevisionMismatch_Refuses(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "ar1m", Ref: "FAC-ARM", Status: StatusDone, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})

	broker := NewAuthBroker(store).BindRevisionReader(mp.GetTask)
	if err := store.MarkAmbiguous(ctx, OpReceipt{
		OpID: "op-bad-rev", TaskID: "ar1m", FenceToken: 1,
		ExpectedStatus: StatusDone, Revision: "not-the-live-rev", Ambiguous: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(ctx, "ar1m", 1); err != nil {
		t.Fatal(err)
	}
	calls := 0
	err := broker.Execute(ctx, "ar1m", 1, "op-bad-rev", StatusDone, "",
		func(ctx context.Context) error {
			calls++
			return nil
		},
		func(ctx context.Context) (EffectState, error) { return EffectPresent, nil },
	)
	if err == nil {
		t.Fatal("expected revision mismatch refuse")
	}
	if !errors.Is(err, claim.ErrProviderAmbiguous) && !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("want mismatch/ambiguous error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("must not re-mutate on mismatch (calls=%d)", calls)
	}
}

// TestAuthBroker_AppliedReceiptCarriesRevision: successful status mutate
// stores non-empty revision on the applied receipt.
func TestAuthBroker_AppliedReceiptCarriesRevision(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryFenceStore()
	mp := NewMemoryProvider()
	now := time.Now().UTC()
	mp.AddTask(&Task{ID: "ar2", Ref: "FAC-AR2", Status: StatusInReview, ProjectID: "p",
		UpdatedAt: now, CreatedAt: now})

	broker := NewAuthBroker(store).BindRevisionReader(mp.GetTask)
	if _, err := store.Advance(ctx, "ar2", 1); err != nil {
		t.Fatal(err)
	}
	err := broker.Execute(ctx, "ar2", 1, "op-with-rev", StatusDone, "",
		func(ctx context.Context) error {
			return mp.UpdateStatus(ctx, "ar2", StatusDone)
		},
		func(ctx context.Context) (EffectState, error) {
			return EffectAbsent, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.LookupApplied(ctx, "op-with-rev")
	if err != nil || rec == nil {
		t.Fatalf("applied receipt missing: %v", err)
	}
	if rec.Ambiguous || rec.Revision == "" {
		t.Fatalf("applied must have revision evidence: %+v", rec)
	}
}
