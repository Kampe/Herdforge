package main

import (
	"bufio"
	"context"
	"encoding/json"
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
// Unflagged callers keep the exact JSON array they always had. A caller that
// passes --after-cursor or --limit opts into a bounded object response, so
// nothing existing changes shape underneath it.
//
// The two stores are paged SEPARATELY because they number records with
// independent counters: the control bus assigns mailbox sequences, while
// feedback conversion sets Sequence from the feedback file's own per-file id.
// A single high-water mark over both would skip whichever space ran ahead.
// The cursor therefore carries one mark per source.
//
// Ordering is source-major and ascending within each source: all new control
// records, then all new feedback records. Timestamps are deliberately NOT the
// sort key -- the feedback converter parses its timestamp with the error
// discarded, so an unparseable one silently becomes the zero time and would
// reorder the page.
//
// Both stores are append-only with monotonic per-file identifiers: control
// sequences are reserved under the mailbox lock, and the feedback sender
// appends under an advisory lock with id = max(existing)+1. A record appended
// between two pages therefore appears on a later page; a strict > comparison
// means it can never appear twice.

const (
	defaultBoundedLimit    = 100
	defaultBoundedMaxBytes = 1 << 20
)

// boundedInboxRequest is the parsed paging intent. Active is false for the
// legacy unflagged call.
type boundedInboxRequest struct {
	Active   bool
	Cursor   string
	Limit    int
	MaxBytes int
}

// boundedInboxResponse is the paged wire shape. Truncated is explicit rather
// than inferred from a full-looking page.
type boundedInboxResponse struct {
	Envelopes     []*mail.Envelope `json:"envelopes"`
	NextCursor    string           `json:"next_cursor"`
	Truncated     bool             `json:"truncated"`
	RetainedBytes int              `json:"retained_bytes"`
}

// readBoundedInbox produces one page across both stores under one budget.
//
// A missing feedback store is normal and yields nothing. An UNREADABLE one is
// an error: a page that silently omitted feedback would certify itself
// complete while hiding data, which is the failure mode this whole command is
// meant to remove.
func readBoundedInbox(ctx context.Context, box *mail.Mailbox, recipient string, req boundedInboxRequest) (boundedInboxResponse, error) {
	out := boundedInboxResponse{Envelopes: []*mail.Envelope{}}
	cur, err := mail.ParseCursor(req.Cursor, recipient)
	if err != nil {
		return out, err
	}
	page, err := box.ReadBoundedControl(ctx, recipient, cur, mail.BoundedOptions{Limit: req.Limit, MaxBytes: req.MaxBytes})
	if err != nil {
		return out, err
	}
	out.Envelopes = append(out.Envelopes, page.Envelopes...)
	out.RetainedBytes = page.Bytes
	out.Truncated = page.Truncated

	next, err := mail.ParseCursor(page.Next, recipient)
	if err != nil {
		return out, err
	}
	if out.Truncated {
		// The control store already filled the budget. Feedback is untouched
		// this page and its mark is carried forward unchanged.
		out.NextCursor = next.String()
		return out, nil
	}
	feedbackEnvelopes, highest, truncated, ferr := readFeedbackMailboxBounded(
		recipient, next.Feedback, req.Limit-len(out.Envelopes), req.MaxBytes-out.RetainedBytes)
	if ferr != nil {
		return out, ferr
	}
	for _, env := range feedbackEnvelopes {
		out.Envelopes = append(out.Envelopes, env)
	}
	out.Truncated = out.Truncated || truncated
	next.Feedback = highest
	out.NextCursor = next.String()
	return out, nil
}

// readFeedbackMailboxBounded streams the feedback store, returning records
// with id strictly greater than after, the highest id retained, and whether
// the budget stopped it early.
//
// It reuses the producer's own path resolver so reader and writer cannot
// drift apart, and it mirrors the unbounded converter's schema exactly --
// feedback writes id(int)/from/to/summary/message/read_at, which will not
// unmarshal into mail.Envelope directly.
func readFeedbackMailboxBounded(recipient string, after int64, limit, maxBytes int) ([]*mail.Envelope, int64, bool, error) {
	highest := after
	if strings.TrimSpace(recipient) == "" || limit <= 0 || maxBytes <= 0 {
		return nil, highest, limit <= 0 || maxBytes <= 0, nil
	}
	path := filepath.Join(feedback.FleetMailDir(firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")), recipient+".jsonl")
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Normal: a lane never polled for feedback has no store.
			return nil, highest, false, nil
		}
		return nil, highest, false, fmt.Errorf("mail: open feedback mailbox: %w", err)
	}
	defer file.Close()

	var out []*mail.Envelope
	used := 0
	truncated := false
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), mail.MaxBoundedRecordBytes)
	for scanner.Scan() {
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
			// An unparseable feedback request is still a request that was
			// made. The unbounded path reports and continues; a BOUNDED page
			// must fail, because "reported on stderr" is not visible to a
			// caller consuming the page and would certify a short page as
			// complete.
			return nil, highest, false, fmt.Errorf("mail: unparseable feedback record in feedback store: %w", err)
		}
		if fb.ID <= after {
			continue
		}
		if len(out) >= limit || used+len(line) > maxBytes {
			truncated = true
			break
		}
		ts, _ := time.Parse(time.RFC3339, fb.When)
		out = append(out, &mail.Envelope{
			ID:        fmt.Sprintf("feedback-%d", fb.ID),
			Sequence:  fb.ID,
			Sender:    fb.From,
			Recipient: fb.To,
			Subject:   fb.Summary,
			Body:      fb.Message,
			Read:      fb.ReadAt != nil && strings.TrimSpace(*fb.ReadAt) != "",
			Timestamp: ts,
		})
		used += len(line)
		if fb.ID > highest {
			highest = fb.ID
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, highest, false, fmt.Errorf("mail: scan feedback mailbox: %w", err)
	}
	return out, highest, truncated, nil
}
