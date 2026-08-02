package mail

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

type mockPubSub struct {
	ch chan *redis.Message
	mu sync.Mutex
}

func newMockPubSub() *mockPubSub {
	return &mockPubSub{ch: make(chan *redis.Message, 10)}
}

func (m *mockPubSub) ReceiveMessage(_ context.Context) (*redis.Message, error) {
	return <-m.ch, nil
}

func (m *mockPubSub) Channel(opts ...redis.ChannelOption) <-chan *redis.Message {
	return m.ch
}

func (m *mockPubSub) Close() error {
	return nil
}

type mockRedisClient struct {
	mu         sync.Mutex
	published  []publishedMsg
	subs       map[string]*mockPubSub
	publishErr atomic.Value
	closeErr   error
	closed     atomic.Bool
}

type publishedMsg struct {
	Channel string
	Data    string
}

func newMockRedisClient() *mockRedisClient {
	return &mockRedisClient{subs: make(map[string]*mockPubSub)}
}

func (m *mockRedisClient) Publish(_ context.Context, channel string, message interface{}) *redis.IntCmd {
	data, _ := message.([]byte)
	msg := publishedMsg{Channel: channel, Data: string(data)}
	m.mu.Lock()
	m.published = append(m.published, msg)
	// also deliver to matching subs
	if err := m.publishErr.Load(); err != nil {
		m.mu.Unlock()
		return redis.NewIntResult(0, err.(error))
	}
	if ps, ok := m.subs[channel]; ok {
		ps.ch <- &redis.Message{Channel: channel, Payload: string(data)}
	}
	m.mu.Unlock()
	return redis.NewIntResult(1, nil)
}

func (m *mockRedisClient) Subscribe(_ context.Context, channels ...string) Subscription {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range channels {
		if _, ok := m.subs[ch]; !ok {
			m.subs[ch] = newMockPubSub()
		}
	}
	return m.subs[channels[0]]
}

func (m *mockRedisClient) Close() error {
	m.closed.Store(true)
	return m.closeErr
}

func TestMessageBroker_SendPublish(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	defer broker.Close()

	env, err := broker.SendMessage("alice", "bob", "Hello", "World")
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if env == nil {
		t.Fatal("expected non-nil envelope")
	}

	mock.mu.Lock()
	if len(mock.published) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(mock.published))
	}
	if mock.published[0].Channel != "herd.bob" {
		t.Errorf("expected channel 'herd.bob', got %s", mock.published[0].Channel)
	}
	mock.mu.Unlock()
}

func TestMessageBroker_SubscriberWritesLocalMailbox(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")
	mb := NewMailbox(mailFile)
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	defer broker.Close()

	env := &Envelope{
		ID:        "host-100",
		Sender:    "carol",
		Recipient: "alice",
		Subject:   "Remote msg",
		Body:      "from another host",
		Timestamp: time.Now(),
	}
	data, _ := json.Marshal(env)

	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	ps := mock.subs["herd.*"]
	mock.mu.Unlock()
	if ps == nil {
		t.Fatal("subscriber should have registered for 'herd.*'")
	}

	ps.ch <- &redis.Message{Channel: "herd.alice", Payload: string(data)}

	time.Sleep(100 * time.Millisecond)

	inbox, err := broker.ReadInbox("alice")
	if err != nil {
		t.Fatalf("ReadInbox failed: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("expected 1 envelope from remote, got %d", len(inbox))
	}
	if inbox[0].Subject != "Remote msg" {
		t.Errorf("expected Subject 'Remote msg', got %s", inbox[0].Subject)
	}
	if inbox[0].Sender != "carol" {
		t.Errorf("expected Sender 'carol', got %s", inbox[0].Sender)
	}
}

func TestMessageBroker_NoRedis(t *testing.T) {
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb)
	defer broker.Close()

	env, err := broker.SendMessage("local", "local", "test", "no redis")
	if err != nil {
		t.Fatalf("SendMessage without redis failed: %v", err)
	}
	if env == nil {
		t.Fatal("expected non-nil envelope")
	}
	inbox, err := broker.ReadInbox("local")
	if err != nil {
		t.Fatalf("ReadInbox failed: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(inbox))
	}
}

func TestMessageBroker_ClosePropagation(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))

	if err := broker.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if !mock.closed.Load() {
		t.Error("expected redis client to be closed")
	}
}

func TestNewID_HostPrefixed(t *testing.T) {
	id1 := newID()
	id2 := newID()
	if id1 >= id2 {
		t.Errorf("expected id1 < id2, got %s >= %s", id1, id2)
	}
	if len(id1) < 3 {
		t.Errorf("id too short: %s", id1)
	}
}

func TestStartSubscriber_ContextCanceled(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	broker.Close()
}

func TestStartSubscriber_BadPayload(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	defer broker.Close()

	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	ps := mock.subs["herd.*"]
	mock.mu.Unlock()

	ps.ch <- &redis.Message{Channel: "herd.alice", Payload: "not-json"}

	time.Sleep(50 * time.Millisecond)

	// Should not crash, bad JSON is silently skipped
	inbox, err := broker.ReadInbox("alice")
	if err != nil {
		t.Fatalf("ReadInbox failed: %v", err)
	}
	if len(inbox) != 0 {
		t.Fatalf("expected 0 envelopes, got %d", len(inbox))
	}
}

func TestStartSubscriber_ChannelClosed(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	defer broker.Close()

	time.Sleep(50 * time.Millisecond)

	mock.mu.Lock()
	ps := mock.subs["herd.*"]
	mock.mu.Unlock()

	// Close the channel to simulate the subscriber goroutine seeing !ok
	close(ps.ch)

	time.Sleep(50 * time.Millisecond)
}
