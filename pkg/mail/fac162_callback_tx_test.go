package mail

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestCallbackConsumer_AckFails_StateUnchanged proves Ack is transactional:
// if the durable save cannot complete, Pending/Acked must not advance in
// the live process (same truth as disk / restart).
func TestCallbackConsumer_AckFails_StateUnchanged(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "ack-tx.jsonl")
	mb := NewMailbox(mailFile)
	if _, err := mb.PostCallback("a", Callback{Ref: "FAC-X", Kind: CallbackComplete, Repo: "r", LeaseGeneration: 1, SHA: "s"}); err != nil {
		t.Fatal(err)
	}
	c, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}
	drained, err := c.Drain()
	if err != nil || len(drained) != 1 {
		t.Fatalf("drain setup: %v %#v", err, drained)
	}
	id := drained[0].EnvelopeID
	if _, ok := c.state.Pending[id]; !ok {
		t.Fatal("expected pending after drain")
	}

	rel := holdMailboxLock(t, mailFile)
	defer rel()

	ctx, cancel, h := withCancelOnPoll(2)
	defer cancel()
	setTestHooks(h)
	defer clearTestHooks()

	err = c.AckContext(ctx, id)
	if err == nil {
		t.Fatal("expected Ack fail-closed under held lock")
	}
	if !errors.Is(err, ErrMailboxLockTimeout) {
		t.Fatalf("want BLOCKED, got %v", err)
	}
	if _, ok := c.state.Pending[id]; !ok {
		t.Fatal("Pending removed despite failed Ack — non-transactional mutation")
	}
	if _, ok := c.state.Acked[ackKey("r", "FAC-X")]; ok {
		t.Fatal("Acked advanced despite failed Ack")
	}

	// Restart must still see pending (disk unchanged).
	rel()
	c2, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c2.state.Pending[id]; !ok {
		// Pending is on disk only after drain write; Ack failed so disk still has it.
		t.Fatal("restart lost pending after failed Ack")
	}
}

// TestCallbackConsumer_DrainFails_StateUnchanged: Drain that cannot save
// must not leave incremented Attempts in live memory.
func TestCallbackConsumer_DrainFails_StateUnchanged(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "drain-tx.jsonl")
	mb := NewMailbox(mailFile)
	if _, err := mb.PostCallback("a", Callback{Ref: "FAC-Y", Kind: CallbackComplete, Repo: "r", LeaseGeneration: 1, SHA: "s"}); err != nil {
		t.Fatal(err)
	}
	c, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}

	rel := holdMailboxLock(t, mailFile)
	defer rel()

	ctx, cancel, h := withCancelOnPoll(2)
	defer cancel()
	setTestHooks(h)
	defer clearTestHooks()

	out, err := c.DrainContext(ctx)
	if err == nil {
		t.Fatal("Drain must fail under held lock")
	}
	if out != nil {
		t.Fatalf("failed Drain must not return results: %#v", out)
	}
	if len(c.state.Pending) != 0 {
		t.Fatalf("Pending mutated on failed Drain: %#v", c.state.Pending)
	}
}

// TestCallbackConsumer_ConcurrentConsumers_NoLostUpdate proves cross-process
// lock transactions: two OS processes each ack a disjoint half of pending
// callbacks. Goroutine-only instances cannot prove OS flock serialization.
func TestCallbackConsumer_ConcurrentConsumers_NoLostUpdate(t *testing.T) {
	if os.Getenv("MAIL_CB_CHILD") == "1" {
		mailFile := os.Getenv("MAIL_CB_FILE")
		ids := strings.Split(os.Getenv("MAIL_CB_IDS"), ",")
		mb := NewMailbox(mailFile)
		c, err := NewCallbackConsumer(mb, 5)
		if err != nil {
			fmt.Fprintf(os.Stderr, "child consumer: %v\n", err)
			os.Exit(2)
		}
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if err := c.Ack(id); err != nil {
				fmt.Fprintf(os.Stderr, "child ack %s: %v\n", id, err)
				os.Exit(3)
			}
		}
		os.Exit(0)
	}

	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "conc.jsonl")
	mb := NewMailbox(mailFile)
	const n = 8
	for i := 0; i < n; i++ {
		if _, err := mb.PostCallback("a", Callback{
			Ref: fmt.Sprintf("FAC-%d", i), Kind: CallbackComplete, Repo: "r",
			LeaseGeneration: 1, SHA: "s",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cSetup, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}
	drained, err := cSetup.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(drained) != n {
		t.Fatalf("want %d drained, got %d", n, len(drained))
	}
	ids := make([]string, 0, n)
	for _, d := range drained {
		ids = append(ids, d.EnvelopeID)
	}
	mid := n / 2
	halfA := strings.Join(ids[:mid], ",")
	halfB := strings.Join(ids[mid:], ",")

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runChild := func(idList string) error {
		cmd := exec.Command(exe, "-test.run=^TestCallbackConsumer_ConcurrentConsumers_NoLostUpdate$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"MAIL_CB_CHILD=1",
			"MAIL_CB_FILE="+mailFile,
			"MAIL_CB_IDS="+idList,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v\n%s", err, out)
		}
		return nil
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, half := range []string{halfA, halfB} {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			errCh <- runChild(h)
		}(half)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	final, err := NewCallbackConsumer(mb, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.state.Pending) != 0 {
		t.Fatalf("lost-update left pending: %#v", final.state.Pending)
	}
	if len(final.state.Acked) != n {
		t.Fatalf("acked marks %d want %d", len(final.state.Acked), n)
	}
	again, err := final.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("acked callbacks redelivered: %d", len(again))
	}
}

// TestCallbackConsumer_DeadLetterFail_KeepsPending: if dead-letter append
// fails inside the drain transaction, pending must remain for retry.
func TestCallbackConsumer_DeadLetterFail_KeepsPending(t *testing.T) {
	tmpDir := t.TempDir()
	mailFile := filepath.Join(tmpDir, "dl-fail.jsonl")
	mb := NewMailbox(mailFile)
	// maxRetries=1 so second drain attempt after first succeeds would DL —
	// instead inject append failure on first exhaust path.
	c, err := NewCallbackConsumer(mb, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mb.PostCallback("a", Callback{Ref: "FAC-DL", Kind: CallbackComplete, Repo: "r", LeaseGeneration: 1, SHA: "s"}); err != nil {
		t.Fatal(err)
	}
	// First drain: attempt 1, under maxRetries.
	d1, err := c.Drain()
	if err != nil || len(d1) != 1 {
		t.Fatalf("first drain: %v %#v", err, d1)
	}
	// Second drain without ack: attempt 2 > maxRetries → dead-letter path.
	// Make deadPath a directory so appendLine fails.
	if err := os.RemoveAll(mb.MailFile + ".dead-letters.jsonl"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(mb.MailFile+".dead-letters.jsonl", 0755); err != nil {
		t.Fatal(err)
	}
	d2, err := c.Drain()
	if err == nil {
		t.Fatal("expected dead-letter failure")
	}
	if d2 != nil {
		t.Fatalf("must not return results on failed DL txn: %#v", d2)
	}
	// Live pending must still reflect pre-transaction state (attempt 1).
	if p := c.state.Pending[d1[0].EnvelopeID]; p == nil || p.Attempts != 1 {
		t.Fatalf("pending after failed DL txn: %#v", c.state.Pending)
	}
	// Restart same truth.
	c2, err := NewCallbackConsumer(mb, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p := c2.state.Pending[d1[0].EnvelopeID]; p == nil || p.Attempts != 1 {
		t.Fatalf("disk pending after failed DL: %#v", c2.state.Pending)
	}
}
