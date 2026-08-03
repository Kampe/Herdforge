package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
)

func TestConsumerRestartUsesDurableStructuredAck(t *testing.T) {
	mb := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	o := fixtureOrder()
	key, digest, payload, err := identityKey(o)
	if err != nil {
		t.Fatal(err)
	}
	o.BodyDigest = digest
	_, _, payload, err = identityKey(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := mb.SendEnvelopeContext(context.Background(), &mail.Envelope{ID: messageID(key), Sender: mail.CoordinatorInbox, Recipient: o.Lane, Subject: "control/repair", Body: payload}); err != nil {
		t.Fatal(err)
	}
	processed := 0
	c := &Consumer{Mailbox: mb, Identity: o.LaneIdentity, Process: func(context.Context, Order) error { processed++; return nil }}
	if err := c.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if processed != 1 {
		t.Fatalf("processed %d times, want one", processed)
	}
	envs, err := mb.ReadInboxContext(context.Background(), mail.CoordinatorInbox)
	if err != nil {
		t.Fatal(err)
	}
	var found AckEvidence
	for _, env := range envs {
		if env.Subject == "control/ack" {
			if err := json.Unmarshal([]byte(env.Body), &found); err != nil {
				t.Fatal(err)
			}
		}
	}
	if found.IdempotencyKey != key || found.MessageID != messageID(key) || found.Sequence <= 0 || found.BodyDigest != digest {
		t.Fatalf("incomplete ack evidence: %+v", found)
	}
}
