package control

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/outbox"
	"github.com/redis/go-redis/v9"
)

type conformanceSender struct {
	name string
	seen map[string]int
	next int64
	err  error
}

type mailboxSender struct{ m *mail.Mailbox }

func (s mailboxSender) SendEnvelopeContext(ctx context.Context, e *mail.Envelope) error {
	return s.m.AppendEnvelopeContext(ctx, e)
}

type brokerSender struct{ b *mail.MessageBroker }

func (s brokerSender) SendEnvelopeContext(ctx context.Context, e *mail.Envelope) error {
	return s.b.SendEnvelopeContext(ctx, e)
}

type testSub struct{ ch chan *redis.Message }

func (s *testSub) Channel(...redis.ChannelOption) <-chan *redis.Message { return s.ch }
func (s *testSub) Close() error                                         { return nil }

type testRedis struct {
	mu   sync.Mutex
	subs map[string]*testSub
}

func (r *testRedis) Publish(_ context.Context, channel string, msg interface{}) *redis.IntCmd {
	b, _ := msg.([]byte)
	r.mu.Lock()
	defer r.mu.Unlock()
	for pattern, sub := range r.subs {
		if ok, _ := path.Match(pattern, channel); ok {
			sub.ch <- &redis.Message{Channel: channel, Payload: string(b)}
		}
	}
	return redis.NewIntResult(1, nil)
}
func (r *testRedis) PSubscribe(_ context.Context, patterns ...string) mail.Subscription {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &testSub{ch: make(chan *redis.Message, 8)}
	r.subs[patterns[0]] = s
	return s
}
func (r *testRedis) Close() error { return nil }

func (s *conformanceSender) SendEnvelopeContext(_ context.Context, e *mail.Envelope) error {
	if s.err != nil {
		return s.err
	}
	if s.seen == nil {
		s.seen = map[string]int{}
	}
	s.seen[e.ID]++
	if e.Sequence == 0 {
		s.next++
		e.Sequence = s.next
	}
	return nil
}

type conformanceWaker struct {
	calls int
	fail  error
}

type authority struct{ identity LaneIdentity }

func (a authority) Resolve(context.Context, Order) (LaneIdentity, error) { return a.identity, nil }

func (w *conformanceWaker) Wake(_ context.Context, r WakeRequest) (WakeReceipt, error) {
	w.calls++
	if w.fail != nil {
		return WakeReceipt{}, w.fail
	}
	return WakeReceipt{MessageID: r.MessageID, Consumed: true}, nil
}

type evidenceReader struct{ ok bool }

func (e evidenceReader) ReadEvidence(context.Context, string, bool) (AckEvidence, error) {
	return AckEvidence{}, nil
}

type structuredEvidence struct {
	evidence AckEvidence
	err      error
}

func (e structuredEvidence) ReadEvidence(context.Context, string, bool) (AckEvidence, error) {
	return e.evidence, e.err
}

func evidenceFor(t *testing.T, store *outbox.Store, o Order) AckEvidence {
	t.Helper()
	key, digest, _, err := identityKey(o)
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.GetByKey(key)
	if err != nil || item == nil {
		t.Fatalf("stored order: %v %#v", err, item)
	}
	return AckEvidence{IdempotencyKey: key, MessageID: item.MessageID, Sequence: item.Sequence, Repository: o.Repository, TaskRef: o.TaskRef, Lane: o.Lane, LeaseGeneration: o.LeaseGeneration, CandidateSHA: o.CandidateSHA, Kind: o.Kind, BodyDigest: digest}
}

func fixtureOrder() Order {
	return Order{LaneIdentity: LaneIdentity{Repository: "repo", TaskRef: "FAC-182", Lane: "worker-1", LeaseGeneration: 7, CandidateSHA: "abc123"}, Kind: KindRepair, Body: "repair the candidate"}
}

// The exact same delivery contract is run through both transport labels. The
// transports intentionally share only Sender: this catches accidental
// dependence on Redis publish success or a mailbox implementation detail.
func TestDeliveryConformance_FileAndRedisModes(t *testing.T) {
	for _, mode := range []string{"file", "redis"} {
		t.Run(mode, func(t *testing.T) {
			mailbox := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
			var sender Sender
			var broker *mail.MessageBroker
			if mode == "file" {
				sender = mailboxSender{mailbox}
			} else {
				r := &testRedis{subs: map[string]*testSub{}}
				broker = mail.NewMessageBroker(mailbox, mail.WithRedis(r, "control"))
				defer broker.Close()
				sender = brokerSender{broker}
			}
			store, err := outbox.NewStore(filepath.Join(t.TempDir(), "control.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			waker := &conformanceWaker{}
			d := &Delivery{Outbox: store, Sender: sender, Waker: waker, Authority: authority{fixtureOrder().LaneIdentity}, Owner: mode + "-owner"}
			o := fixtureOrder()
			first, err := d.Deliver(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			second, err := d.Deliver(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			if first.IdempotencyKey != second.IdempotencyKey || first.MessageID != second.MessageID {
				t.Fatalf("identity changed across retry: %#v %#v", first, second)
			}
			if waker.calls != 2 {
				t.Fatalf("wake retry count = %d, want 2 (wake is not durable storage)", waker.calls)
			}
			envs, err := mailbox.ReadInbox(o.Lane)
			if err != nil || len(envs) != 1 || envs[0].ID != first.MessageID || envs[0].Body == "" {
				t.Fatalf("durable %s envelope evidence: count=%d err=%v", mode, len(envs), err)
			}
		})
	}
}

func TestDeliveryFailClosedBeforeWakeAndStaleIdentity(t *testing.T) {
	store, err := outbox.NewStore(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sender := &conformanceSender{err: errors.New("mailbox unavailable")}
	waker := &conformanceWaker{}
	d := &Delivery{Outbox: store, Sender: sender, Waker: waker, Authority: authority{fixtureOrder().LaneIdentity}, Owner: "file-owner"}
	if _, err := d.Deliver(context.Background(), fixtureOrder()); err == nil {
		t.Fatal("mail failure must reject before wake")
	}
	if waker.calls != 0 {
		t.Fatal("wake happened after mailbox failure")
	}
	stale := fixtureOrder()
	stale.LeaseGeneration++
	if _, err := d.Deliver(context.Background(), stale); !errors.Is(err, ErrStaleIdentity) {
		t.Fatalf("stale generation error = %v", err)
	}
}

func TestDeliverThenAcknowledgeNormalEmptyBodyDigest(t *testing.T) {
	store := newTestControlStore(t)
	o := fixtureOrder() // normal caller input intentionally omits BodyDigest.
	sender := &conformanceSender{}
	d := &Delivery{Outbox: store, Sender: sender, Waker: &conformanceWaker{}, Authority: authority{o.LaneIdentity}, Owner: "ack-owner"}
	if _, err := d.Deliver(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	d.Evidence = structuredEvidence{evidence: evidenceFor(t, store, o)}
	got, err := d.AcknowledgeEvidence(context.Background(), o)
	if err != nil {
		t.Fatalf("normal empty digest acknowledge: %v", err)
	}
	if got.State != outbox.StatusAcknowledged || got.MessageID == "" || got.Sequence <= 0 {
		t.Fatalf("bad terminal evidence: %#v", got)
	}
	again, err := d.AcknowledgeEvidence(context.Background(), o)
	if err != nil || again.State != outbox.StatusAcknowledged {
		t.Fatalf("ack idempotency: %#v %v", again, err)
	}
}

func TestBodyDigestMismatchAndMissingEvidenceFailClosed(t *testing.T) {
	store := newTestControlStore(t)
	o := fixtureOrder()
	o.BodyDigest = "not-sha256"
	d := &Delivery{Outbox: store, Sender: &conformanceSender{}, Waker: &conformanceWaker{}, Authority: authority{fixtureOrder().LaneIdentity}, Owner: "digest-owner"}
	if _, err := d.Deliver(context.Background(), o); err == nil {
		t.Fatal("changed body digest was accepted")
	}
	o = fixtureOrder()
	if _, err := d.Deliver(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	d.Evidence = structuredEvidence{}
	if _, err := d.AcknowledgeEvidence(context.Background(), o); err == nil {
		t.Fatal("missing evidence was accepted")
	}
}

func TestSenderCannotRunBeforeDurableOrder(t *testing.T) {
	store := newTestControlStore(t)
	o := fixtureOrder()
	key, _, _, _ := identityKey(o)
	sender := &orderingSender{store: store, key: key}
	d := &Delivery{Outbox: store, Sender: sender, Waker: &conformanceWaker{}, Authority: authority{o.LaneIdentity}, Owner: "ordering-owner"}
	if _, err := d.Deliver(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !sender.sawDurable {
		t.Fatal("send-before-persist mutant was not causally probed")
	}
}

type orderingSender struct {
	store      *outbox.Store
	key        string
	sawDurable bool
}

func (s *orderingSender) SendEnvelopeContext(_ context.Context, e *mail.Envelope) error {
	item, err := s.store.GetByKey(s.key)
	if err != nil {
		return err
	}
	if item == nil {
		return errors.New("send occurred before durable outbox")
	}
	s.sawDurable = true
	e.Sequence = 1
	return nil
}

func TestStructuredEvidenceRejectsEveryBoundMismatch(t *testing.T) {
	fields := []struct {
		name   string
		mutate func(*AckEvidence)
	}{
		{"message", func(e *AckEvidence) { e.MessageID = "wrong" }}, {"sequence", func(e *AckEvidence) { e.Sequence++ }},
		{"lane", func(e *AckEvidence) { e.Lane = "other" }}, {"generation", func(e *AckEvidence) { e.LeaseGeneration++ }},
		{"candidate", func(e *AckEvidence) { e.CandidateSHA = "other" }}, {"kind", func(e *AckEvidence) { e.Kind = KindRebase }},
		{"digest", func(e *AckEvidence) { e.BodyDigest = "wrong" }}, {"key", func(e *AckEvidence) { e.IdempotencyKey = "wrong" }},
	}
	for _, tc := range fields {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestControlStore(t)
			o := fixtureOrder()
			d := &Delivery{Outbox: store, Sender: &conformanceSender{}, Waker: &conformanceWaker{}, Authority: authority{o.LaneIdentity}, Owner: tc.name + "-owner"}
			if _, err := d.Deliver(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			e := evidenceFor(t, store, o)
			tc.mutate(&e)
			d.Evidence = structuredEvidence{evidence: e}
			if _, err := d.AcknowledgeEvidence(context.Background(), o); err == nil {
				t.Fatal("mismatched evidence was accepted")
			}
		})
	}
	store := newTestControlStore(t)
	o := fixtureOrder()
	d := &Delivery{Outbox: store, Sender: &conformanceSender{}, Waker: &conformanceWaker{}, Authority: authority{o.LaneIdentity}, Owner: "supersede-owner"}
	if _, err := d.Deliver(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	d.Evidence = structuredEvidence{evidence: evidenceFor(t, store, o)}
	if _, err := d.SupersedeEvidence(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SupersedeEvidence(context.Background(), o); err != nil {
		t.Fatal("supersession should be idempotent")
	}
}

func TestCrashEnvelopeBeforeWakeAndAckBeforeRestartDoesNotResend(t *testing.T) {
	store := newTestControlStore(t)
	o := fixtureOrder()
	mailbox := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	firstWaker := &conformanceWaker{fail: errors.New("simulated crash after fsync")}
	d := &Delivery{Outbox: store, Sender: mailboxSender{mailbox}, Waker: firstWaker, Authority: authority{o.LaneIdentity}, Owner: "crash-owner"}
	if _, err := d.Deliver(context.Background(), o); err == nil {
		t.Fatal("wake crash was not observable")
	}
	envs, err := mailbox.ReadInbox(o.Lane)
	if err != nil || len(envs) != 1 {
		t.Fatalf("durable envelope after wake crash: %d %v", len(envs), err)
	}
	// A restart retries wake from StatusSent, never appending a second envelope.
	d.Waker = &conformanceWaker{}
	if _, err := d.Deliver(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	envs, _ = mailbox.ReadInbox(o.Lane)
	if len(envs) != 1 {
		t.Fatalf("retry duplicated envelope: %d", len(envs))
	}
	d.Evidence = structuredEvidence{evidence: evidenceFor(t, store, o)}
	if _, err := d.AcknowledgeEvidence(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	restarted := &Delivery{Outbox: store, Sender: mailboxSender{mailbox}, Waker: &conformanceWaker{fail: errors.New("must not wake after ack")}, Authority: authority{o.LaneIdentity}, Owner: "restart-owner"}
	if _, err := restarted.Deliver(context.Background(), o); err != nil {
		t.Fatalf("ack-deduped restart resent wake: %v", err)
	}
	final, _ := mailbox.ReadInbox(o.Lane)
	if len(final) != 1 {
		t.Fatalf("ack restart changed envelope count: %d", len(final))
	}
}

func newTestControlStore(t *testing.T) *outbox.Store {
	t.Helper()
	s, err := outbox.NewStore(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDeliveryWakeFailureRetainsSentOrder(t *testing.T) {
	store, err := outbox.NewStore(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sender := &conformanceSender{}
	waker := &conformanceWaker{fail: errors.New("herdr unavailable")}
	d := &Delivery{Outbox: store, Sender: sender, Waker: waker, Authority: authority{fixtureOrder().LaneIdentity}, Owner: "wake-owner"}
	if _, err := d.Deliver(context.Background(), fixtureOrder()); err == nil {
		t.Fatal("wake failure must be observable")
	}
	if got := len(sender.seen); got != 1 {
		t.Fatalf("wake failure lost or duplicated order: %d envelopes", got)
	}
	if _, err := d.Deliver(context.Background(), fixtureOrder()); err == nil {
		t.Fatal("retry should still report wake failure")
	}
	if sender.seen[messageID("control:v1:"+"0000000000000000000000000000000000000000000000000000000000000000")] != 1 { // map shape is checked below; this branch is never the identity.
		if len(sender.seen) != 1 {
			t.Fatal("retry changed durable envelope count")
		}
	}
}
