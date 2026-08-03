package control

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
)

type testProcessor func(context.Context, Order, string) (string, error)

func (p testProcessor) Apply(ctx context.Context, o Order, key string) (string, error) {
	return p(ctx, o, key)
}

func testConsumer(mb *mail.Mailbox, identity LaneIdentity, p testProcessor) *Consumer {
	return &Consumer{Mailbox: mb, Identity: identity, Processor: p, State: MailboxProcessorStore{Mailbox: mb, Sender: identity.Lane}}
}

type durableResultProcessor struct {
	Mailbox *mail.Mailbox
	Calls   int
}

func (p *durableResultProcessor) Apply(ctx context.Context, _ Order, key string) (string, error) {
	envs, err := p.Mailbox.ReadInboxContext(ctx, mail.CoordinatorInbox)
	if err != nil {
		return "", err
	}
	for _, env := range envs {
		if env.Subject == "control/processor_result" && env.ID == "result-"+shortID(key) {
			return env.Body, nil
		}
	}
	p.Calls++
	if err := p.Mailbox.SendEnvelopeContext(ctx, &mail.Envelope{ID: "result-" + shortID(key), Sender: "durable-processor", Recipient: mail.CoordinatorInbox, Subject: "control/processor_result", Body: "committed-result"}); err != nil {
		return "", err
	}
	return "committed-result", nil
}

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
	c := testConsumer(mb, o.LaneIdentity, func(context.Context, Order, string) (string, error) { processed++; return "ok", nil })
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

func TestConsumerRestartAfterProcessingUsesIdempotencyKey(t *testing.T) {
	mb := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	o := fixtureOrder()
	key, digest, payload, err := identityKey(o)
	if err != nil {
		t.Fatal(err)
	}
	o.BodyDigest = digest
	_, _, payload, _ = identityKey(o)
	orderEnv := &mail.Envelope{ID: messageID(key), Sender: mail.CoordinatorInbox, Recipient: o.Lane, Subject: "control/repair", Body: payload}
	if err := mb.SendEnvelopeContext(context.Background(), orderEnv); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	c := testConsumer(mb, o.LaneIdentity, func(_ context.Context, _ Order, id string) (string, error) { seen[id]++; return "ok", nil })
	if err := c.State.Save(context.Background(), ProcessorState{Key: key, State: "processing", MessageID: orderEnv.ID, Sequence: orderEnv.Sequence}); err != nil {
		t.Fatal(err)
	}
	if err := c.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen[key] != 1 {
		t.Fatalf("idempotent processor calls = %d, want one", seen[key])
	}
	if err := c.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen[key] != 1 {
		t.Fatalf("restart reapplied side effect: %d", seen[key])
	}
}

func TestConsumerAppliedMarkerReconstructsMissingAck(t *testing.T) {
	mb := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	o := fixtureOrder()
	key, digest, payload, err := identityKey(o)
	if err != nil {
		t.Fatal(err)
	}
	o.BodyDigest = digest
	_, _, payload, _ = identityKey(o)
	env := &mail.Envelope{ID: messageID(key), Sender: mail.CoordinatorInbox, Recipient: o.Lane, Subject: "control/repair", Body: payload}
	if err := mb.SendEnvelopeContext(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c := testConsumer(mb, o.LaneIdentity, func(context.Context, Order, string) (string, error) { calls++; return "ok", nil })
	if err := c.State.Save(context.Background(), ProcessorState{Key: key, State: "applied", Result: "ok", MessageID: env.ID, Sequence: env.Sequence}); err != nil {
		t.Fatal(err)
	}
	if err := c.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("applied restart reprocessed side effect: %d", calls)
	}
	ack, err := mb.ReadInboxContext(context.Background(), mail.CoordinatorInbox)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ack {
		if e.Subject == "control/ack" {
			found = true
		}
	}
	if !found {
		t.Fatal("applied restart did not emit missing ack")
	}
}

func TestConsumerReconstructsDurableProcessorAfterSideEffectCrash(t *testing.T) {
	mb := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	o := fixtureOrder()
	key, digest, payload, err := identityKey(o)
	if err != nil {
		t.Fatal(err)
	}
	o.BodyDigest = digest
	_, _, payload, _ = identityKey(o)
	env := &mail.Envelope{ID: messageID(key), Sender: mail.CoordinatorInbox, Recipient: o.Lane, Subject: "control/repair", Body: payload}
	if err := mb.SendEnvelopeContext(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	state := MailboxProcessorStore{Mailbox: mb, Sender: o.Lane}
	if err := state.Save(context.Background(), ProcessorState{Key: key, State: "processing", MessageID: env.ID, Sequence: env.Sequence}); err != nil {
		t.Fatal(err)
	}
	first := &durableResultProcessor{Mailbox: mb}
	if _, err := first.Apply(context.Background(), o, key); err != nil {
		t.Fatal(err)
	}
	// Simulated crash: the side-effect/result is durable, but Consumer has not
	// written applied or ACK. Reconstruct both processor and Consumer.
	second := &durableResultProcessor{Mailbox: mb}
	c := &Consumer{Mailbox: mb, Identity: o.LaneIdentity, Processor: second, State: MailboxProcessorStore{Mailbox: mb, Sender: o.Lane}}
	if err := c.Consume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second.Calls != 0 {
		t.Fatalf("reconstructed processor duplicated durable side effect: calls=%d", second.Calls)
	}
	results, _ := mb.ReadInboxContext(context.Background(), mail.CoordinatorInbox)
	count := 0
	ack := false
	for _, e := range results {
		if e.Subject == "control/processor_result" {
			count++
		}
		if e.Subject == "control/ack" {
			ack = true
		}
	}
	if count != 1 || !ack {
		t.Fatalf("durable side effect/result count=%d ack=%v", count, ack)
	}
}

func TestConsumerRejectsCorruptProcessorStateBinding(t *testing.T) {
	mb := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	o := fixtureOrder()
	key, digest, payload, _ := identityKey(o)
	o.BodyDigest = digest
	_, _, payload, _ = identityKey(o)
	env := &mail.Envelope{ID: messageID(key), Sender: mail.CoordinatorInbox, Recipient: o.Lane, Subject: "control/repair", Body: payload}
	if err := mb.SendEnvelopeContext(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	state := MailboxProcessorStore{Mailbox: mb, Sender: o.Lane}
	if err := state.Save(context.Background(), ProcessorState{Key: key, State: "processing", MessageID: "wrong-message", Sequence: env.Sequence}); err != nil {
		t.Fatal(err)
	}
	c := testConsumer(mb, o.LaneIdentity, func(context.Context, Order, string) (string, error) { return "", nil })
	if err := c.Consume(context.Background()); err == nil {
		t.Fatal("corrupt processor state was accepted")
	}
}
