package outbox

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type fakeHandler struct {
	kind       string
	mu         sync.Mutex
	sent       []Item
	failNTimes int
	calls      int
}

func (f *fakeHandler) Kind() string { return f.kind }

func (f *fakeHandler) Send(ctx context.Context, item Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failNTimes {
		return errors.New("simulated send failure")
	}
	f.sent = append(f.sent, item)
	return nil
}

func TestRelay_DeliversPendingItemsToMatchingHandler(t *testing.T) {
	s := tempStore(t)
	s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr", Payload: "p1"})
	s.Enqueue(Item{IdempotencyKey: "k2", TaskRef: "FAC-1", Kind: "git", Payload: "p2"})

	herdr := &fakeHandler{kind: "herdr"}
	git := &fakeHandler{kind: "git"}
	relay := NewRelay(s, herdr, git)

	processed, err := relay.RelayOnce(context.Background())
	if err != nil {
		t.Fatalf("relay once: %v", err)
	}
	if processed != 2 {
		t.Errorf("expected 2 items processed, got %d", processed)
	}
	if len(herdr.sent) != 1 || herdr.sent[0].Payload != "p1" {
		t.Errorf("expected herdr handler to receive p1, got %+v", herdr.sent)
	}
	if len(git.sent) != 1 || git.sent[0].Payload != "p2" {
		t.Errorf("expected git handler to receive p2, got %+v", git.sent)
	}

	pending, _ := s.Pending("", 10)
	if len(pending) != 0 {
		t.Errorf("expected no pending items after successful relay, got %d", len(pending))
	}
}

func TestRelay_LeavesItemPendingWithoutRegisteredHandler(t *testing.T) {
	s := tempStore(t)
	s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "unregistered"})

	relay := NewRelay(s)
	processed, err := relay.RelayOnce(context.Background())
	if err != nil {
		t.Fatalf("relay once: %v", err)
	}
	if processed != 0 {
		t.Errorf("expected 0 processed with no handler registered, got %d", processed)
	}
	pending, _ := s.Pending("", 10)
	if len(pending) != 1 {
		t.Errorf("expected item to remain pending, got %d", len(pending))
	}
}

func TestRelay_RetriesOnFailureThenSucceeds(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})

	h := &fakeHandler{kind: "herdr", failNTimes: 1}
	relay := NewRelay(s, h)
	relay.MaxAttempts = 5

	if _, err := relay.RelayOnce(context.Background()); err != nil {
		t.Fatalf("relay once (1): %v", err)
	}
	pending, _ := s.Pending("", 10)
	if len(pending) != 1 || pending[0].Attempts != 1 {
		t.Fatalf("expected item still pending with attempts=1 after failure, got %+v", pending)
	}

	if _, err := relay.RelayOnce(context.Background()); err != nil {
		t.Fatalf("relay once (2): %v", err)
	}
	pending, _ = s.Pending("", 10)
	if len(pending) != 0 {
		t.Fatalf("expected item to succeed on retry, got %d pending", len(pending))
	}
	got, _ := s.Get(item.ID)
	if got.Status != StatusSent {
		t.Errorf("expected status=sent, got %s", got.Status)
	}
}

func TestRelay_GivesUpAfterMaxAttempts(t *testing.T) {
	s := tempStore(t)
	s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})

	h := &fakeHandler{kind: "herdr", failNTimes: 100}
	relay := NewRelay(s, h)
	relay.MaxAttempts = 2

	for i := 0; i < relay.MaxAttempts; i++ {
		relay.RelayOnce(context.Background())
	}
	pending, _ := s.Pending("", 10)
	if len(pending) != 0 {
		t.Fatalf("expected item to leave pending queue after max attempts, got %d", len(pending))
	}
}
