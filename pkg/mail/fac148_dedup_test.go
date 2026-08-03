package mail

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// FAC-148 finding 2: the bounded 4096-entry in-memory dedup cache silently
// evicted old IDs, so redelivery of an envelope older than the cache window
// wrote a real duplicate to the mailbox file.
// TestDedup_RedeliveryOlderThanCacheWindowStillIdempotent seeds a mailbox
// file with one target envelope plus exactly maxSeenIDs newer envelopes
// (guaranteeing the target is evicted from a fresh process's reloaded
// cache), then redelivers the target's ID and proves no duplicate lands on
// disk. This is a mutation probe: it fails against the pre-fix code, which
// has no durable fallback once the in-memory cache evicts an ID.
func TestDedup_RedeliveryOlderThanCacheWindowStillIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")

	var sb strings.Builder
	writeLine := func(env Envelope) {
		line, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(line)
		sb.WriteByte('\n')
	}
	writeLine(Envelope{ID: "old-envelope", Sequence: 1, Sender: "carol", Recipient: "alice", Subject: "hi", Body: "b", Timestamp: time.Now()})
	for i := 0; i < maxSeenIDs; i++ {
		writeLine(Envelope{ID: fmt.Sprintf("filler-%d", i), Sequence: int64(i + 2), Sender: "carol", Recipient: "alice", Subject: "s", Body: "b", Timestamp: time.Now()})
	}
	if err := os.WriteFile(mailFile, []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mailFile+".seq", []byte(strconv.Itoa(maxSeenIDs+1)), 0644); err != nil {
		t.Fatal(err)
	}

	mb := NewMailbox(mailFile) // fresh handle: simulates a process restart
	redelivered := &Envelope{ID: "old-envelope", Sender: "carol", Recipient: "alice", Subject: "hi", Body: "b", Timestamp: time.Now()}
	if err := mb.appendEnvelope(redelivered); err != nil {
		t.Fatalf("redelivery append failed: %v", err)
	}

	envs, err := mb.ReadInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != maxSeenIDs+1 {
		t.Fatalf("redelivery of an envelope older than the %d-entry dedup cache must not append a new record: expected %d envelopes, got %d", maxSeenIDs, maxSeenIDs+1, len(envs))
	}
	count := 0
	for _, e := range envs {
		if e.ID == "old-envelope" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 copy of the redelivered envelope, found %d", count)
	}
}
