package mail

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// returned over 8 MB, every time. This path bounds what is RETAINED and
// RETURNED. It does not bound the SCAN: JSONL carries no index, so a page
// still walks the file from the start, and it walks it to the END even after
// the page is full, because an integrity error past the boundary is still an
// integrity error. Page N costs O(file) to scan and O(limit) to hold.

const (
	// MaxBoundedRecordBytes caps one encoded record so a single pathological
	// line cannot defeat the budget it is counted against.
	MaxBoundedRecordBytes = 1 << 20
	// MaxBoundedLimit and MaxBoundedPageBytes cap operator input. A caller
	// asking for a billion records is asking for the unbounded behaviour this
	// command exists to replace.
	MaxBoundedLimit     = 10000
	MaxBoundedPageBytes = 32 << 20
	// maxReportedQuarantineErrors bounds what a failing page carries back.
	// Accumulating one error per malformed row would make a corrupt file
	// allocate without limit, which is the bug this file is about.
	maxReportedQuarantineErrors = 4
)

// ErrRecordExceedsBudget reports a record that cannot fit any page at the
// requested budget. Returning it is what stops a caller looping forever on
// empty, truncated pages whose cursor never advances.
var ErrRecordExceedsBudget = errors.New("mail: record exceeds the requested page byte budget")

// ErrStorageRewound reports a cursor ahead of the store. The control mailbox
// is NOT purely append-only -- the receipt-backed repair path rewrites it in
// place -- so a cursor can outlive the numbering it was issued against.
// Resuming anyway would silently skip everything below the mark.
var ErrStorageRewound = errors.New("mail: cursor is ahead of the store; it was replaced, truncated or renumbered")

// ErrStorageUnordered reports records whose sequences do not ascend in file
// order. A high-water mark is only sound over an ascending store; on any
// other shape it skips the lower records that follow a higher one.
var ErrStorageUnordered = errors.New("mail: store sequences do not ascend in file order; a high-water mark would skip records")

// Cursor is a versioned position carrying one high-water mark per source,
// bound to both the recipient and the STORES it was issued against.
//
// Two marks, not one, because the control bus and the feedback store number
// records with SEPARATE counters -- feedback conversion sets Sequence from
// its own per-file id -- so a single shared mark skips records in whichever
// space ran ahead.
//
// The source fingerprint is a hash, never a path: cursors travel through logs
// and artifacts, and a host path does not belong in either.
type Cursor struct {
	Recipient string
	Source    string
	Control   int64
	Feedback  int64
}

const cursorVersion = "v1"

// SourceFingerprint identifies the exact stores a cursor is valid against.
func SourceFingerprint(controlPath, feedbackDir string) string {
	sum := sha256.Sum256([]byte(controlPath + "\x00" + feedbackDir))
	return hex.EncodeToString(sum[:8])
}

// String renders the cursor. The recipient is hex-encoded because it is
// INJECTIVE: an earlier draft mapped '.' to '_', which made "a.b" and "a_b"
// encode identically and accept each other's cursors.
func (c Cursor) String() string {
	return fmt.Sprintf("%s.%s.%s.c%d.f%d",
		cursorVersion, hex.EncodeToString([]byte(c.Recipient)), c.Source, c.Control, c.Feedback)
}

// ParseCursor parses and BINDS a cursor to the recipient and stores being
// read. Any mismatch is a hard error: resuming from an unverifiable position
// silently omits records, which is the failure this command exists to avoid.
func ParseCursor(raw, recipient, source string) (Cursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Cursor{Recipient: recipient, Source: source}, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 5 {
		return Cursor{}, fmt.Errorf("mail cursor is malformed: want %s.<recipient>.<source>.c<n>.f<n>", cursorVersion)
	}
	if parts[0] != cursorVersion {
		return Cursor{}, fmt.Errorf("mail cursor version %q is not supported by this build (want %s)", parts[0], cursorVersion)
	}
	decoded, err := hex.DecodeString(parts[1])
	if err != nil {
		return Cursor{}, fmt.Errorf("mail cursor recipient field is not valid encoding: %w", err)
	}
	if string(decoded) != recipient {
		return Cursor{}, errors.New("mail cursor was issued for a different recipient; refusing to resume another mailbox's position")
	}
	if parts[2] != source {
		return Cursor{}, errors.New("mail cursor was issued against different storage (mailbox path or feedback root changed); refusing to resume")
	}
	control, err := parseCursorMark(parts[3], "c")
	if err != nil {
		return Cursor{}, err
	}
	feedback, err := parseCursorMark(parts[4], "f")
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{Recipient: recipient, Source: source, Control: control, Feedback: feedback}, nil
}

func parseCursorMark(field, prefix string) (int64, error) {
	if !strings.HasPrefix(field, prefix) {
		return 0, fmt.Errorf("mail cursor field is missing its %q marker", prefix)
	}
	value, err := strconv.ParseInt(strings.TrimPrefix(field, prefix), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("mail cursor field is not a number: %w", err)
	}
	if value < 0 {
		return 0, errors.New("mail cursor field is negative")
	}
	return value, nil
}

// BoundedPage is one bounded read. Truncated is explicit rather than inferred
// from len(Envelopes) == limit, which is ambiguous when the remainder ends
// exactly on the boundary.
type BoundedPage struct {
	Envelopes []*Envelope
	Next      string
	Truncated bool
	Bytes     int
}

// BoundedOptions configures one page. Both budgets are required; this package
// does not invent a default.
type BoundedOptions struct {
	Limit    int
	MaxBytes int
}

func (o BoundedOptions) validate() error {
	if o.Limit <= 0 || o.MaxBytes <= 0 {
		return errors.New("mail: bounded read requires a positive limit and byte budget")
	}
	if o.Limit > MaxBoundedLimit {
		return fmt.Errorf("mail: limit %d exceeds the %d maximum", o.Limit, MaxBoundedLimit)
	}
	if o.MaxBytes > MaxBoundedPageBytes {
		return fmt.Errorf("mail: byte budget %d exceeds the %d maximum", o.MaxBytes, MaxBoundedPageBytes)
	}
	return nil
}

// EnvelopeBytes is the accounted size of one record: the MARSHALLED envelope,
// which is what the caller actually receives. The raw input line is not the
// same thing -- escaping and field conversion change it -- and counting the
// input would let a page overshoot its stated budget.
//
// The response wrapper itself (next_cursor, truncated, retained_bytes) is
// deliberately NOT counted: it is a small fixed overhead, and documenting it
// is more honest than folding an unrelated constant into the record budget.
func EnvelopeBytes(env *Envelope) (int, error) {
	data, err := json.Marshal(env)
	if err != nil {
		return 0, fmt.Errorf("mail: measure envelope: %w", err)
	}
	return len(data), nil
}

// ReadBoundedControl streams the control mailbox and returns records for
// recipient whose Sequence is strictly greater than the cursor's control
// mark, in ascending order.
//
// After the page fills it KEEPS SCANNING to the end of the file, retaining
// nothing further, so that a malformed row, a failed quarantine, an unordered
// store or a cancelled context past the boundary is still reported. A page
// that looked complete while an integrity error went unmentioned would be
// worse than no page at all. The cursor never advances over a record that was
// not returned.
//
// Paging never acknowledges, rewrites, or deletes anything.
func (m *Mailbox) ReadBoundedControl(ctx context.Context, recipient string, cur Cursor, opts BoundedOptions) (BoundedPage, error) {
	page := BoundedPage{Envelopes: []*Envelope{}}
	if m == nil {
		return page, errors.New("mail: nil mailbox")
	}
	if ctx == nil {
		return page, errors.New("mail: bounded read requires a context with a deadline")
	}
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return page, errors.New("mail: recipient is required")
	}
	if err := opts.validate(); err != nil {
		return page, err
	}
	// A Cursor handed in directly must be validated exactly as a parsed one
	// is: the CLI is not the only caller, and an unchecked struct literal
	// would bypass every binding below.
	if cur.Recipient != recipient {
		return page, errors.New("mail: cursor recipient does not match the read recipient")
	}
	if cur.Control < 0 || cur.Feedback < 0 {
		return page, errors.New("mail: cursor marks must not be negative")
	}
	high := cur.Control

	info, err := os.Stat(m.MailFile)
	if err != nil {
		if os.IsNotExist(err) {
			if cur.Control > 0 {
				return page, fmt.Errorf("%w: control mailbox is absent but the cursor is at %d", ErrStorageRewound, cur.Control)
			}
			page.Next = Cursor{Recipient: recipient, Source: cur.Source, Control: high, Feedback: cur.Feedback}.String()
			return page, nil
		}
		return page, fmt.Errorf("mail: stat mailbox: %w", err)
	}
	// A FIFO or device would block past any deadline this read was given.
	if !info.Mode().IsRegular() {
		return page, errors.New("mail: control mailbox is not a regular file; a bounded read cannot bound a stream")
	}

	file, err := os.Open(m.MailFile)
	if err != nil {
		return page, fmt.Errorf("mail: open mailbox: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxBoundedRecordBytes)
	var quarantineErrs []error
	quarantineFailures := 0
	var maxSeen int64
	sawAny := false
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
			if qErr := m.quarantineLineContext(ctx, line, decodeErr); qErr != nil {
				quarantineFailures++
				if len(quarantineErrs) < maxReportedQuarantineErrors {
					quarantineErrs = append(quarantineErrs, qErr)
				}
			}
			continue
		}
		// Ordering is validated over the WHOLE file, for every recipient,
		// because the sequence space is global and a high-water mark is only
		// sound while it ascends.
		if sawAny && env.Sequence < maxSeen {
			return page, fmt.Errorf("%w: sequence %d follows %d", ErrStorageUnordered, env.Sequence, maxSeen)
		}
		if env.Sequence > maxSeen || !sawAny {
			maxSeen = env.Sequence
		}
		sawAny = true

		if env.Recipient != recipient && env.Recipient != "all" {
			continue
		}
		if env.Sequence <= cur.Control {
			continue
		}
		if page.Truncated {
			// Page is already full. Keep validating, retain nothing, and do
			// NOT advance the cursor over this record.
			continue
		}
		size, sizeErr := EnvelopeBytes(&env)
		if sizeErr != nil {
			return page, sizeErr
		}
		if size > opts.MaxBytes {
			// Skipping it would loop forever on empty truncated pages whose
			// cursor never moves; consuming it would break the budget.
			return page, fmt.Errorf("%w: sequence %d needs %d bytes, budget is %d",
				ErrRecordExceedsBudget, env.Sequence, size, opts.MaxBytes)
		}
		if len(page.Envelopes) >= opts.Limit || page.Bytes+size > opts.MaxBytes {
			page.Truncated = true
			continue
		}
		copied := env
		page.Envelopes = append(page.Envelopes, &copied)
		page.Bytes += size
		if env.Sequence > high {
			high = env.Sequence
		}
	}
	if err := scanner.Err(); err != nil {
		return page, fmt.Errorf("mail: scan mailbox: %w", err)
	}
	if len(quarantineErrs) > 0 {
		return page, fmt.Errorf("mail: %d quarantine write(s) failed: %w", quarantineFailures, errors.Join(quarantineErrs...))
	}
	if sawAny && cur.Control > maxSeen {
		return page, fmt.Errorf("%w: cursor at %d, highest stored sequence is %d", ErrStorageRewound, cur.Control, maxSeen)
	}
	page.Next = Cursor{Recipient: recipient, Source: cur.Source, Control: high, Feedback: cur.Feedback}.String()
	return page, nil
}
