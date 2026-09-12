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
	"path/filepath"
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

// Cursor is a versioned position carrying, per source, a high-water mark AND
// an anchor on that store's first record. It is bound to the recipient and to
// the resolved identity of the stores it was issued against.
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
	// ControlAnchor identifies the first record of the control store, and
	// FeedbackAnchor the first record of the feedback store. Path binding
	// alone cannot notice a store REPLACED by a different stream that happens
	// to reach the same or higher numbers; the first record can. Appends
	// never change it, and acknowledgement writes the handled sidecar rather
	// than the mailbox, so an ack-only rewrite keeps a cursor usable. A
	// repair that rewrites the first record does invalidate it, which is the
	// conservative direction.
	ControlAnchor  string
	Feedback       int64
	FeedbackAnchor string
}

const (
	cursorVersion = "v2"
	// EmptyAnchor is the anchor of a store with no records.
	EmptyAnchor = "0"
)

// SourceFingerprint identifies the exact stores a cursor is valid against.
//
// Paths are RESOLVED first: two runs with the same relative --mail value from
// different working directories address different files, and hashing the raw
// strings made them accept each other's cursors. Symlinks are resolved too,
// so one store reached by two names is one identity. A path that cannot be
// resolved is fingerprinted from its absolute form, which still separates it
// from an unrelated store; it is never silently treated as equal.
func SourceFingerprint(controlPath, feedbackDir string) string {
	sum := sha256.Sum256([]byte(canonicalStoragePath(controlPath) + "\x00" + canonicalStoragePath(feedbackDir)))
	return hex.EncodeToString(sum[:8])
}

func canonicalStoragePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// RecordAnchor identifies a store's first record.
func RecordAnchor(id string, seq int64) string {
	sum := sha256.Sum256([]byte(id + "\x00" + strconv.FormatInt(seq, 10)))
	return hex.EncodeToString(sum[:8])
}

// String renders the cursor. The recipient is hex-encoded because it is
// INJECTIVE: an earlier draft mapped '.' to '_', which made "a.b" and "a_b"
// encode identically and accept each other's cursors.
func (c Cursor) String() string {
	return fmt.Sprintf("%s.%s.%s.c%d:%s.f%d:%s",
		cursorVersion, hex.EncodeToString([]byte(c.Recipient)), c.Source,
		c.Control, anchorOrEmpty(c.ControlAnchor), c.Feedback, anchorOrEmpty(c.FeedbackAnchor))
}

func anchorOrEmpty(a string) string {
	if strings.TrimSpace(a) == "" {
		return EmptyAnchor
	}
	return a
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
		return Cursor{}, fmt.Errorf("mail cursor is malformed: want %s.<recipient>.<source>.c<n>:<anchor>.f<n>:<anchor>", cursorVersion)
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
	control, controlAnchor, err := parseCursorMark(parts[3], "c")
	if err != nil {
		return Cursor{}, err
	}
	feedback, feedbackAnchor, err := parseCursorMark(parts[4], "f")
	if err != nil {
		return Cursor{}, err
	}
	return Cursor{
		Recipient: recipient, Source: source,
		Control: control, ControlAnchor: controlAnchor,
		Feedback: feedback, FeedbackAnchor: feedbackAnchor,
	}, nil
}

func parseCursorMark(field, prefix string) (int64, string, error) {
	if !strings.HasPrefix(field, prefix) {
		return 0, "", fmt.Errorf("mail cursor field is missing its %q marker", prefix)
	}
	mark, anchor, ok := strings.Cut(strings.TrimPrefix(field, prefix), ":")
	if !ok {
		return 0, "", errors.New("mail cursor field is missing its store anchor")
	}
	value, err := strconv.ParseInt(mark, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("mail cursor field is not a number: %w", err)
	}
	if value < 0 {
		return 0, "", errors.New("mail cursor field is negative")
	}
	if strings.TrimSpace(anchor) == "" {
		return 0, "", errors.New("mail cursor field has an empty store anchor")
	}
	if value > 0 && anchor == EmptyAnchor {
		return 0, "", errors.New("mail cursor claims a position in a store it records as empty")
	}
	return value, anchor, nil
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
	// Source is the fingerprint the caller computed for the stores it is
	// reading. It is checked against the cursor here so a Cursor built as a
	// struct literal cannot bypass the binding the parsed path enforces.
	Source string
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
		return page, errors.New("mail: bounded read requires a context")
	}
	// Checked at ENTRY, so a cancelled read fails even when the store is
	// missing or empty and the scan below never runs.
	if err := ctx.Err(); err != nil {
		return page, err
	}
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return page, errors.New("mail: recipient is required")
	}
	if err := opts.validate(); err != nil {
		return page, err
	}
	if cur.Recipient != recipient {
		return page, errors.New("mail: cursor recipient does not match the read recipient")
	}
	if cur.Control < 0 || cur.Feedback < 0 {
		return page, errors.New("mail: cursor marks must not be negative")
	}
	// A Cursor handed in directly must have its SOURCE checked too. Accepting
	// whatever the caller put there let a struct literal bypass the binding
	// the parsed path enforces.
	if opts.Source != "" && cur.Source != "" && cur.Source != opts.Source {
		return page, errors.New("mail: cursor source does not identify the stores being read")
	}
	high := cur.Control

	file, err := os.Open(m.MailFile)
	if err != nil {
		if os.IsNotExist(err) {
			if cur.Control > 0 {
				return page, fmt.Errorf("%w: control mailbox is absent but the cursor is at %d", ErrStorageRewound, cur.Control)
			}
			page.Next = cur.withControl(0, EmptyAnchor).String()
			return page, nil
		}
		return page, fmt.Errorf("mail: open mailbox: %w", err)
	}
	defer file.Close()

	// Stat the OPEN HANDLE, not the path. A stat before the open leaves a
	// window in which the regular file it approved is replaced by something
	// unbounded, so the check would have described a file this read never had.
	info, err := file.Stat()
	if err != nil {
		return page, fmt.Errorf("mail: stat mailbox: %w", err)
	}
	if !info.Mode().IsRegular() {
		return page, errors.New("mail: control mailbox is not a regular file; a bounded read cannot bound a stream")
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxBoundedRecordBytes)
	var quarantineErrs []error
	quarantineFailures := 0
	var maxSeen int64
	anchor := EmptyAnchor
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
		// A record with no usable sequence is malformed, not old. Treating it
		// as <= cursor silently discarded it on every page.
		if env.Sequence <= 0 {
			return page, fmt.Errorf("%w: record %q carries sequence %d", ErrStorageUnordered, env.ID, env.Sequence)
		}
		// STRICTLY increasing. `<` accepted DUPLICATES: two records at the
		// same sequence, read one per page, made the second vanish because
		// the cursor had already advanced to that number.
		if sawAny && env.Sequence <= maxSeen {
			return page, fmt.Errorf("%w: sequence %d does not exceed %d", ErrStorageUnordered, env.Sequence, maxSeen)
		}
		if !sawAny {
			anchor = RecordAnchor(env.ID, env.Sequence)
			if cur.Control > 0 && cur.ControlAnchor != EmptyAnchor && cur.ControlAnchor != anchor {
				return page, fmt.Errorf("%w: the control store's first record changed", ErrStorageRewound)
			}
		}
		maxSeen = env.Sequence
		sawAny = true

		if env.Recipient != recipient && env.Recipient != "all" {
			continue
		}
		if env.Sequence <= cur.Control {
			continue
		}
		if page.Truncated {
			continue
		}
		size, sizeErr := EnvelopeBytes(&env)
		if sizeErr != nil {
			return page, sizeErr
		}
		if size > opts.MaxBytes {
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
	if err := ctx.Err(); err != nil {
		return page, err
	}
	if len(quarantineErrs) > 0 {
		return page, fmt.Errorf("mail: %d quarantine write(s) failed: %w", quarantineFailures, errors.Join(quarantineErrs...))
	}
	// An EMPTY regular file is a truncated store, not a fresh one. Guarding
	// this behind sawAny let a file emptied under a live cursor accept any
	// mark and report a clean, permanently empty page.
	if !sawAny && cur.Control > 0 {
		return page, fmt.Errorf("%w: control store holds no records but the cursor is at %d", ErrStorageRewound, cur.Control)
	}
	if sawAny && cur.Control > maxSeen {
		return page, fmt.Errorf("%w: cursor at %d, highest stored sequence is %d", ErrStorageRewound, cur.Control, maxSeen)
	}
	page.Next = cur.withControl(high, anchor).String()
	return page, nil
}

// withControl returns the cursor advanced over the control store, preserving
// the feedback position untouched.
func (c Cursor) withControl(mark int64, anchor string) Cursor {
	c.Control = mark
	c.ControlAnchor = anchor
	return c
}
