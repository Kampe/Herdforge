package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/feedback"
	"github.com/Kampe/Herdforge/pkg/mail"
)

// Bounded inbox paging.
//
// Every fixture gives the two stores deliberately COLLIDING sequence values --
// control 1..3 and feedback 1..3 for the same recipient -- because they are
// independent counters and a single shared high-water mark passes the easy
// cases and loses data on exactly this shape.

const boundsRecipient = "lane-under-test"

type boundsFixture struct {
	box         *mail.Mailbox
	root        string
	feedbackDir string
}

func newBoundsFixture(t *testing.T, controlSeqs, feedbackIDs []int64) boundsFixture {
	t.Helper()
	root := t.TempDir()
	mailFile := filepath.Join(root, "control-mail.jsonl")
	var lines []string
	for _, seq := range controlSeqs {
		lines = append(lines, controlLine(seq, boundsRecipient, fmt.Sprintf("control body %d", seq)))
	}
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(mailFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	feedbackDir := filepath.Join(root, "feedback-mail")
	if err := os.MkdirAll(feedbackDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if len(feedbackIDs) > 0 {
		var fb []string
		for _, id := range feedbackIDs {
			fb = append(fb, feedbackLine(id, boundsRecipient))
		}
		if err := os.WriteFile(filepath.Join(feedbackDir, boundsRecipient+".jsonl"), []byte(strings.Join(fb, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(feedback.EnvMailDir, feedbackDir)
	return boundsFixture{box: mail.NewMailbox(mailFile), root: root, feedbackDir: feedbackDir}
}

func controlLine(seq int64, recipient, body string) string {
	return fmt.Sprintf(
		`{"id":"c-%d","seq":%d,"sender":"root","recipient":%q,"subject":"control %d","body":%q,"read":false,"timestamp":"2026-09-12T00:00:00Z"}`,
		seq, seq, recipient, seq, body)
}

func feedbackLine(id int64, recipient string) string {
	return fmt.Sprintf(
		`{"id":%d,"type":"message","from":"coordinator","to":%q,"timestamp":"2026-09-12T00:00:00Z","summary":"feedback %d","message":"feedback body %d"}`,
		id, recipient, id, id)
}

func appendLineTo(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func boundsRequest(cursor string, limit, maxBytes int) boundedInboxRequest {
	return boundedInboxRequest{Active: true, Cursor: cursor, Limit: limit, MaxBytes: maxBytes, Timeout: 30 * time.Second}
}

func boundsRead(t *testing.T, f boundsFixture, cursor string, limit, maxBytes int) boundedInboxResponse {
	t.Helper()
	resp, err := readBoundedInbox(context.Background(), f.box, boundsRecipient, boundsRequest(cursor, limit, maxBytes))
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	return resp
}

func boundsReadErr(f boundsFixture, cursor string, limit, maxBytes int) error {
	_, err := readBoundedInbox(context.Background(), f.box, boundsRecipient, boundsRequest(cursor, limit, maxBytes))
	return err
}

func boundsIDs(resp boundedInboxResponse) []string {
	out := make([]string, 0, len(resp.Envelopes))
	for _, env := range resp.Envelopes {
		out = append(out, env.ID)
	}
	return out
}

// Colliding sequence spaces must not lose records across pages.
func TestBoundedInboxKeepsIndependentSequenceSpacesWhole(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2, 3}, []int64{1, 2, 3})
	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 12; page++ {
		resp := boundsRead(t, f, cursor, 2, 1<<20)
		for _, id := range boundsIDs(resp) {
			seen[id]++
		}
		if resp.NextCursor == "" {
			t.Fatal("every page must report a resumable cursor")
		}
		if len(resp.Envelopes) == 0 && !resp.Truncated {
			break
		}
		cursor = resp.NextCursor
	}
	for _, want := range []string{"c-1", "c-2", "c-3", "feedback-1", "feedback-2", "feedback-3"} {
		if seen[want] != 1 {
			t.Fatalf("record %s appeared %d times, want exactly 1 (seen=%v)", want, seen[want], seen)
		}
	}
}

// A malformed row AFTER the page has filled must still be quarantined. The
// page is limit 1 over a valid record FOLLOWED by the malformed one, so the
// scan genuinely crosses the boundary; an earlier version used limit 10 over
// two records and never filled the page at all.
func TestBoundedInboxQuarantinesMalformedRowPastAFullPage(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	appendLineTo(t, f.box.MailFile, controlLine(2, boundsRecipient, "second"))
	appendLineTo(t, f.box.MailFile, "{not json")

	before := f.box.QuarantineCount()
	resp := boundsRead(t, f, "", 1, 1<<20)
	if len(resp.Envelopes) != 1 || !resp.Truncated {
		t.Fatalf("page = %v truncated=%v, want exactly one record and truncated", boundsIDs(resp), resp.Truncated)
	}
	if f.box.QuarantineCount() <= before {
		t.Fatal("the malformed row past the page boundary was never quarantined")
	}
}

// A quarantine sink that cannot be written must FAIL the page rather than let
// it read as clean.
func TestBoundedInboxFailsWhenQuarantineSinkIsUnwritable(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	appendLineTo(t, f.box.MailFile, "{not json")
	// A directory at the quarantine path makes the append fail.
	if err := os.MkdirAll(f.box.MailFile+".quarantine.jsonl", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := boundsReadErr(f, "", 10, 1<<20); err == nil {
		t.Fatal("a failed quarantine write must fail the page, not be swallowed")
	}
}

// An unreadable feedback record must be reported even when the control store
// already filled the page. Returning early on control truncation hid it.
func TestBoundedInboxReportsFeedbackErrorBehindAFullControlPage(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2, 3}, []int64{1})
	appendLineTo(t, filepath.Join(f.feedbackDir, boundsRecipient+".jsonl"), "{broken feedback")

	err := boundsReadErr(f, "", 1, 1<<20)
	if err == nil {
		t.Fatal("a full control page hid an unreadable feedback store")
	}
	if !strings.Contains(err.Error(), "feedback") {
		t.Fatalf("error must name the feedback store, got: %v", err)
	}
}

// Cursor encoding must be injective: "a.b" and "a_b" are different lanes.
func TestBoundedInboxCursorEncodingIsInjective(t *testing.T) {
	source := mail.SourceFingerprint("/x", "/y")
	dotted := mail.Cursor{Recipient: "a.b", Source: source, Control: 1}.String()
	if _, err := mail.ParseCursor(dotted, "a_b", source); err == nil {
		t.Fatal("a cursor for a.b was accepted for a_b; the encoding is not injective")
	}
	if _, err := mail.ParseCursor(dotted, "a.b", source); err != nil {
		t.Fatalf("a cursor must still be accepted by its own recipient: %v", err)
	}
}

// A cursor is bound to the STORES it was issued against, not just the name.
func TestBoundedInboxCursorIsBoundToStorage(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	issued := boundsRead(t, f, "", 10, 1<<20).NextCursor

	other := newBoundsFixture(t, []int64{1}, nil)
	if _, err := readBoundedInbox(context.Background(), other.box, boundsRecipient, boundsRequest(issued, 10, 1<<20)); err == nil {
		t.Fatal("a cursor from a different mailbox/feedback root was accepted")
	}
}

// Every unusable cursor shape fails rather than resuming from an
// unverifiable position.
func TestBoundedInboxRejectsUnusableCursors(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	good := boundsRead(t, f, "", 10, 1<<20).NextCursor
	parts := strings.Split(good, ".")

	for name, cursor := range map[string]string{
		"wrong version":    "v2." + strings.Join(parts[1:], "."),
		"other recipient":  "v1." + fmt.Sprintf("%x", "someone-else") + "." + parts[2] + ".c0.f0",
		"missing field":    strings.Join(parts[:4], "."),
		"non-numeric mark": strings.Join(parts[:3], ".") + ".cx.f0",
		"negative mark":    strings.Join(parts[:3], ".") + ".c-5.f0",
		"garbage":          "not-a-cursor",
	} {
		t.Run(name, func(t *testing.T) {
			if err := boundsReadErr(f, cursor, 10, 1<<20); err == nil {
				t.Fatalf("cursor %q was accepted", cursor)
			}
		})
	}
	if err := boundsReadErr(f, good, 10, 1<<20); err != nil {
		t.Fatalf("a cursor this build issued must be accepted: %v", err)
	}
}

// A cursor ahead of the store means the store was replaced, truncated or
// renumbered. The control mailbox is NOT purely append-only: the
// receipt-backed repair path rewrites it in place.
func TestBoundedInboxRejectsRewoundStorage(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2, 3}, nil)
	ahead := mail.Cursor{
		Recipient: boundsRecipient,
		Source:    mail.SourceFingerprint(f.box.MailFile, f.feedbackDir),
		Control:   99,
	}.String()
	if err := boundsReadErr(f, ahead, 10, 1<<20); err == nil {
		t.Fatal("a cursor ahead of the store was accepted; lower records would be skipped silently")
	}
}

// A store whose sequences do not ascend in file order cannot be paged by a
// high-water mark without skipping the lower record that follows.
func TestBoundedInboxRejectsUnorderedStorage(t *testing.T) {
	f := newBoundsFixture(t, []int64{5}, nil)
	appendLineTo(t, f.box.MailFile, controlLine(2, boundsRecipient, "out of order"))
	if err := boundsReadErr(f, "", 10, 1<<20); err == nil {
		t.Fatal("an unordered store was paged anyway")
	}
}

// A record larger than the whole budget must be an actionable error, not an
// endless run of empty truncated pages with an unmoving cursor.
func TestBoundedInboxOversizedRecordFailsInsteadOfStalling(t *testing.T) {
	f := newBoundsFixture(t, nil, nil)
	appendLineTo(t, f.box.MailFile, controlLine(1, boundsRecipient, strings.Repeat("x", 4096)))
	err := boundsReadErr(f, "", 10, 512)
	if err == nil {
		t.Fatal("an oversized record produced a page instead of an error")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("error must name the budget, got: %v", err)
	}
}

// Controls that exactly fill the limit with NO feedback store must not claim
// truncation: that falsely advertised more data.
func TestBoundedInboxExactExhaustionWithNoFeedbackIsNotTruncated(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2}, nil)
	resp := boundsRead(t, f, "", 2, 1<<20)
	if len(resp.Envelopes) != 2 {
		t.Fatalf("page = %v, want both records", boundsIDs(resp))
	}
	if resp.Truncated {
		t.Fatal("an exactly-exhausted page with no feedback store claimed more data existed")
	}
}

// Retained bytes must include BOTH sources and stay within the budget.
func TestBoundedInboxBytesAccountForBothSources(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, []int64{1})
	resp := boundsRead(t, f, "", 10, 1<<20)
	if len(resp.Envelopes) != 2 {
		t.Fatalf("page = %v, want one record from each store", boundsIDs(resp))
	}
	total := 0
	for _, env := range resp.Envelopes {
		size, err := mail.EnvelopeBytes(env)
		if err != nil {
			t.Fatal(err)
		}
		total += size
	}
	if resp.RetainedBytes != total {
		t.Fatalf("retained_bytes = %d, want %d (feedback bytes were not counted)", resp.RetainedBytes, total)
	}
}

// The byte budget binds on MARSHALLED size and is never exceeded.
func TestBoundedInboxByteBudgetBindsOnSerializedSize(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2, 3}, nil)
	one, err := mail.EnvelopeBytes(boundsRead(t, f, "", 1, 1<<20).Envelopes[0])
	if err != nil {
		t.Fatal(err)
	}
	resp := boundsRead(t, f, "", 100, one+1)
	if len(resp.Envelopes) != 1 || !resp.Truncated {
		t.Fatalf("near-boundary page = %v truncated=%v, want exactly one and truncated", boundsIDs(resp), resp.Truncated)
	}
	if resp.RetainedBytes > one+1 {
		t.Fatalf("retained %d bytes, over the %d budget", resp.RetainedBytes, one+1)
	}
}

// Cancellation must be reported, never traded for a page.
func TestBoundedInboxPropagatesCancellation(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2, 3}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readBoundedInbox(ctx, f.box, boundsRecipient, boundsRequest("", 10, 1<<20)); err == nil {
		t.Fatal("a cancelled read returned a page")
	}
}

// Invalid explicit bounds must be REFUSED, never degraded to unbounded.
func TestBoundedRequestRejectsInvalidBounds(t *testing.T) {
	for name, req := range map[string]boundedInboxRequest{
		"negative limit":   {Limit: -1},
		"negative bytes":   {MaxBytes: -1},
		"negative timeout": {Timeout: -time.Second},
		"limit too large":  {Limit: mail.MaxBoundedLimit + 1},
		"bytes too large":  {MaxBytes: mail.MaxBoundedPageBytes + 1},
		"timeout too long": {Timeout: maxBoundedTimeout + time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			r := req
			if err := r.validate(); err == nil {
				t.Fatalf("%+v was accepted; an invalid bound must never fall back to unbounded", req)
			}
		})
	}
	empty := boundedInboxRequest{}
	if err := empty.validate(); err != nil {
		t.Fatalf("an all-default request must be accepted: %v", err)
	}
	if empty.Limit != defaultBoundedLimit || empty.MaxBytes != defaultBoundedMaxBytes || empty.Timeout != defaultBoundedTimeout {
		t.Fatalf("defaults were not applied: %+v", empty)
	}
}

// Missing feedback store is normal.
func TestBoundedInboxToleratesMissingFeedbackStore(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	resp := boundsRead(t, f, "", 10, 1<<20)
	if len(resp.Envelopes) != 1 || resp.Truncated {
		t.Fatalf("missing feedback store disturbed the page: %v truncated=%v", boundsIDs(resp), resp.Truncated)
	}
}

// Appends between pages arrive on a later page exactly once.
func TestBoundedInboxAppendBetweenPagesIsNotDuplicated(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2}, nil)
	first := boundsRead(t, f, "", 10, 1<<20)
	appendLineTo(t, f.box.MailFile, controlLine(3, boundsRecipient, "late"))
	second := boundsRead(t, f, first.NextCursor, 10, 1<<20)
	if len(second.Envelopes) != 1 || second.Envelopes[0].ID != "c-3" {
		t.Fatalf("second page = %v, want exactly the appended [c-3]", boundsIDs(second))
	}
}

// ---- compiled CLI fixtures ----
//
// These run the REAL binary with every storage and environment override
// pinned to a temp tree, so no live fleet path is read and no live mailbox is
// touched. Nothing above proves the flags are wired, the exit codes are
// right, or the JSON shape is what a caller receives.

func boundsCLI(t *testing.T, f boundsFixture, args ...string) (string, string, int) {
	t.Helper()
	binary := buildHerd(t)
	cmd := exec.Command(binary, append([]string{"mail", "inbox", "--recipient", boundsRecipient, "--mail", f.box.MailFile}, args...)...)
	cmd.Dir = f.root
	cmd.Env = append(os.Environ(),
		"HERD_ROOT="+f.root,
		"HERD_REPO_ROOT="+f.root,
		feedback.EnvMailDir+"="+f.feedbackDir,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run herd: %v", err)
		}
	}
	return stdout.String(), stderr.String(), code
}

// An unflagged call must still return the legacy ARRAY.
func TestBoundedInboxCLILegacyShapeUnchanged(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2}, nil)
	stdout, stderr, code := boundsCLI(t, f)
	if code != 0 {
		t.Fatalf("legacy call exited %d: %s", code, stderr)
	}
	var envelopes []*mail.Envelope
	if err := json.Unmarshal([]byte(stdout), &envelopes); err != nil {
		t.Fatalf("legacy output is not a JSON array: %v\n%s", err, stdout)
	}
	if len(envelopes) != 2 {
		t.Fatalf("legacy output had %d records, want 2", len(envelopes))
	}
}

// A paged call returns the bounded OBJECT, with both stores represented.
func TestBoundedInboxCLIPagedShapeAndScope(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2}, []int64{1})
	stdout, stderr, code := boundsCLI(t, f, "--limit", "10")
	if code != 0 {
		t.Fatalf("paged call exited %d: %s", code, stderr)
	}
	var resp boundedInboxResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		t.Fatalf("paged output is not the bounded object: %v\n%s", err, stdout)
	}
	if len(resp.Envelopes) != 3 {
		t.Fatalf("paged output had %d records, want 2 control + 1 feedback", len(resp.Envelopes))
	}
	if resp.NextCursor == "" || resp.RetainedBytes <= 0 {
		t.Fatalf("paged output is missing continuation or byte accounting: %+v", resp)
	}
}

// Explicitly invalid bounds must exit non-zero, NOT silently return the
// unbounded legacy array.
func TestBoundedInboxCLIRejectsInvalidBounds(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2}, nil)
	for name, args := range map[string][]string{
		"negative limit": {"--limit", "-1"},
		"negative bytes": {"--max-bytes", "-1"},
		"huge limit":     {"--limit", "99999999"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, _, code := boundsCLI(t, f, args...)
			if code == 0 {
				t.Fatalf("%v exited 0 and returned %q; an invalid bound must be refused", args, stdout)
			}
			if strings.HasPrefix(strings.TrimSpace(stdout), "[") {
				t.Fatalf("%v fell back to the unbounded legacy array", args)
			}
		})
	}
}

// An empty --after-cursor is still PAGING, not a fallthrough to unbounded.
func TestBoundedInboxCLIEmptyCursorStaysBounded(t *testing.T) {
	f := newBoundsFixture(t, []int64{1, 2}, nil)
	stdout, stderr, code := boundsCLI(t, f, "--after-cursor", "")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, stderr)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "[") {
		t.Fatal("an explicit empty cursor fell through to the unbounded legacy array")
	}
}

// A corrupt cursor exits non-zero through the real CLI.
func TestBoundedInboxCLIRejectsCorruptCursor(t *testing.T) {
	f := newBoundsFixture(t, []int64{1}, nil)
	_, stderr, code := boundsCLI(t, f, "--after-cursor", "not-a-cursor")
	if code == 0 {
		t.Fatal("a corrupt cursor exited 0")
	}
	if !strings.Contains(stderr, "cursor") {
		t.Fatalf("stderr must name the cursor problem, got: %s", stderr)
	}
}
