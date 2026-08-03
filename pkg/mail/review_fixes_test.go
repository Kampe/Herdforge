package mail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAC-126 review rejection (bound to 3124c82) — regressions for the three
// findings: (1) durable outbox/replay for Redis publish failures, (2)
// fail-closed propagation/retention for quarantine and error-channel
// overflow, (3) crash-consistent dead-lettering (see callback_consumer_test.go).

// TestOutbox_ReplayOnRestartAfterPublishFailure is the failure-injection +
// restart test finding 1 asked for: a publish failure must not permanently
// lose the Redis fan-out. The message stays in a durable outbox until a
// later broker (simulating a restarted process, pointed at the same
// mailbox file) successfully replays it.
func TestOutbox_ReplayOnRestartAfterPublishFailure(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")

	failing := newMockRedisClient()
	failing.setPublishErr(fmt.Errorf("redis: connection refused"))
	broker1 := NewMessageBroker(NewMailbox(mailFile), WithRedis(failing, "herd"))

	env, err := broker1.SendMessage("alice", "bob", "Hello", "World")
	if err == nil {
		t.Fatal("expected the publish failure to surface")
	}
	if env == nil {
		t.Fatal("local durability must not be lost just because the publish failed")
	}
	broker1.Close()

	outboxPath := mailFile + ".outbox.json"
	raw, err := os.ReadFile(outboxPath)
	if err != nil {
		t.Fatalf("expected a durable outbox record after publish failure: %v", err)
	}
	if !strings.Contains(string(raw), env.ID) {
		t.Fatalf("outbox record missing envelope %s: %s", env.ID, raw)
	}

	// "Redis recovers" and the process "restarts": a fresh broker/mailbox
	// handle pointed at the same durable mailbox file, backed by a healthy
	// mock (a fresh mock plays the role of "the same external Redis is now
	// reachable" — the durability being tested lives in the outbox FILE,
	// not in any in-memory mock state).
	healthy := newMockRedisClient()
	broker2 := NewMessageBroker(NewMailbox(mailFile), WithRedis(healthy, "herd"))
	defer broker2.Close()

	time.Sleep(50 * time.Millisecond) // let the constructor's replay + subscriber settle

	healthy.mu.Lock()
	pubCount := len(healthy.published)
	healthy.mu.Unlock()
	if pubCount < 1 {
		t.Fatalf("expected startup outbox replay to publish the pending message, got %d publishes", pubCount)
	}

	if raw, err := os.ReadFile(outboxPath); err == nil && strings.Contains(string(raw), env.ID) {
		t.Fatalf("expected the outbox entry to be cleared after successful replay, still present: %s", raw)
	}
}

// TestOutbox_FlushOutboxIsIdempotentWithRedelivery proves that even if the
// outbox is flushed twice for the same entry (e.g. two overlapping replay
// attempts), the receiving mailbox only ever records the message once —
// idempotent publish, not just idempotent bookkeeping.
func TestOutbox_FlushOutboxIsIdempotentWithRedelivery(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")
	mock := newMockRedisClient()
	mock.setPublishErr(fmt.Errorf("redis: connection refused"))

	broker := NewMessageBroker(NewMailbox(mailFile), WithRedis(mock, "herd"))
	defer broker.Close()

	env, err := broker.SendMessage("alice", "bob", "Hello", "World")
	if err == nil {
		t.Fatal("expected publish failure")
	}

	mock.setPublishErr(nil)
	if _, err := broker.FlushOutbox(); err != nil {
		t.Fatalf("first flush failed: %v", err)
	}
	if _, err := broker.FlushOutbox(); err != nil {
		t.Fatalf("second flush failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	envs, err := broker.ReadInbox("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 {
		t.Fatalf("expected exactly 1 record despite double replay, got %d", len(envs))
	}
	if envs[0].ID != env.ID {
		t.Errorf("unexpected envelope: %+v", envs[0])
	}
}

// TestQuarantine_SinkFailurePropagates is the fail-closed regression for
// finding 2: if the quarantine sink itself can't be written, ReadInbox must
// surface that as an error instead of silently pretending the malformed
// record was preserved.
func TestQuarantine_SinkFailurePropagates(t *testing.T) {
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
	f.WriteString("{not valid json\n")
	f.Close()

	// Make the quarantine sink itself unwritable: put a directory where the
	// quarantine file needs to go.
	if err := os.MkdirAll(mailFile+".quarantine.jsonl", 0755); err != nil {
		t.Fatal(err)
	}

	envs, err := mb.ReadInbox("b")
	if err == nil {
		t.Fatal("expected ReadInbox to surface the quarantine sink failure")
	}
	if len(envs) != 1 {
		t.Fatalf("a quarantine-sink failure must not lose the successfully-parsed envelopes: got %d", len(envs))
	}
}

// TestErrs_SaturationDurablyRetained is the fail-closed regression for
// finding 2's other half: an error that overflows the bounded Errs()
// channel must never vanish — it lands in <MailFile>.errors.jsonl and
// DroppedErrCount reflects it.
func TestErrs_SaturationDurablyRetained(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")
	mock := newMockRedisClient()
	mock.setPublishErr(fmt.Errorf("redis: connection refused"))
	broker := NewMessageBroker(NewMailbox(mailFile), WithRedis(mock, "herd"))
	defer broker.Close()

	// Never drain Errs() — every publish failure below has to go somewhere.
	const attempts = 20 // > the 16-entry buffer
	for i := 0; i < attempts; i++ {
		if _, err := broker.SendMessage("alice", fmt.Sprintf("r%d", i), "s", "b"); err == nil {
			t.Fatal("expected every publish to fail")
		}
	}

	if broker.DroppedErrCount() == 0 {
		t.Fatal("expected some errors to overflow the buffered channel and be counted as dropped")
	}

	data, err := os.ReadFile(mailFile + ".errors.jsonl")
	if err != nil {
		t.Fatalf("expected a durable error log for overflowed errors: %v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		t.Fatal("expected at least one durably-retained error entry")
	}
}
