package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/feedback"
	"github.com/Kampe/Herdforge/pkg/mail"
)

// Bounded inbox paging. The two stores number records with INDEPENDENT
// counters, so every fixture here deliberately gives them COLLIDING values:
// control sequences 1..3 and feedback ids 1..3 for the same recipient. A
// single shared high-water mark passes the easy cases and loses data on
// exactly this shape.

const boundsRecipient = "lane-under-test"

// boundsFixture writes a control mailbox and a feedback store whose sequence
// spaces overlap, and points the feedback resolver at the temp tree.
func boundsFixture(t *testing.T, controlSeqs []int64, feedbackIDs []int64) *mail.Mailbox {
	t.Helper()
	root := t.TempDir()
	mailFile := filepath.Join(root, "control-mail.jsonl")
	var lines []string
	for _, seq := range controlSeqs {
		lines = append(lines, fmt.Sprintf(
			`{"id":"c-%d","seq":%d,"sender":"root","recipient":%q,"subject":"control %d","body":"control body %d","read":false,"timestamp":"2026-09-12T00:00:00Z"}`,
			seq, seq, boundsRecipient, seq, seq))
	}
	if err := os.WriteFile(mailFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	feedbackDir := filepath.Join(root, "feedback-mail")
	if err := os.MkdirAll(feedbackDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var fb []string
	for _, id := range feedbackIDs {
		fb = append(fb, fmt.Sprintf(
			`{"id":%d,"type":"message","from":"coordinator","to":%q,"timestamp":"2026-09-12T00:00:00Z","summary":"feedback %d","message":"feedback body %d"}`,
			id, boundsRecipient, id, id))
	}
	if len(fb) > 0 {
		if err := os.WriteFile(filepath.Join(feedbackDir, boundsRecipient+".jsonl"), []byte(strings.Join(fb, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(feedback.EnvMailDir, feedbackDir)
	return mail.NewMailbox(mailFile)
}

func boundsPage(t *testing.T, box *mail.Mailbox, cursor string, limit, maxBytes int) boundedInboxResponse {
	t.Helper()
	resp, err := readBoundedInbox(context.Background(), box, boundsRecipient,
		boundedInboxRequest{Active: true, Cursor: cursor, Limit: limit, MaxBytes: maxBytes})
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	return resp
}

func boundsIDs(resp boundedInboxResponse) []string {
	out := make([]string, 0, len(resp.Envelopes))
	for _, env := range resp.Envelopes {
		out = append(out, env.ID)
	}
	return out
}

// Colliding sequence spaces must not lose records. Paging to exhaustion has
// to yield every control record AND every feedback record exactly once.
func TestBoundedInboxKeepsIndependentSequenceSpacesWhole(t *testing.T) {
	box := boundsFixture(t, []int64{1, 2, 3}, []int64{1, 2, 3})

	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		resp := boundsPage(t, box, cursor, 2, 1<<20)
		for _, id := range boundsIDs(resp) {
			seen[id]++
		}
		if resp.NextCursor == "" {
			t.Fatal("a page must always report a resumable cursor")
		}
		if !resp.Truncated && len(resp.Envelopes) == 0 {
			break
		}
		if cursor == resp.NextCursor && !resp.Truncated {
			break
		}
		cursor = resp.NextCursor
	}
	for _, want := range []string{"c-1", "c-2", "c-3", "feedback-1", "feedback-2", "feedback-3"} {
		if seen[want] != 1 {
			t.Fatalf("record %s appeared %d times across pages, want exactly 1 (seen=%v)", want, seen[want], seen)
		}
	}
}

// A limit must actually bind, and the cursor must advance past exactly what
// was returned.
func TestBoundedInboxLimitBindsAndCursorAdvances(t *testing.T) {
	box := boundsFixture(t, []int64{1, 2, 3}, nil)

	first := boundsPage(t, box, "", 1, 1<<20)
	if len(first.Envelopes) != 1 || first.Envelopes[0].ID != "c-1" {
		t.Fatalf("first page = %v, want exactly [c-1]", boundsIDs(first))
	}
	if !first.Truncated {
		t.Fatal("a page stopped by the limit must report truncated")
	}
	second := boundsPage(t, box, first.NextCursor, 1, 1<<20)
	if len(second.Envelopes) != 1 || second.Envelopes[0].ID != "c-2" {
		t.Fatalf("second page = %v, want exactly [c-2]", boundsIDs(second))
	}
}

// An exhausted mailbox yields an empty, non-truncated page that still
// carries a usable cursor.
func TestBoundedInboxEmptyPageIsHonest(t *testing.T) {
	box := boundsFixture(t, []int64{1}, nil)
	first := boundsPage(t, box, "", 10, 1<<20)
	empty := boundsPage(t, box, first.NextCursor, 10, 1<<20)
	if len(empty.Envelopes) != 0 {
		t.Fatalf("exhausted page returned %v", boundsIDs(empty))
	}
	if empty.Truncated {
		t.Fatal("an exhausted page must not claim truncation")
	}
	if empty.NextCursor == "" {
		t.Fatal("an empty page must still return a cursor")
	}
}

// The byte budget must bind independently of the record limit.
func TestBoundedInboxByteBudgetBinds(t *testing.T) {
	box := boundsFixture(t, []int64{1, 2, 3}, nil)
	resp := boundsPage(t, box, "", 100, 200)
	if len(resp.Envelopes) >= 3 {
		t.Fatalf("byte budget did not bind: returned %d records", len(resp.Envelopes))
	}
	if !resp.Truncated {
		t.Fatal("a page stopped by the byte budget must report truncated")
	}
	if resp.RetainedBytes > 200 {
		t.Fatalf("retained %d bytes, over the 200 budget", resp.RetainedBytes)
	}
}

// Cursors are bound to a recipient and a version. Every rejection below would
// otherwise resume from an unverifiable position and silently omit records.
func TestBoundedInboxRejectsUnusableCursors(t *testing.T) {
	box := boundsFixture(t, []int64{1}, nil)
	good := boundsPage(t, box, "", 10, 1<<20).NextCursor

	for name, cursor := range map[string]string{
		"wrong version":    strings.Replace(good, "v1.", "v2.", 1),
		"other recipient":  "v1.someone-else.c0.f0",
		"missing field":    "v1." + boundsRecipient + ".c0",
		"non-numeric mark": "v1." + boundsRecipient + ".cx.f0",
		"negative mark":    "v1." + boundsRecipient + ".c-5.f0",
		"garbage":          "not-a-cursor",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readBoundedInbox(context.Background(), box, boundsRecipient,
				boundedInboxRequest{Active: true, Cursor: cursor, Limit: 10, MaxBytes: 1 << 20})
			if err == nil {
				t.Fatalf("cursor %q was accepted; an unverifiable position must fail", cursor)
			}
		})
	}
	// Positive control: the cursor this build issued is still accepted.
	if _, err := readBoundedInbox(context.Background(), box, boundsRecipient,
		boundedInboxRequest{Active: true, Cursor: good, Limit: 10, MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("a cursor this build issued must be accepted: %v", err)
	}
}

// A malformed control row AFTER the page boundary must not be lost: the page
// that reaches it quarantines it, and a quarantine that cannot be recorded
// fails the read rather than certifying a clean page.
func TestBoundedInboxQuarantinesLateMalformedRow(t *testing.T) {
	box := boundsFixture(t, []int64{1, 2}, nil)
	f, err := os.OpenFile(box.MailFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	before := box.QuarantineCount()
	if _, err := readBoundedInbox(context.Background(), box, boundsRecipient,
		boundedInboxRequest{Active: true, Cursor: "", Limit: 10, MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("a quarantinable row must not fail an otherwise readable page: %v", err)
	}
	if box.QuarantineCount() <= before {
		t.Fatal("the malformed row was skipped instead of quarantined")
	}
}

// A cancelled context must be reported, not traded for a page that looks
// complete.
func TestBoundedInboxPropagatesCancellation(t *testing.T) {
	box := boundsFixture(t, []int64{1, 2, 3}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readBoundedInbox(ctx, box, boundsRecipient,
		boundedInboxRequest{Active: true, Cursor: "", Limit: 10, MaxBytes: 1 << 20}); err == nil {
		t.Fatal("a cancelled read must fail rather than return a page")
	}
}

// An unreadable feedback record must fail the page. Reporting it only on
// stderr would let a short page certify itself complete.
func TestBoundedInboxFailsOnUnreadableFeedbackRecord(t *testing.T) {
	box := boundsFixture(t, []int64{1}, []int64{1})
	path := filepath.Join(os.Getenv(feedback.EnvMailDir), boundsRecipient+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{broken feedback\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := readBoundedInbox(context.Background(), box, boundsRecipient,
		boundedInboxRequest{Active: true, Cursor: "", Limit: 10, MaxBytes: 1 << 20}); err == nil {
		t.Fatal("an unparseable feedback record must fail the page, not be silently dropped")
	}
}

// A missing feedback store is normal and must not fail or truncate.
func TestBoundedInboxToleratesMissingFeedbackStore(t *testing.T) {
	box := boundsFixture(t, []int64{1}, nil)
	resp := boundsPage(t, box, "", 10, 1<<20)
	if len(resp.Envelopes) != 1 || resp.Truncated {
		t.Fatalf("missing feedback store disturbed the page: %v truncated=%v", boundsIDs(resp), resp.Truncated)
	}
}

// Records appended between pages appear on a later page exactly once.
func TestBoundedInboxAppendBetweenPagesIsNotDuplicated(t *testing.T) {
	box := boundsFixture(t, []int64{1, 2}, nil)
	first := boundsPage(t, box, "", 10, 1<<20)
	if len(first.Envelopes) != 2 {
		t.Fatalf("first page = %v, want both records", boundsIDs(first))
	}
	f, err := os.OpenFile(box.MailFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, `{"id":"c-3","seq":3,"sender":"root","recipient":%q,"subject":"late","body":"late","read":false,"timestamp":"2026-09-12T00:00:00Z"}`+"\n", boundsRecipient); err != nil {
		t.Fatal(err)
	}
	f.Close()

	second := boundsPage(t, box, first.NextCursor, 10, 1<<20)
	if len(second.Envelopes) != 1 || second.Envelopes[0].ID != "c-3" {
		t.Fatalf("second page = %v, want exactly the appended [c-3]", boundsIDs(second))
	}
}
