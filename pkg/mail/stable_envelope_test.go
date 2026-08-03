package mail

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStableEnvelopeIDRejectsChangedContent(t *testing.T) {
	m := NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	first := &Envelope{ID: "stable-1", Sender: "coordinator", Recipient: "worker", Subject: "control/repair", Body: "one", Timestamp: time.Now().UTC()}
	if err := m.AppendEnvelopeContext(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	retry := &Envelope{ID: first.ID, Sender: first.Sender, Recipient: first.Recipient, Subject: first.Subject, Body: "changed", Timestamp: time.Now().UTC()}
	if err := m.AppendEnvelopeContext(context.Background(), retry); err == nil {
		t.Fatal("same stable ID with changed content was accepted")
	}
	if retry.Sequence != first.Sequence || retry.Timestamp != first.Timestamp {
		t.Fatalf("stable retry did not recover authoritative identity: %#v", retry)
	}
}
