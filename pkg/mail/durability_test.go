package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// FAC-126: durability, dedup, and observability conformance tests.

// TestConcurrentWriters_NoInterleaveNoDuplicate simulates multiple
// concurrent processes (separate Mailbox handles, same underlying file,
// each opening its own fd for the flock) hammering SendMessage at once.
// Every record must parse cleanly (no interleaved corruption), every ID
// must be unique, and the assigned sequence numbers must be an exact,
// gap-free permutation of 1..total — proving cross-process writers never
// clobber each other.
func TestConcurrentWriters_NoInterleaveNoDuplicate(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")

	const writers = 8
	const perWriter = 25
	const total = writers * perWriter

	var wg sync.WaitGroup
	errCh := make(chan error, total)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			mb := NewMailbox(mailFile) // separate handle per "process"
			for i := 0; i < perWriter; i++ {
				if _, err := mb.SendMessage(fmt.Sprintf("writer-%d", w), "all", "msg", "body"); err != nil {
					errCh <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent SendMessage failed: %v", err)
	}

	reader := NewMailbox(mailFile)
	envs, err := reader.ReadInbox("")
	if err != nil {
		t.Fatalf("ReadInbox failed: %v", err)
	}
	if len(envs) != total {
		t.Fatalf("expected %d envelopes, got %d (quarantined=%d)", total, len(envs), reader.QuarantineCount())
	}
	if reader.QuarantineCount() != 0 {
		t.Fatalf("expected 0 quarantined lines from concurrent writers, got %d", reader.QuarantineCount())
	}

	ids := make(map[string]bool, total)
	seqs := make(map[int64]bool, total)
	for _, e := range envs {
		if ids[e.ID] {
			t.Fatalf("duplicate envelope ID %s", e.ID)
		}
		ids[e.ID] = true
		if e.Sequence <= 0 {
			t.Fatalf("envelope %s missing a monotonic sequence", e.ID)
		}
		if seqs[e.Sequence] {
			t.Fatalf("duplicate sequence number %d", e.Sequence)
		}
		seqs[e.Sequence] = true
	}
	for i := int64(1); i <= int64(total); i++ {
		if !seqs[i] {
			t.Fatalf("sequence %d missing — gap in monotonic ordering", i)
		}
	}
}

// TestQuarantine_MalformedLineSurfaced proves malformed records are
// quarantined and surfaced, not silently skipped.
func TestQuarantine_MalformedLineSurfaced(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "herd-mail.jsonl")
	mb := NewMailbox(mailFile)

	if _, err := mb.SendMessage("a", "b", "ok", "body"); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(mailFile, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{this is not valid json\n")
	f.Close()

	envs, err := mb.ReadInbox("b")
	if err != nil {
		t.Fatalf("ReadInbox should not fail on malformed lines: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("expected 1 valid envelope, got %d", len(envs))
	}
	if mb.QuarantineCount() != 1 {
		t.Fatalf("expected 1 quarantined line surfaced, got %d", mb.QuarantineCount())
	}

	data, err := os.ReadFile(mailFile + ".quarantine.jsonl")
	if err != nil {
		t.Fatalf("expected a durable quarantine file: %v", err)
	}
	var entry QuarantineEntry
	if err := json.Unmarshal(data[:len(data)-1], &entry); err != nil {
		t.Fatalf("quarantine file should contain a valid QuarantineEntry: %v", err)
	}
	if entry.Line != "{this is not valid json" {
		t.Errorf("quarantine entry lost the offending line: %+v", entry)
	}
	if entry.Reason == "" {
		t.Error("expected a non-empty quarantine reason")
	}
}

// TestBroker_SelfEchoDeduped is the direct regression test for the bug this
// ticket's audit flagged: a broker always subscribes to its own publish
// channel pattern, so without dedup a SendMessage would loop back through
// the subscriber and duplicate itself in the local mailbox file.
func TestBroker_SelfEchoDeduped(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")
	mb := NewMailbox(mailFile)
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	defer broker.Close()

	time.Sleep(50 * time.Millisecond) // let the subscriber register its pattern

	if _, err := broker.SendMessage("alice", "bob", "Hello", "World"); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the self-echo (if any) land

	raw, err := os.ReadFile(mailFile)
	if err != nil {
		t.Fatalf("failed to read mailbox file: %v", err)
	}
	lines := splitLines(string(raw))
	nonEmpty := 0
	for _, l := range lines {
		if l != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 1 {
		t.Fatalf("expected exactly 1 record (self-echo must be deduped), got %d: %q", nonEmpty, string(raw))
	}
}

// TestBroker_RedeliveryIdempotent proves that relaying the exact same
// envelope through the Redis path twice (simulating at-least-once
// redelivery) writes it to the local mailbox only once.
func TestBroker_RedeliveryIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))

	env := &Envelope{ID: "host-remote-1", Sender: "carol", Recipient: "alice", Subject: "hi", Body: "b", Timestamp: time.Now()}
	if err := mb.appendEnvelope(env); err != nil {
		t.Fatalf("first relay failed: %v", err)
	}
	firstSeq := env.Sequence

	// Redeliver: a fresh copy decoded off the wire again, same ID.
	redelivered := &Envelope{ID: "host-remote-1", Sender: "carol", Recipient: "alice", Subject: "hi", Body: "b", Timestamp: time.Now()}
	if err := mb.appendEnvelope(redelivered); err != nil {
		t.Fatalf("redelivery relay failed: %v", err)
	}

	envs, err := mb.ReadInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 {
		t.Fatalf("redelivery must be idempotent: expected 1 envelope, got %d", len(envs))
	}
	if envs[0].Sequence != firstSeq {
		t.Errorf("redelivered duplicate must not consume a new sequence: got %d, want %d", envs[0].Sequence, firstSeq)
	}
}

// TestBroker_RestartSeenReloadsFromDisk proves the in-memory dedup set is
// rebuilt from the mailbox file on first use, so a redelivery arriving
// after a process restart is still deduped rather than double-written.
func TestBroker_RestartSeenReloadsFromDisk(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")

	first := NewMailbox(mailFile)
	env := &Envelope{ID: "host-dur-1", Sender: "carol", Recipient: "alice", Subject: "hi", Body: "b", Timestamp: time.Now()}
	if err := first.appendEnvelope(env); err != nil {
		t.Fatal(err)
	}

	// Simulate a process restart: a brand new Mailbox handle, no shared
	// in-memory state with `first`.
	restarted := NewMailbox(mailFile)
	redelivered := &Envelope{ID: "host-dur-1", Sender: "carol", Recipient: "alice", Subject: "hi", Body: "b", Timestamp: time.Now()}
	if err := restarted.appendEnvelope(redelivered); err != nil {
		t.Fatal(err)
	}

	envs, err := restarted.ReadInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 {
		t.Fatalf("post-restart redelivery must still be deduped: expected 1 envelope, got %d", len(envs))
	}
}

// TestPublishError_PropagatesToCaller proves publish failures are no
// longer swallowed: SendMessage must return the error, and it must also
// land on Errs() for callers monitoring asynchronously.
func TestPublishError_PropagatesToCaller(t *testing.T) {
	mock := newMockRedisClient()
	mock.setPublishErr(fmt.Errorf("redis: connection refused"))
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	defer broker.Close()

	env, err := broker.SendMessage("alice", "bob", "Hello", "World")
	if err == nil {
		t.Fatal("expected SendMessage to propagate the publish error")
	}
	if env == nil {
		t.Fatal("local durability must not be lost just because fan-out failed")
	}

	select {
	case gotErr := <-broker.Errs():
		if gotErr == nil {
			t.Fatal("expected a non-nil error on Errs()")
		}
	case <-time.After(time.Second):
		t.Fatal("expected the publish failure to surface on Errs()")
	}
}

// TestSubscribeChannelClosed_PropagatesErr proves a dead subscription
// surfaces on Errs() instead of failing silently.
func TestSubscribeChannelClosed_PropagatesErr(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	defer broker.Close()

	time.Sleep(50 * time.Millisecond)
	mock.mu.Lock()
	ps := mock.subs["herd.*"]
	mock.mu.Unlock()
	if ps == nil {
		t.Fatal("subscriber should have registered pattern herd.*")
	}
	close(ps.ch)

	select {
	case err := <-broker.Errs():
		if err == nil {
			t.Fatal("expected a non-nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("expected channel closure to surface on Errs()")
	}
}

// TestPSubscribe_PatternMatchesMultipleChannels proves the broker
// subscribes with a genuine wildcard pattern (PSubscribe), so publishes on
// any per-recipient channel under the prefix are received — not just a
// literal channel string.
func TestPSubscribe_PatternMatchesMultipleChannels(t *testing.T) {
	mock := newMockRedisClient()
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	broker := NewMessageBroker(mb, WithRedis(mock, "herd"))
	defer broker.Close()

	time.Sleep(50 * time.Millisecond)

	for _, recipient := range []string{"alice", "bob", "carol"} {
		env := &Envelope{ID: "remote-" + recipient, Sender: "dave", Recipient: recipient, Subject: "s", Body: "b", Timestamp: time.Now()}
		data, _ := json.Marshal(env)
		if err := mock.Publish(context.Background(), "herd."+recipient, data).Err(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	for _, recipient := range []string{"alice", "bob", "carol"} {
		envs, err := broker.ReadInbox(recipient)
		if err != nil {
			t.Fatal(err)
		}
		if len(envs) != 1 {
			t.Errorf("expected 1 relayed envelope for %s via pattern subscription, got %d", recipient, len(envs))
		}
	}
}

// TestConformance_FileOnlyAndRedisModes runs the same send/read/dedup
// workload against a plain file-only Mailbox and a Redis-backed
// MessageBroker, asserting both observe identical durability behavior.
func TestConformance_FileOnlyAndRedisModes(t *testing.T) {
	type sender interface {
		SendMessage(sender, recipient, subject, body string) (*Envelope, error)
		ReadInbox(recipient string) ([]*Envelope, error)
	}

	newFileOnly := func(t *testing.T, mailFile string) sender {
		return NewMailbox(mailFile)
	}
	newRedisBacked := func(t *testing.T, mailFile string) sender {
		mock := newMockRedisClient()
		broker := NewMessageBroker(NewMailbox(mailFile), WithRedis(mock, "herd"))
		t.Cleanup(func() { broker.Close() })
		time.Sleep(20 * time.Millisecond)
		return broker
	}

	modes := []struct {
		name string
		new  func(t *testing.T, mailFile string) sender
	}{
		{"file-only", newFileOnly},
		{"redis-backed", newRedisBacked},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			s := mode.new(t, filepath.Join(tmpDir, "mail.jsonl"))

			for i := 0; i < 5; i++ {
				if _, err := s.SendMessage("agent", "coordinator", fmt.Sprintf("subj-%d", i), "body"); err != nil {
					t.Fatalf("SendMessage failed: %v", err)
				}
			}

			envs, err := s.ReadInbox("coordinator")
			if err != nil {
				t.Fatalf("ReadInbox failed: %v", err)
			}
			if len(envs) != 5 {
				t.Fatalf("expected 5 envelopes, got %d", len(envs))
			}
			seqs := make(map[int64]bool)
			for _, e := range envs {
				if seqs[e.Sequence] {
					t.Fatalf("duplicate sequence %d in %s mode", e.Sequence, mode.name)
				}
				seqs[e.Sequence] = true
			}
		})
	}
}
