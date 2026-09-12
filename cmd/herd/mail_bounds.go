package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/feedback"
	"github.com/Kampe/Herdforge/pkg/mail"
)

// Bounded paging for `herd mail inbox|read`.
//
// Unflagged callers keep the exact JSON array they always had. Passing any
// paging flag opts into a bounded object response.
//
// The two stores are paged SEPARATELY because they number records with
// independent counters: the control bus assigns mailbox sequences, while
// feedback conversion sets Sequence from the feedback file's own per-file id.
// One shared high-water mark would skip whichever space ran ahead.
//
// Ordering is source-major and ascending within each source. Timestamps are
// deliberately NOT the sort key: the feedback converter parses its timestamp
// with the error discarded, so an unparseable one silently becomes the zero
// time and would reorder the page.

const (
	defaultBoundedLimit    = 100
	defaultBoundedMaxBytes = 1 << 20
	// defaultBoundedTimeout gives every bounded read a FINITE sweep. The
	// production caller previously passed context.Background(), so the
	// cancellation path existed in the code and could never fire.
	defaultBoundedTimeout = 30 * time.Second
	maxBoundedTimeout     = 10 * time.Minute
)

// boundedInboxRequest is the parsed paging intent. Active is decided by FLAG
// PRESENCE, not by value: --limit -1 used to be indistinguishable from an
// absent flag and silently fell back to the unbounded legacy read, which is
// the exact behaviour paging exists to prevent.
type boundedInboxRequest struct {
	Active   bool
	Cursor   string
	Limit    int
	MaxBytes int
	Timeout  time.Duration
}

// boundedInboxResponse is the paged wire shape.
type boundedInboxResponse struct {
	Envelopes     []*mail.Envelope `json:"envelopes"`
	NextCursor    string           `json:"next_cursor"`
	Truncated     bool             `json:"truncated"`
	RetainedBytes int              `json:"retained_bytes"`
}

// validate rejects invalid explicit values instead of degrading to unbounded.
func (r *boundedInboxRequest) validate() error {
	if r.Limit < 0 {
		return fmt.Errorf("--limit must be positive, got %d", r.Limit)
	}
	if r.MaxBytes < 0 {
		return fmt.Errorf("--max-bytes must be positive, got %d", r.MaxBytes)
	}
	if r.Timeout < 0 {
		return fmt.Errorf("--timeout must be positive, got %s", r.Timeout)
	}
	if r.Limit == 0 {
		r.Limit = defaultBoundedLimit
	}
	if r.MaxBytes == 0 {
		r.MaxBytes = defaultBoundedMaxBytes
	}
	if r.Timeout == 0 {
		r.Timeout = defaultBoundedTimeout
	}
	if r.Limit > mail.MaxBoundedLimit {
		return fmt.Errorf("--limit %d exceeds the %d maximum", r.Limit, mail.MaxBoundedLimit)
	}
	if r.MaxBytes > mail.MaxBoundedPageBytes {
		return fmt.Errorf("--max-bytes %d exceeds the %d maximum", r.MaxBytes, mail.MaxBoundedPageBytes)
	}
	if r.Timeout > maxBoundedTimeout {
		return fmt.Errorf("--timeout %s exceeds the %s maximum", r.Timeout, maxBoundedTimeout)
	}
	return nil
}

func feedbackMailDir() string {
	return feedback.FleetMailDir(firstEnv("HERD_ROOT", "HERD_REPO_ROOT", "."))
}

// readBoundedInbox produces one page across both stores under one budget.
//
// Both stores are validated to completion even after the page fills: an
// integrity error past the boundary is still an integrity error, and a page
// that hid one would certify itself complete.
func readBoundedInbox(ctx context.Context, box *mail.Mailbox, recipient string, req boundedInboxRequest) (boundedInboxResponse, error) {
	out := boundedInboxResponse{Envelopes: []*mail.Envelope{}}
	dir := feedbackMailDir()
	source := mail.SourceFingerprint(box.MailFile, dir)
	cur, err := mail.ParseCursor(req.Cursor, recipient, source)
	if err != nil {
		return out, err
	}
	page, controlErr := box.ReadBoundedControl(ctx, recipient, cur, mail.BoundedOptions{Limit: req.Limit, MaxBytes: req.MaxBytes})

	// The feedback store is ALWAYS validated, even when the control read
	// failed or already filled the page. Returning early on control.Truncated
	// is what hid an unreadable feedback store behind a full page.
	remainingLimit := req.Limit - len(page.Envelopes)
	remainingBytes := req.MaxBytes - page.Bytes
	if page.Truncated || controlErr != nil {
		remainingLimit, remainingBytes = 0, 0
	}
	fb, highest, fbTruncated, fbErr := readFeedbackMailboxBounded(ctx, dir, recipient, cur.Feedback, remainingLimit, remainingBytes, req.MaxBytes)

	if controlErr != nil {
		return out, errors.Join(controlErr, fbErr)
	}
	if fbErr != nil {
		return out, fbErr
	}

	out.Envelopes = append(out.Envelopes, page.Envelopes...)
	out.Envelopes = append(out.Envelopes, fb...)
	out.RetainedBytes = page.Bytes
	for _, env := range fb {
		size, sizeErr := mail.EnvelopeBytes(env)
		if sizeErr != nil {
			return out, sizeErr
		}
		out.RetainedBytes += size
	}
	out.Truncated = page.Truncated || fbTruncated

	next, err := mail.ParseCursor(page.Next, recipient, source)
	if err != nil {
		return out, err
	}
	next.Feedback = highest
	out.NextCursor = next.String()
	return out, nil
}

// readFeedbackMailboxBounded streams the feedback store, returning records
// with id strictly greater than after, the highest id RETAINED, and whether
// the budget stopped it early.
//
// budget is the caller's whole-page byte budget, used only to reject a single
// record that could never fit; remainingBytes is what is actually left.
//
// It reuses the producer's own path resolver so reader and writer cannot
// drift, and mirrors the unbounded converter's schema exactly: feedback
// writes id(int)/from/to/summary/message/read_at, which will not unmarshal
// into mail.Envelope directly.
func readFeedbackMailboxBounded(ctx context.Context, dir, recipient string, after int64, remainingLimit, remainingBytes, budget int) ([]*mail.Envelope, int64, bool, error) {
	highest := after
	if strings.TrimSpace(recipient) == "" {
		return nil, highest, false, nil
	}
	path := filepath.Join(dir, recipient+".jsonl")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Normal: a lane never polled for feedback has no store. This is
			// checked BEFORE any budget decision, because reporting
			// truncated=true here claimed more data existed when the store
			// did not exist at all.
			return nil, highest, false, nil
		}
		return nil, highest, false, fmt.Errorf("mail: stat feedback mailbox: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, highest, false, errors.New("mail: feedback mailbox is not a regular file; a bounded read cannot bound a stream")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, highest, false, fmt.Errorf("mail: open feedback mailbox: %w", err)
	}
	defer file.Close()

	var out []*mail.Envelope
	used := 0
	truncated := false
	var maxSeen int64
	sawAny := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), mail.MaxBoundedRecordBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, highest, false, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var fb struct {
			ID      int64   `json:"id"`
			From    string  `json:"from"`
			To      string  `json:"to"`
			When    string  `json:"timestamp"`
			Summary string  `json:"summary"`
			Message string  `json:"message"`
			ReadAt  *string `json:"read_at"`
		}
		if err := json.Unmarshal([]byte(line), &fb); err != nil {
			// The unbounded path reports this on stderr and continues, which
			// a paged caller cannot see; a bounded page that dropped it would
			// certify itself complete.
			return nil, highest, false, fmt.Errorf("mail: unparseable feedback record: %w", err)
		}
		if sawAny && fb.ID < maxSeen {
			return nil, highest, false, fmt.Errorf("%w: feedback id %d follows %d", mail.ErrStorageUnordered, fb.ID, maxSeen)
		}
		if fb.ID > maxSeen || !sawAny {
			maxSeen = fb.ID
		}
		sawAny = true
		if fb.ID <= after {
			continue
		}
		if truncated {
			// Page full: keep validating, retain nothing, do not advance.
			continue
		}
		ts, _ := time.Parse(time.RFC3339, fb.When)
		env := &mail.Envelope{
			ID:        fmt.Sprintf("feedback-%d", fb.ID),
			Sequence:  fb.ID,
			Sender:    fb.From,
			Recipient: fb.To,
			Subject:   fb.Summary,
			Body:      fb.Message,
			Read:      fb.ReadAt != nil && strings.TrimSpace(*fb.ReadAt) != "",
			Timestamp: ts,
		}
		size, sizeErr := mail.EnvelopeBytes(env)
		if sizeErr != nil {
			return nil, highest, false, sizeErr
		}
		if size > budget {
			return nil, highest, false, fmt.Errorf("%w: feedback id %d needs %d bytes, budget is %d",
				mail.ErrRecordExceedsBudget, fb.ID, size, budget)
		}
		if len(out) >= remainingLimit || used+size > remainingBytes {
			truncated = true
			continue
		}
		out = append(out, env)
		used += size
		if fb.ID > highest {
			highest = fb.ID
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, highest, false, fmt.Errorf("mail: scan feedback mailbox: %w", err)
	}
	if sawAny && after > maxSeen {
		return nil, highest, false, fmt.Errorf("%w: feedback cursor at %d, highest stored id is %d", mail.ErrStorageRewound, after, maxSeen)
	}
	return out, highest, truncated, nil
}
