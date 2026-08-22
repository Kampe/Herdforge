package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/mail"
)

// TestAcknowledgementActuallyDrains proves the queue can reach empty.
//
// FAC-569: the pending filter tested env.Read, which nothing sets, so a drain
// could never complete and both of the consumer's entries stayed read=false
// after one was processed. Acknowledging must genuinely remove an entry.
func TestAcknowledgementActuallyDrains(t *testing.T) {
	mailPath := filepath.Join(t.TempDir(), "control-mail.jsonl")
	const sup = "forge-review-harvest-su"

	api := handoffEnvelopeID("api", []harvest.UnlandedCommit{{SHA: "a1"}})
	defi := handoffEnvelopeID("defi", []harvest.UnlandedCommit{{SHA: "d1"}})
	for _, id := range []string{api, defi} {
		if _, err := enqueueReviewHandoff(context.Background(), mailPath, "pulse", sup, "s", "b", id); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := pendingIn(mailPath, sup)
	if err != nil || len(pending) != 2 {
		t.Fatalf("both must be pending: %d %v", len(pending), err)
	}

	// Acknowledge only the API handoff, as the live supervisor did.
	if err := mail.NewMailbox(mailPath).MarkHandled(sup, api); err != nil {
		t.Fatal(err)
	}
	pending, err = pendingIn(mailPath, sup)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("exactly one must remain pending, got %d", len(pending))
	}
	if pending[0].ID != defi {
		t.Fatalf("the UNPROCESSED handoff must remain, got %s", pending[0].ID)
	}

	// Acknowledging the second empties the queue: a drain can complete.
	if err := mail.NewMailbox(mailPath).MarkHandled(sup, defi); err != nil {
		t.Fatal(err)
	}
	pending, _ = pendingIn(mailPath, sup)
	if len(pending) != 0 {
		t.Fatalf("queue must reach empty, got %d", len(pending))
	}
}

// pendingIn is PendingReviewHandoffs against an explicit mailbox path.
func pendingIn(mailPath, recipient string) ([]*mail.Envelope, error) {
	return pendingReviewHandoffsIn(mailPath, recipient)
}
