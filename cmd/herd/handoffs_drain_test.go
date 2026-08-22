package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/pulse"
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

// TestListingNeverMarksHandled is FAC-568 acceptance criterion 3.
//
// An entry may be marked read only when its handoff reaches a DISPOSITION, so
// that an unread entry always means unfinished work. If merely listing the queue
// marked entries handled, a supervisor that looked and then died would leave
// work that no longer appears pending — indistinguishable from work that was
// actually done.
func TestListingNeverMarksHandled(t *testing.T) {
	mailPath := filepath.Join(t.TempDir(), "control-mail.jsonl")
	const sup = "forge-review-harvest-su"
	id := handoffEnvelopeID("api", []harvest.UnlandedCommit{{SHA: "a1"}})
	if _, err := enqueueReviewHandoff(context.Background(), mailPath, "pulse", sup, "s", "b", id); err != nil {
		t.Fatal(err)
	}
	// List repeatedly: observation must never be mistaken for disposition.
	for i := 0; i < 3; i++ {
		pending, err := pendingIn(mailPath, sup)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 1 {
			t.Fatalf("listing must not consume the entry; pass %d saw %d pending", i, len(pending))
		}
	}
	box := mail.NewMailbox(mailPath)
	handled, err := box.Handled(sup, id)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("an entry must not be handled until its handoff reaches a disposition")
	}
	if err := box.MarkHandled(sup, id); err != nil {
		t.Fatal(err)
	}
	handled, err = box.Handled(sup, id)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("disposition must mark the entry handled")
	}
}

// TestNudgeStatesItDoesNotSupersede is FAC-568 acceptance criterion 2.
//
// The live supervisor read two sequential nudges and concluded "The latest
// handoff supersedes the DeFi request", processing one and leaving both durable
// records unread. Retention alone did not help, because the supervisor treated
// the pane as the queue. The packet must say plainly that it is not.
func TestNudgeStatesItDoesNotSupersede(t *testing.T) {
	packet := pulseReviewPacket(pulse.AgentObservation{Name: "defi"}, []harvest.UnlandedCommit{{SHA: "d1d1d1d1d1d1", Subject: "defi work"}}, nil)
	for _, want := range []string{
		"DOES NOT SUPERSEDE",
		"each is independent work",
		"DURABLE INBOX",
	} {
		if !strings.Contains(packet, want) {
			t.Errorf("nudge must state %q; without it a supervisor applies pane supersession", want)
		}
	}
	// It must also name the supported drain commands, so draining is an
	// operation rather than a convention the supervisor has to invent.
	for _, want := range []string{"herd handoffs list", "herd handoffs done"} {
		if !strings.Contains(packet, want) {
			t.Errorf("nudge must name %q so draining is a supported operation", want)
		}
	}
}
