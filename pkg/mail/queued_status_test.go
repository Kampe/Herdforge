package mail

import (
	"context"
	"strings"
	"testing"
)

func TestPendingVisibleListsQueuedDurableAndOrdinary(t *testing.T) {
	box := NewMailbox(t.TempDir() + "/mail.jsonl")
	queued, err := box.QueueRoutine(context.Background(), "coord", "worker", "bind-1", "queued body")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := box.SendMessage("coord", "worker", "ordinary report", "ordinary body"); err != nil {
		t.Fatal(err)
	}
	if pending, err := box.PendingOrdinary("worker"); err != nil || len(pending) != 1 || pending[0].Subject != "ordinary report" {
		t.Fatalf("ordinary pending leaked queued: %+v err=%v", pending, err)
	}
	visible, err := box.PendingVisible("worker")
	if err != nil || len(visible) != 2 {
		t.Fatalf("visible=%+v err=%v", visible, err)
	}
	var sawQueued, sawOrdinary bool
	for _, env := range visible {
		if env.ID == queued.ID && IsQueuedDelivery(env) {
			sawQueued = true
		}
		if env.Subject == "ordinary report" && IsOrdinaryReport(env) {
			sawOrdinary = true
		}
	}
	if !sawQueued || !sawOrdinary {
		t.Fatalf("visible missing classes: %+v", visible)
	}
}

func TestStatusEnvelopeQueuedUntilHandledAndNotDelivered(t *testing.T) {
	box := NewMailbox(t.TempDir() + "/mail.jsonl")
	queued, err := box.QueueRoutine(context.Background(), "coord", "worker", "bind-1", "queued body")
	if err != nil {
		t.Fatal(err)
	}
	status, err := box.StatusEnvelope("worker", queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ordinary || !status.Queued || !status.Pending || status.Handled || status.Kind != EnvelopeKindQueuedDurable {
		t.Fatalf("queued status before consume = %+v", status)
	}
	if _, err := box.StatusOrdinary("worker", queued.ID); err == nil || !strings.Contains(err.Error(), "not an ordinary report") {
		t.Fatalf("StatusOrdinary must keep ordinary-report semantics: %v", err)
	}
	if err := box.MarkHandled("worker", queued.ID); err != nil {
		t.Fatal(err)
	}
	status, err = box.StatusEnvelope("worker", queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Queued || status.Pending || !status.Handled || status.Ordinary {
		t.Fatalf("queued status after consume = %+v", status)
	}
	pending, err := box.PendingVisible("worker")
	if err != nil || len(pending) != 0 {
		t.Fatalf("consumed queued still visible: %+v err=%v", pending, err)
	}
}
