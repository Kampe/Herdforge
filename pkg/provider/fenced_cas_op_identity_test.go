package provider

import (
	"context"
	"testing"
	"time"
)

// TestFencedCAS_AmbiguousComment_BareBodyDoesNotSatisfyOpID is the outer
// FencedCAS regression for hold ctimb2a9: when OpID is set, an untagged
// prior comment with the same free text must NOT MarkApplied the new op.
// Deleting the op-bound check (restoring cmt == ExpectedComment) fails this.
func TestFencedCAS_AmbiguousComment_BareBodyDoesNotSatisfyOpID(t *testing.T) {
	ctx := context.Background()
	mp := NewMemoryProvider()
	mp.AddTask(testTask("opb1", "FAC-OPB", "to-do"))
	// Prior untagged comment with identical free text (cross-op collapse bait).
	_ = mp.AddComment(ctx, "opb1", "same free text")

	store := NewMemoryFenceStore()
	cas, err := NewFencedCAS(store, mp)
	if err != nil {
		t.Fatal(err)
	}
	// Ambiguous receipt for a NEW opID expecting the same free text.
	opID := "op-new-999"
	if err := store.MarkAmbiguous(ctx, OpReceipt{
		OpID:            opID,
		TaskID:          "opb1",
		FenceToken:      1,
		ExpectedComment: "same free text",
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = store.Advance(ctx, "opb1", 1)

	rev, _ := cas.ReadRevision(ctx, "opb1")
	var mutated int
	_, err = cas.CompareAndSwap(WithCASExpectation(ctx, "", "same free text"), "opb1", rev, 1, opID, func(ctx context.Context) error {
		mutated++
		// Production path posts op-tagged body.
		return mp.AddComment(ctx, "opb1", CommentOpTaggedBody("same free text", opID))
	})
	if err != nil {
		t.Fatalf("recovery CAS: %v", err)
	}
	// Must have re-mutated (ABSENT for op-bound match), not silent MarkApplied
	// from bare body equality on the prior untagged comment.
	if mutated != 1 {
		t.Fatalf("want recovery mutate once (bare body must not satisfy OpID), mutated=%d", mutated)
	}
	rec, _ := store.LookupApplied(ctx, opID)
	if rec == nil || rec.Ambiguous {
		t.Fatalf("want applied receipt after recovery, got %+v", rec)
	}
	// Tagged comment present; untagged alone must not MatchCommentOp.
	if MatchCommentOp("same free text", "same free text", opID) {
		t.Fatal("untagged body must not match op-bound identity")
	}
	_ = time.Second
}
