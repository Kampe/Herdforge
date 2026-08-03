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
