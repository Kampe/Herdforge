package mail

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestQueueRoutineIsIdempotentAndPreservesBytes(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	body := "line one\n`literal` and $(not expanded)\nline three"
	first, err := box.QueueRoutine(context.Background(), "herd-send", "worker", body)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.Body != body || first.Subject != QueuedDeliverySubject {
		t.Fatalf("envelope = %+v", first)
	}
	second, err := box.QueueRoutine(context.Background(), "herd-send", "worker", body)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Sequence != first.Sequence {
		t.Fatalf("retry minted a new envelope: first=%+v second=%+v", first, second)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != body {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestPendingQueuedSkipsHandledAndPreservesOrder(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	first, err := box.QueueRoutine(context.Background(), "herd-send", "worker", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	second, err := box.QueueRoutine(context.Background(), "herd-send", "worker", "beta")
	if err != nil {
		t.Fatal(err)
	}
	if err := box.MarkHandled("worker", first.ID); err != nil {
		t.Fatal(err)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != second.ID || pending[0].Body != "beta" {
		t.Fatalf("pending after ack = %+v", pending)
	}
}

func TestPendingRoutineIncludesReportsButNotControlOrCallbacks(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	for _, msg := range []struct {
		subject string
		body    string
	}{
		{"FAC-773 report", "report bytes"},
		{ControlSubjectPrefix + " issue FAC-1", "signed control"},
		{"complete: FAC-2", "callback"},
	} {
		if _, err := box.SendMessage("worker", "coordinator", msg.subject, msg.body); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := box.PendingRoutine("coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Subject != "FAC-773 report" || pending[0].Body != "report bytes" {
		t.Fatalf("routine pending = %+v", pending)
	}
}

func TestQueueRoutineCancelBeforeAppendLeavesNoEnvelope(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := box.QueueRoutine(ctx, "herd-send", "worker", "payload"); err == nil {
		t.Fatal("canceled context must fail closed")
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("canceled send left envelopes: %+v", pending)
	}
}

func TestQueueRoutineConcurrentSamePayloadWritesOneLine(t *testing.T) {
	box := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			env, err := box.QueueRoutine(ctx, "herd-send", "worker", "same")
			if err != nil {
				t.Errorf("queue: %v", err)
				return
			}
			ids <- env.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]struct{}{}
	for id := range ids {
		seen[id] = struct{}{}
	}
	if len(seen) != 1 {
		t.Fatalf("concurrent same payload produced %d identities: %v", len(seen), seen)
	}
	pending, err := box.PendingQueued("worker")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("concurrent appends duplicated the mailbox: %+v", pending)
	}
}
