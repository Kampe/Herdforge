package mail

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-126: ack/retry/dedupe/dead-letter conformance for the durable
// callback consumer.

func TestCallbackConsumer_DrainAck_Basic(t *testing.T) {
	mb := newTestMailbox(t)
	if _, err := mb.PostCallback("herd-smith", Callback{Ref: "FAC-1", Kind: CallbackComplete, SHA: "abc", Repo: "herdforge", LeaseGeneration: 1}); err != nil {
		t.Fatal(err)
	}

	c, err := NewCallbackConsumer(mb, 0)
	if err != nil {
		t.Fatal(err)
	}

	drained, err := c.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 1 {
		t.Fatalf("expected 1 callback, got %d", len(drained))
	}
	if drained[0].Attempt != 1 {
		t.Errorf("expected first delivery attempt=1, got %d", drained[0].Attempt)
	}
	if drained[0].Callback.SenderRole != "herd-smith" {
		t.Errorf("expected SenderRole bound from sender, got %q", drained[0].Callback.SenderRole)
	}

	if err := c.Ack(drained[0].EnvelopeID); err != nil {
		t.Fatal(err)
	}

	// Acked callbacks never reappear on a subsequent drain.
	again, err := c.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("expected acked callback to never redeliver, got %d", len(again))
	}
}

// TestCallbackConsumer_RestartRetry_NoDuplication proves that an unacked
// callback survives a simulated process restart (a brand new
// CallbackConsumer loading the same persisted state file) and is retried
// — with its attempt count correctly carried forward — rather than being
// lost or treated as a fresh delivery that could double-apply.
func TestCallbackConsumer_RestartRetry_NoDuplication(t *testing.T) {
	tmpDir := t.TempDir()
	mb := NewMailbox(filepath.Join(tmpDir, "mail.jsonl"))
	if _, err := mb.PostCallback("herd-smith", Callback{Ref: "FAC-2", Kind: CallbackComplete, Repo: "herdforge", LeaseGeneration: 1}); err != nil {
		t.Fatal(err)
	}

	c1, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c1.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("expected 1 callback at attempt 1, got %+v", first)
	}
	// Simulated crash: no Ack call, c1 is discarded.

	c2, err := NewCallbackConsumer(mb, 5) // "restart": fresh consumer, same mailbox/state file
	if err != nil {
		t.Fatal(err)
	}
	second, err := c2.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("expected the unacked callback to retry after restart, got %d", len(second))
	}
	if second[0].Attempt != 2 {
		t.Fatalf("expected retry attempt count to carry across restart: got %d, want 2", second[0].Attempt)
	}
	if second[0].EnvelopeID != first[0].EnvelopeID {
		t.Fatalf("restart must not treat the retry as a new envelope")
	}

	if err := c2.Ack(second[0].EnvelopeID); err != nil {
		t.Fatal(err)
	}

	c3, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}
	third, err := c3.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(third) != 0 {
		t.Fatalf("acked callback must not resurrect after another restart, got %d", len(third))
	}
}

// TestCallbackConsumer_StaleLeaseGeneration_NeverAdvances proves a callback
// from a superseded lease generation is never delivered once a newer
// generation for the same ref has been acked — it can never advance state.
func TestCallbackConsumer_StaleLeaseGeneration_NeverAdvances(t *testing.T) {
	mb := newTestMailbox(t)
	c, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mb.PostCallback("agent-1", Callback{Ref: "FAC-3", Kind: CallbackComplete, Repo: "herdforge", LeaseGeneration: 2, SHA: "current"}); err != nil {
		t.Fatal(err)
	}
	drained, err := c.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 1 {
		t.Fatalf("expected the gen-2 callback, got %d", len(drained))
	}
	if err := c.Ack(drained[0].EnvelopeID); err != nil {
		t.Fatal(err)
	}

	// A late, stale-lease callback (gen=1) arrives after gen=2 already acked.
	if _, err := mb.PostCallback("agent-1-stale", Callback{Ref: "FAC-3", Kind: CallbackComplete, Repo: "herdforge", LeaseGeneration: 1, SHA: "stale"}); err != nil {
		t.Fatal(err)
	}
	staleDrained, err := c.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(staleDrained) != 0 {
		t.Fatalf("stale lease generation callback must never be delivered, got %+v", staleDrained)
	}

	// A duplicate at the SAME generation/sequence must also never re-deliver.
	dup, err := c.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(dup) != 0 {
		t.Fatalf("expected no redelivery of already-acked generation, got %d", len(dup))
	}
}

// TestCallbackConsumer_DeadLetterAfterMaxRetries proves a callback that is
// never acked is retried up to maxRetries and then permanently dropped to
// the dead-letter file, rather than retried forever or silently vanishing.
func TestCallbackConsumer_DeadLetterAfterMaxRetries(t *testing.T) {
	mb := newTestMailbox(t)
	const maxRetries = 2
	c, err := NewCallbackConsumer(mb, maxRetries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mb.PostCallback("agent-1", Callback{Ref: "FAC-4", Kind: CallbackBlocked, Detail: "stuck"}); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= maxRetries; attempt++ {
		drained, err := c.Drain()
		if err != nil {
			t.Fatal(err)
		}
		if len(drained) != 1 {
			t.Fatalf("attempt %d: expected redelivery while under maxRetries, got %d", attempt, len(drained))
		}
		if drained[0].Attempt != attempt {
			t.Errorf("attempt %d: expected Attempt=%d, got %d", attempt, attempt, drained[0].Attempt)
		}
	}

	// One more drain pushes attempts past maxRetries -> dead-lettered.
	final, err := c.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 0 {
		t.Fatalf("expected the callback to be dead-lettered, not redelivered, got %d", len(final))
	}

	stats, err := c.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLetters != 1 {
		t.Fatalf("expected 1 dead letter, got %d", stats.DeadLetters)
	}
	if stats.PendingCount != 0 {
		t.Fatalf("dead-lettered callback must be cleared from pending, got %d", stats.PendingCount)
	}
}

func TestCallbackConsumer_Stats(t *testing.T) {
	mb := newTestMailbox(t)
	c, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := mb.PostCallback("agent-1", Callback{Ref: "FAC-5", Kind: CallbackComplete, Repo: "herdforge", LeaseGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	drained, err := c.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != 1 {
		t.Fatalf("expected 1 drained callback, got %d", len(drained))
	}

	stats, err := c.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingCount != 1 {
		t.Errorf("expected 1 pending, got %d", stats.PendingCount)
	}
	if stats.QueueAge <= 0 {
		t.Error("expected a positive queue age for the pending callback")
	}

	if err := c.Ack(drained[0].EnvelopeID); err != nil {
		t.Fatal(err)
	}
	stats, err = c.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.PendingCount != 0 {
		t.Errorf("expected 0 pending after ack, got %d", stats.PendingCount)
	}
	if stats.LastConsumedSeq != drained[0].Sequence {
		t.Errorf("expected LastConsumedSeq %d, got %d", drained[0].Sequence, stats.LastConsumedSeq)
	}
}

func TestCallbackConsumer_AckUnknownEnvelope(t *testing.T) {
	mb := newTestMailbox(t)
	c, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ack("does-not-exist"); err == nil {
		t.Fatal("expected an error acking an unknown envelope")
	}
}

// TestCallbackConsumer_DeadLetterDurableBeforeStateSave_CrashConsistency is
// the FAC-126 review-rejection regression for finding 3: Drain must write
// the dead-letter record BEFORE clearing/saving the pending entry, so a
// crash or failure in between never silently loses the callback and never
// resets its attempt count back to 1 on the next Drain (which would let a
// perpetually-retrying callback dodge maxRetries forever).
//
// The state file's directory is made read-only after the dead-letter file
// is pre-created, so writeFileAtomic's temp-file creation for
// callback-state.json fails (needs a new directory entry) while appending
// to the already-existing dead-letter file still succeeds (appending to an
// existing file needs no directory write permission) — isolating exactly
// the failure window the fix targets.
func TestCallbackConsumer_DeadLetterDurableBeforeStateSave_CrashConsistency(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "mail.jsonl")
	mb := NewMailbox(mailFile)
	if _, err := mb.PostCallback("agent-1", Callback{Ref: "FAC-6", Kind: CallbackBlocked, Detail: "stuck"}); err != nil {
		t.Fatal(err)
	}

	const maxRetries = 1
	c, err := NewCallbackConsumer(mb, maxRetries)
	if err != nil {
		t.Fatal(err)
	}

	first, err := c.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Attempt != 1 {
		t.Fatalf("expected attempt 1, got %+v", first)
	}

	deadPath := mailFile + ".dead-letters.jsonl"
	if err := os.WriteFile(deadPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmpDir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(tmpDir, 0755) })

	// Attempt 2 pushes attempts past maxRetries -> dead-letter path; the
	// state save that follows is forced to fail by the read-only directory.
	_, err = c.Drain()
	if err == nil {
		t.Fatal("expected Drain to surface the forced state-save failure")
	}

	data, rErr := os.ReadFile(deadPath)
	if rErr != nil {
		t.Fatalf("expected the dead-letter file to still exist: %v", rErr)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		t.Fatal("dead-letter record must be durable even when the subsequent state save fails")
	}

	os.Chmod(tmpDir, 0755)

	// A fresh consumer ("restart") loads the last successfully-saved state
	// (from attempt 1, since attempt 2's save never completed) — it must
	// still see the callback as pending, continuing the count from where
	// the disk left off, never resetting to a fresh attempt 1.
	c2, err := NewCallbackConsumer(mb, maxRetries)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c2.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("expected the retried-and-exceeded callback to dead-letter again, not redeliver as fresh: %+v", second)
	}

	stats, err := c2.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.DeadLetters < 1 {
		t.Fatalf("expected the durable dead-letter record to have survived the crash, got %d", stats.DeadLetters)
	}
	if stats.PendingCount != 0 {
		t.Fatalf("expected pending cleared once the restart-drain's dead-letter+save succeeded, got %d", stats.PendingCount)
	}
}
