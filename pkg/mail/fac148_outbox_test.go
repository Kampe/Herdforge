package mail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAC-148 finding 1: SendMessage fsync'd the mailbox append before
// addOutboxEntry. TestSendMessage_OutboxWrittenBeforeMailboxAppend proves
// the outbox entry is durable even when the local mailbox append itself
// fails, which is only possible if the outbox write happens first. Under
// the old mailbox-first ordering, b.mb.SendMessage would fail immediately
// and the broker would return (nil, err) before ever touching the outbox —
// this test would panic on a nil envelope.
func TestSendMessage_OutboxWrittenBeforeMailboxAppend(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")
	if err := os.Mkdir(mailFile, 0755); err != nil { // mail file path is a directory -> local append fails
		t.Fatal(err)
	}

	mock := newMockRedisClient()
	broker := NewMessageBroker(NewMailbox(mailFile), WithRedis(mock, "herd"))
	defer broker.Close()

	env, err := broker.SendMessage("alice", "bob", "Hello", "World")
	if err == nil {
		t.Fatal("expected the local mailbox append to fail (mail file path is a directory)")
	}
	if env == nil {
		t.Fatal("expected a non-nil envelope: the outbox entry must be written before the local append is attempted")
	}

	raw, err := os.ReadFile(mailFile + ".outbox.json")
	if err != nil {
		t.Fatalf("expected the outbox entry to be durable despite the failed mailbox append: %v", err)
	}
	if !strings.Contains(string(raw), env.ID) {
		t.Fatalf("outbox record missing envelope %s: %s", env.ID, raw)
	}
}

// TestOutbox_CrashBetweenOutboxWriteAndMailboxAppend_RecoversOnRestart
// simulates a crash landing exactly between the two durable writes: the
// outbox entry exists, but the local mailbox file was never appended to. A
// restarted broker (FlushOutbox + this broker's own subscription self-echo)
// must still recover the message into the local mailbox.
func TestOutbox_CrashBetweenOutboxWriteAndMailboxAppend_RecoversOnRestart(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")

	env := newEnvelope("alice", "bob", "Hello", "World")
	channel := "herd." + env.Recipient
	entries := map[string]outboxEntry{env.ID: {Envelope: *env, Channel: channel}}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(mailFile+".outbox.json", data, 0644); err != nil {
		t.Fatal(err)
	}

	mock := newMockRedisClient()
	broker := NewMessageBroker(NewMailbox(mailFile), WithRedis(mock, "herd"))
	defer broker.Close()

	time.Sleep(50 * time.Millisecond) // let startup replay + self-echo settle

	envs, err := broker.ReadInbox("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 {
		t.Fatalf("expected the crashed-mid-write message recovered into the local mailbox, got %d envelopes", len(envs))
	}
	if envs[0].ID != env.ID {
		t.Errorf("recovered envelope has wrong ID: got %s want %s", envs[0].ID, env.ID)
	}

	if raw, err := os.ReadFile(mailFile + ".outbox.json"); err == nil && strings.Contains(string(raw), env.ID) {
		t.Fatalf("expected the outbox entry cleared after recovery, still present: %s", raw)
	}
}

// FAC-148 finding 4: persistDroppedErr ignored the error from its own
// durable write, so a saturated Errs() channel plus a failing errors.jsonl
// sink silently lost the error entirely.
// TestPersistenceFailure_ObservableFailClosed forces that write to fail (by
// putting a directory where the errors.jsonl file needs to go) and proves
// the failure is durably observable via
// PersistenceFailed/PersistenceFailureCount/LastPersistenceError instead of
// vanishing.
func TestPersistenceFailure_ObservableFailClosed(t *testing.T) {
	mock := newMockRedisClient()
	mock.setPublishErr(fmt.Errorf("redis: connection refused"))
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")
	broker := NewMessageBroker(NewMailbox(mailFile), WithRedis(mock, "herd"))
	defer broker.Close()

	// Make the errors.jsonl sink itself unwritable.
	if err := os.MkdirAll(mailFile+".errors.jsonl", 0755); err != nil {
		t.Fatal(err)
	}

	if broker.PersistenceFailed() {
		t.Fatal("persistence should not report failed before any dropped error was attempted")
	}

	// Saturate Errs() (buffer size 16) so subsequent errors take the
	// persistDroppedErr path, which will fail because the sink is a dir.
	const attempts = 20
	for i := 0; i < attempts; i++ {
		if _, err := broker.SendMessage("alice", fmt.Sprintf("r%d", i), "s", "b"); err == nil {
			t.Fatal("expected every publish to fail")
		}
	}

	if !broker.PersistenceFailed() {
		t.Fatal("expected the errors.jsonl write failure to be durably observable via PersistenceFailed")
	}
	if broker.PersistenceFailureCount() == 0 {
		t.Fatal("expected a non-zero PersistenceFailureCount")
	}
	if broker.LastPersistenceError() == nil {
		t.Fatal("expected a non-nil LastPersistenceError")
	}
	if broker.DroppedErrCount() == 0 {
		t.Fatal("expected DroppedErrCount to still reflect every overflowed error even though retention failed")
	}
}
