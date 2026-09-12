package mail

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Bounded incremental reads for the control mailbox.
//
// A full read materialises the whole JSONL file and returns a recipient's
// entire history: the canonical bus is over 11 MB and one root inbox read
// returned over 8 MB, every time, for observations that only wanted what was
// new. This path bounds what is RETAINED and RETURNED. It does not and cannot
// bound the SCAN: JSONL carries no index, so locating records after a cursor
// still walks the file from the start. Page N therefore costs O(file) to scan
// and O(limit) to hold. That is the honest shape of the fix — it removes the
// repeated multi-megabyte allocation and copy, not the read itself.

// MaxBoundedRecordBytes caps one encoded record. A single pathological line
// cannot be allowed to defeat the page budget it is counted against.
const MaxBoundedRecordBytes = 1 << 20

// Cursor is a versioned, recipient-bound position carrying one high-water
// mark per source.
//
// Two marks, not one, because the control bus and the feedback store number
// their records with SEPARATE counters -- feedback conversion sets Sequence
// from its own per-file id -- so a single shared mark would skip records in
// whichever space ran ahead. It is rendered as text so an operator can read it, and
// parsed strictly so a wrong or stale one fails instead of silently
// resuming from the wrong place.
type Cursor struct {
	Recipient string
	Control   int64
	Feedback  int64
}

const cursorVersion = "v1"

// String renders the cursor. Recipient is embedded so a cursor cannot be
// replayed against a different mailbox.
func (c Cursor) String() string {
	return fmt.Sprintf("%s.%s.c%d.f%d", cursorVersion, encodeCursorRecipient(c.Recipient), c.Control, c.Feedback)
}

func encodeCursorRecipient(r string) string {
	// Recipients are lane names; the separator is the only character that
	// could confuse the parse, so it is the only one escaped.
	return strings.ReplaceAll(strings.TrimSpace(r), ".", "_")
}

// ParseCursor parses and BINDS a cursor to the recipient being read. An
// unknown version, a malformed field, a negative mark, or a cursor issued for
// another recipient is a hard error: resuming from an unverifiable position
// would silently omit messages, which is the exact failure this command
// exists to avoid.
func ParseCursor(raw, recipient string) (Cursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Cursor{Recipient: recipient}, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 4 {
		return Cursor{}, fmt.Errorf("mail cursor %q is malformed: want %s.<recipient>.c<n>.f<n>", raw, cursorVersion)
	}
	if parts[0] != cursorVersion {
		return Cursor{}, fmt.Errorf("mail cursor version %q is not supported by this build (want %s)", parts[0], cursorVersion)
	}
	if want := encodeCursorRecipient(recipient); parts[1] != want {
		return Cursor{}, fmt.Errorf("mail cursor was issued for a different recipient; refusing to resume another mailbox's position")
	}
	control, err := parseCursorMark(parts[2], "c")
	if err != nil {
		return Cursor{}, err
	}
	feedback, err := parseCursorMark(parts[3], "f")
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{Recipient: recipient, Control: control, Feedback: feedback}, nil
}

func parseCursorMark(field, prefix string) (int64, error) {
	if !strings.HasPrefix(field, prefix) {
		return 0, fmt.Errorf("mail cursor field %q is missing its %q marker", field, prefix)
	}
	value, err := strconv.ParseInt(strings.TrimPrefix(field, prefix), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("mail cursor field %q is not a number: %w", field, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("mail cursor field %q is negative", field)
	}
	return value, nil
}

// BoundedPage is one bounded read. Truncated is reported explicitly rather
// than inferred from len(Envelopes) == limit, which is ambiguous when the
// remainder happens to end exactly on the boundary.
type BoundedPage struct {
	Envelopes []*Envelope `json:"envelopes"`
	Next      string      `json:"next_cursor"`
	Truncated bool        `json:"truncated"`
	Bytes     int         `json:"retained_bytes"`
}

// BoundedOptions configures one page. Limit and MaxBytes are both required to
// be positive by the caller; this package does not invent a default budget.
type BoundedOptions struct {
	Limit    int
	MaxBytes int
}

// ReadBoundedControl streams the control mailbox and returns records for
// recipient whose Sequence is strictly greater than the cursor's control
// mark, in ascending mailbox order.
//
// Errors are never traded for a full page. A decode failure, a quarantine
// failure, or a cancelled context is returned even when the page already
// filled, because a page that looks complete while an integrity error went
// unreported is worse than no page at all. Paging never acknowledges,
// rewrites, or deletes anything.
func (m *Mailbox) ReadBoundedControl(ctx context.Context, recipient string, cur Cursor, opts BoundedOptions) (BoundedPage, error) {
	page := BoundedPage{Envelopes: []*Envelope{}}
	if m == nil {
		return page, errors.New("mail: nil mailbox")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return page, errors.New("mail: recipient is required")
	}
	if opts.Limit <= 0 || opts.MaxBytes <= 0 {
		return page, errors.New("mail: bounded read requires a positive limit and byte budget")
	}
	high := cur.Control

	file, err := os.Open(m.MailFile)
	if err != nil {
		if os.IsNotExist(err) {
			page.Next = Cursor{Recipient: recipient, Control: high, Feedback: cur.Feedback}.String()
			return page, nil
		}
		return page, fmt.Errorf("mail: open mailbox: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxBoundedRecordBytes)
	var quarantineErrs []error
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var env Envelope
		if decodeErr := json.Unmarshal([]byte(line), &env); decodeErr != nil {
			// Same fail-closed contract as the unbounded read: a malformed
			// row is quarantined, never skipped silently, and a quarantine
			// that cannot be recorded fails the read.
			if qErr := m.quarantineLineContext(ctx, line, decodeErr); qErr != nil {
				quarantineErrs = append(quarantineErrs, qErr)
			}
			continue
		}
		if env.Recipient != recipient && env.Recipient != "all" {
			continue
		}
		if env.Sequence <= cur.Control {
			continue
		}
		if len(page.Envelopes) >= opts.Limit || page.Bytes+len(line) > opts.MaxBytes {
			page.Truncated = true
			break
		}
		copied := env
		page.Envelopes = append(page.Envelopes, &copied)
		page.Bytes += len(line)
		if env.Sequence > high {
			high = env.Sequence
		}
	}
	if err := scanner.Err(); err != nil {
		return page, fmt.Errorf("mail: scan mailbox: %w", err)
	}
	if len(quarantineErrs) > 0 {
		return page, errors.Join(quarantineErrs...)
	}
	page.Next = Cursor{Recipient: recipient, Control: high, Feedback: cur.Feedback}.String()
	return page, nil
}
