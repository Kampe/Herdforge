package mail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// Audit phases. A repair writes at most two durable records per selected row:
// a PREPARE before it touches the mailbox, and a RESULT once the outcome is
// actually known. Nothing claims success in advance.
const (
	RepairPhasePrepare = "prepare"
	RepairPhaseResult  = "result"

	RepairOutcomeApplied = "applied"
	RepairOutcomeFailed  = "failed"
)

// RepairPlan is what a repair would do, or did. Report-only and applied runs
// return the same shape, so an operator reads identical evidence either way.
//
// It is also the durable audit record, which is why Phase and Outcome exist.
// An earlier version wrote ONE record, stamped Applied=true, BEFORE reserving
// the sequence, writing the mailbox, or reading it back — so a mutation that
// failed afterwards left permanent evidence claiming it had succeeded. Applied
// is now only ever true on a RESULT record whose readback has already been
// verified.
type RepairPlan struct {
	ID      string `json:"id"`
	Phase   string `json:"phase,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	// Failure is the reason a RESULT record reports RepairOutcomeFailed. It is
	// what makes a failed attempt legible instead of merely absent.
	Failure           string    `json:"failure,omitempty"`
	OriginalLine      string    `json:"original_line"`
	OriginalSHA256    string    `json:"original_sha256"`
	RepairedLine      string    `json:"repaired_line"`
	RepairedSHA256    string    `json:"repaired_sha256"`
	OriginalTimestamp string    `json:"original_timestamp"`
	RepairedTimestamp time.Time `json:"repaired_timestamp"`
	AssignedSequence  int64     `json:"assigned_sequence"`
	// Applied is true only when the repaired row is durably on disk AND has
	// been read back and compared field by field.
	Applied     bool      `json:"applied"`
	Actor       string    `json:"actor,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	PreparedAt  time.Time `json:"prepared_at,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// RepairRequest bounds one operator recovery to one exact row.
//
// Fingerprint is the sha256 of the exact malformed line the operator reviewed.
// It is REQUIRED to act: the repair is a compare-and-swap, so an operator can
// only ever rewrite the exact bytes they read in a report-only run. Report-only
// runs may omit it, and emit the fingerprint to be used for the act.
type RepairRequest struct {
	ID          string
	Fingerprint string
	Act         bool
	Actor       string
	Reason      string
}

var (
	ErrRepairNotFound       = errors.New("mail repair: no malformed row with that id")
	ErrRepairAmbiguous      = errors.New("mail repair: ambiguous duplicate id")
	ErrRepairStale          = errors.New("mail repair: fingerprint does not match the current bytes")
	ErrRepairUnsupported    = errors.New("mail repair: row defect is not a normalizable legacy timestamp")
	ErrRepairPrivileged     = errors.New("mail repair: privileged signed or control message must not be rewritten")
	ErrRepairReadbackFailed = errors.New("mail repair: durable readback did not match the repaired row")
	// ErrRepairFingerprintRequired keeps acting a compare-and-swap: there is no
	// way to rewrite a row without first naming the exact bytes being replaced.
	ErrRepairFingerprintRequired = errors.New("mail repair: --act requires the exact --fingerprint from a report-only run")
	// ErrRepairConflictingOriginals fires when the quarantine record for this id
	// holds more than one DISTINCT original. Repeated identical copies are
	// normal reader behaviour and are not a conflict; differing bytes are.
	ErrRepairConflictingOriginals = errors.New("mail repair: quarantine holds conflicting originals for this id")
	// ErrRepairActorRequired mirrors the CLI's --actor requirement at the
	// package boundary, so no caller can apply an unattributed mutation.
	ErrRepairActorRequired = errors.New("mail repair: acting requires an actor")
	// ErrRepairCompletionUnrecorded is returned when the mailbox was repaired
	// and verified but the completion record could not be durably recorded.
	//
	// The repair DID happen: the mailbox write and the readback both succeeded
	// before this. What is unconfirmed is the RECORD of it. appendLine writes
	// the bytes and only then fsyncs, so a sync failure leaves a completion
	// record that is present on the live filesystem but not proven to survive a
	// crash. A reader may therefore see a result row saying applied — and that
	// row is TRUE, not a fabrication; only its durability is unconfirmed.
	//
	// This is reported as a failure because the caller is the only party that
	// knows the record is unconfirmed: that fact exists in the returned error
	// and nowhere in the artifact, and no later consumer can recover it by
	// reading bytes.
	ErrRepairCompletionUnrecorded = errors.New("mail repair: row was repaired and verified, but its completion record is not durably recorded and any result row present must be treated as durability-unconfirmed")
	// ErrRepairDuplicateKeys fires when a row repeats a top-level key. Decoding
	// keeps only the last value, so normalizing such a row would silently
	// discard an original the operator never saw.
	ErrRepairDuplicateKeys = errors.New("mail repair: row carries repeated top-level keys")
	// ErrRepairUnrelatedCorruption fires when some OTHER row in the mailbox is
	// malformed or has unreadable identity. Repairing beside it would report
	// success while every strict reader stayed blocked, and that row may itself
	// hold a conflicting identity nobody can read.
	ErrRepairUnrelatedCorruption = errors.New("mail repair: another row in the mailbox is malformed or has unreadable identity")
)

// unrelatedCorruptionError names where the trouble is and what class it is,
// never what the other row contains.
func unrelatedCorruptionError(found []string) error {
	return fmt.Errorf("%w: %d row(s) must be resolved first (%s)",
		ErrRepairUnrelatedCorruption, len(found), strings.Join(found, "; "))
}

// envelopeJSONKeys is every key the Envelope encoder can round-trip. A row
// carrying anything else cannot be re-encoded without silently dropping that
// field, so the repair refuses rather than losing it.
var envelopeJSONKeys = map[string]bool{
	"id": true, "seq": true, "sender": true, "recipient": true, "subject": true,
	"body": true, "read": true, "timestamp": true,
	"original_source_host": true, "original_source_id": true, "binding": true,
}

// privilegedJSONKeys mark a row whose bytes are themselves the authority.
// Re-encoding one would invalidate the very evidence it carries, so those are
// refused outright and must go through the authenticated control path.
var privilegedJSONKeys = []string{"signature", "signed", "attestation", "hmac", "control_token", "nonce"}

// legacyTimestampLayouts are the ordinary, unambiguous forms a hand-written or
// non-Go producer emits instead of RFC3339: a numeric offset with no colon.
// Every layout here names one exact instant; nothing here guesses a zone.
var legacyTimestampLayouts = []string{
	"2006-01-02T15:04:05.999999999-0700",
	"2006-01-02T15:04:05-0700",
	"2006-01-02T15:04:05.999999999Z0700",
}

func sha256OfLine(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])
}

// peekNextSequence reports the sequence a repair WOULD take without reserving
// it. Report-only must not consume a sequence number.
func (m *Mailbox) peekNextSequence() (int64, error) {
	data, err := os.ReadFile(m.SequencePath())
	switch {
	case err == nil:
		cur, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("%w: corrupt sequence file: %v", ErrSequenceInvalid, parseErr)
		}
		// Checked, and checked HERE: a report must refuse an unusable counter
		// rather than print a negative sequence that a later act would then
		// silently replace with 1, filing the row beneath the whole history.
		return nextSequenceValue(cur)
	case os.IsNotExist(err):
		return 1, nil
	default:
		return 0, fmt.Errorf("failed to read sequence file: %w", redactErr(err))
	}
}

// setSequenceFloorLocked raises the durable sequence counter to seq in a single
// atomic write. Caller must hold the file lock. It only ever moves the counter
// forward: the same reserve-before-use ordering nextSequenceLocked relies on,
// so a crash can leave a gap but never a duplicate.
func (m *Mailbox) setSequenceFloorLocked(seq int64) error {
	return writeFileAtomic(m.SequencePath(), []byte(strconv.FormatInt(seq, 10)), 0644)
}

// normalizeLegacyTimestamp parses a non-RFC3339 but unambiguous timestamp,
// preserving the exact instant. It never reads the clock and never invents a
// zone: a value it cannot parse exactly is reported unsupported.
func normalizeLegacyTimestamp(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		// Already canonical: not a defect this operation repairs.
		return ts, false
	}
	for _, layout := range legacyTimestampLayouts {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// RepairMalformedRow is the single narrow operator recovery for one exact
// quarantined row: it normalizes a clearly parseable legacy timestamp, assigns
// a proper monotonic sequence under the canonical mailbox lock, and preserves
// the message id, payload, row position and every other row unchanged.
// Use RepairMalformedRows to explicitly select a batch.
//
// It is REPORT-ONLY unless req.Act is set. It never repairs more than the one
// row named by req.ID, never sweeps other malformed rows, and refuses rather
// than guessing: ambiguity, a stale fingerprint, a defect other than the
// timestamp, an unknown field it would have to drop, or a privileged signed
// message all fail closed with the mailbox untouched.
//
// This does not change how anything PARSES. ReadInbox still quarantines every
// malformed line and still skips nothing; this is the missing recovery path
// for a row that quarantine already caught.
func (m *Mailbox) RepairMalformedRow(ctx context.Context, req RepairRequest) (*RepairPlan, error) {
	plans, err := m.RepairMalformedRows(ctx, []RepairRequest{req})
	if err != nil {
		return nil, err
	}
	return plans[0], nil
}

// appendRepairRecord durably appends one audit record. It takes a copy, so a
// later phase mutating the caller's plan cannot retroactively change what an
// earlier record said.
func (m *Mailbox) appendRepairRecord(plan *RepairPlan) error {
	if plan == nil {
		return errors.New("mail repair: nil audit record")
	}
	record := *plan
	data, err := json.Marshal(&record)
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}
	return appendLine(m.RepairAuditPath(), data)
}

// recordRepairFailure writes the RESULT record for an attempt that did not
// succeed, then returns the original cause.
//
// The recorded reason is REDACTED. The causes that reach here wrap
// *os.PathError — verifyRepairedMailbox's reread and the mailbox write both carry
// the mailbox path — and this record is durable, so storing the raw error
// would persist a host-absolute path into an artifact. redactErr is the
// package's existing answer to exactly that (see its use on the outbox path
// error in mail.go); it reduces a path error to op, basename and underlying
// cause. The only host-absolute text an audit record may carry is whatever was
// already inside the operator's own message payload.
//
// The RETURNED error is deliberately NOT redacted: redactErr rebuilds the
// error and so drops the sentinel chain, and callers match on
// ErrRepairReadbackFailed. Redaction is about what is persisted, not about
// what the caller may inspect in memory.
//
// If the failure record itself cannot be written, both errors are returned
// together rather than one masking the other: the operator needs to know the
// repair failed AND that the artifact is now incomplete. Either way the
// prepare record and the original bytes are already durable, so nothing is
// unrecoverable and nothing reports success.
func (m *Mailbox) recordRepairFailure(plan *RepairPlan, cause error) error {
	if cause == nil {
		// Reaching here means a caller decided the repair failed. A nil cause
		// would record "failed" with no reason, which is worse than saying the
		// reason was lost, so make that explicit rather than storing nothing
		// and %w-wrapping a nil below.
		cause = errors.New("mail repair: failure recorded without a cause")
	}
	plan.Phase = RepairPhaseResult
	plan.Outcome = RepairOutcomeFailed
	plan.Applied = false
	plan.CompletedAt = time.Now().UTC()
	plan.Failure = redactErr(cause).Error()
	if err := m.appendRepairRecord(plan); err != nil {
		return fmt.Errorf("%w (and the failure record could not be written: %v)", cause, redactErr(err))
	}
	return cause
}

// checkQuarantineIdentity compares the live malformed row against what the
// quarantine artifact recorded for the same id.
//
// ReadInbox re-quarantines an unrepaired row on EVERY read, so the artifact
// legitimately accumulates many copies of the same original. Those are one
// piece of evidence, not many, and must never make a repair permanently
// ambiguous — so identity here is the sha256 of the recorded bytes, deduped.
//
// What IS a conflict: two DIFFERENT originals recorded under the same id (the
// row was replaced between reads), or a recorded original that does not match
// the row currently live in the mailbox. Either means the bytes an operator
// reviewed are not the bytes on disk, so the repair refuses rather than acting
// on evidence that has moved. A missing or rotated artifact is not an error:
// the live row plus the required fingerprint still bound the operation.
func (m *Mailbox) checkQuarantineIdentity(id, liveHash string) error {
	data, err := os.ReadFile(m.QuarantinePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("mail repair: read quarantine artifact: %w", err)
	}
	distinct := map[string]bool{}
	for _, line := range splitLines(string(data)) {
		if len(line) == 0 {
			continue
		}
		var entry QuarantineEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if rawObjectID(entry.Line) != id {
			continue
		}
		distinct[sha256OfLine(entry.Line)] = true
	}
	switch len(distinct) {
	case 0:
		return nil
	case 1:
		for hash := range distinct {
			if !strings.EqualFold(hash, liveHash) {
				return fmt.Errorf("%w: quarantined original %s is not the live row %s", ErrRepairStale, hash, liveHash)
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %d distinct originals recorded for %q", ErrRepairConflictingOriginals, len(distinct), id)
	}
}

// verifyRepairedMailbox proves the durable file is byte-for-byte the file this
// repair intended to write, which covers every field of the target row and the
// one-row-only promise in a single comparison. The target row is then re-parsed
// so a failure names that row rather than only a byte count.
func (m *Mailbox) verifyRepairedMailbox(expected []byte, want *Envelope) error {
	got, err := os.ReadFile(m.MailFile)
	if err != nil {
		return fmt.Errorf("%w: reread: %v", ErrRepairReadbackFailed, redactErr(err))
	}
	if !bytes.Equal(got, expected) {
		return fmt.Errorf("%w: durable mailbox is not the exact bytes this repair wrote (%d bytes on disk, %d intended)",
			ErrRepairReadbackFailed, len(got), len(expected))
	}
	line, ok := findLineByID(got, want.ID)
	if !ok {
		return fmt.Errorf("%w: repaired row %q is not present in the durable mailbox", ErrRepairReadbackFailed, want.ID)
	}
	var durable Envelope
	if err := json.Unmarshal(line, &durable); err != nil {
		return fmt.Errorf("%w: repaired row does not parse: %v", ErrRepairReadbackFailed, err)
	}
	if !sameEnvelope(&durable, want) {
		return fmt.Errorf("%w: durable row differs from the repaired row", ErrRepairReadbackFailed)
	}
	return nil
}

// sameEnvelope compares every field an Envelope carries. A new Envelope field
// must be added here too.
func sameEnvelope(a, b *Envelope) bool {
	return a.ID == b.ID &&
		a.Sequence == b.Sequence &&
		a.Sender == b.Sender &&
		a.Recipient == b.Recipient &&
		a.Subject == b.Subject &&
		a.Body == b.Body &&
		a.Read == b.Read &&
		a.Timestamp.Equal(b.Timestamp) &&
		a.OriginalSourceHost == b.OriginalSourceHost &&
		a.OriginalSourceID == b.OriginalSourceID &&
		a.Binding == b.Binding
}

// findLineByID returns the first well-formed row carrying id.
func findLineByID(data []byte, id string) ([]byte, bool) {
	for _, sp := range lineSpans(data) {
		line := data[sp.start:sp.end]
		if len(line) == 0 {
			continue
		}
		var env Envelope
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		if env.ID == id {
			return line, true
		}
	}
	return nil, false
}

// lineSpan is one line's byte range within the mailbox, excluding its
// terminating newline when it has one.
type lineSpan struct{ start, end int }

// lineSpans splits on the same rule as splitLines, keeping the offsets needed
// to edit one row in place.
func lineSpans(data []byte) []lineSpan {
	var spans []lineSpan
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			spans = append(spans, lineSpan{start: start, end: i})
			start = i + 1
		}
	}
	if start < len(data) {
		spans = append(spans, lineSpan{start: start, end: len(data)})
	}
	return spans
}

// buildRepairedEnvelope proves the ONLY defect is the legacy timestamp. It
// re-encodes through the canonical Envelope marshaller, which is also what
// makes the row visible to the dedupe scan again: fileHasID looks for the
// compact `"id":"..."` needle that a hand-written spacing style does not
// produce.
func buildRepairedEnvelope(line string) (*Envelope, string, time.Time, error) {
	scan, isObject := scanJSONObject([]byte(line))
	if !isObject {
		return nil, "", time.Time{}, fmt.Errorf("%w: row is not a well-formed JSON object", ErrRepairUnsupported)
	}
	// Refuse before decoding: the map below keeps only the last value for a
	// repeated key, so repairing such a row would discard an original value.
	if key, dup := duplicateKey(scan.Keys); dup {
		return nil, "", time.Time{}, fmt.Errorf("%w: key %q appears more than once", ErrRepairDuplicateKeys, key)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("%w: row is not a well-formed JSON object: %v", ErrRepairUnsupported, err)
	}
	for _, key := range privilegedJSONKeys {
		if _, ok := raw[key]; ok {
			return nil, "", time.Time{}, fmt.Errorf("%w: row carries %q", ErrRepairPrivileged, key)
		}
	}
	for key := range raw {
		if !envelopeJSONKeys[key] {
			return nil, "", time.Time{}, fmt.Errorf("%w: row carries unknown field %q that a re-encode would drop", ErrRepairUnsupported, key)
		}
	}
	rawTS, ok := raw["timestamp"]
	if !ok {
		return nil, "", time.Time{}, fmt.Errorf("%w: row has no timestamp", ErrRepairUnsupported)
	}
	var tsText string
	if err := json.Unmarshal(rawTS, &tsText); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("%w: timestamp is not a string", ErrRepairUnsupported)
	}
	ts, normalized := normalizeLegacyTimestamp(tsText)
	if !normalized {
		return nil, "", time.Time{}, fmt.Errorf("%w: timestamp %q is not a recognized legacy form", ErrRepairUnsupported, tsText)
	}

	// Substitute the canonical timestamp and require the rest of the row to
	// decode cleanly. If it still fails, the defect was never just the
	// timestamp and this operation must not touch it.
	canonical, err := json.Marshal(ts)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("%w: canonical timestamp: %v", ErrRepairUnsupported, err)
	}
	raw["timestamp"] = canonical
	rebuilt, err := json.Marshal(raw)
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("%w: re-encode: %v", ErrRepairUnsupported, err)
	}
	var env Envelope
	if err := json.Unmarshal(rebuilt, &env); err != nil {
		return nil, "", time.Time{}, fmt.Errorf("%w: row still fails to decode after normalization: %v", ErrRepairUnsupported, err)
	}
	if strings.TrimSpace(env.ID) == "" {
		return nil, "", time.Time{}, fmt.Errorf("%w: row has no id", ErrRepairUnsupported)
	}
	return &env, tsText, ts, nil
}

// rawObjectID reads the id of a row that does NOT parse as an Envelope,
// tolerating any JSON spacing style, so a malformed row can still be addressed
// by its exact id. A row with repeated top-level keys, or without exactly one
// id, is not addressable.
func rawObjectID(line string) string {
	scan, ok := scanJSONObject([]byte(line))
	if !ok {
		return ""
	}
	if _, dup := duplicateKey(scan.Keys); dup {
		return ""
	}
	if len(scan.IDs) != 1 {
		return ""
	}
	return scan.IDs[0]
}

// objectScan is the narrow top-level view this recovery needs of one row:
// every top-level key name, and every top-level "id" whose value is a string.
//
// Both are collected because a row may repeat a key. IDs is a list, not one
// value, so a row declaring id TARGET and then id OTHER is known to carry
// TARGET even though every map-based decode — including Envelope's — keeps only
// the last.
type objectScan struct {
	Keys []string
	IDs  []string
}

// scanJSONObject walks a token stream rather than decoding into a map. A map
// keeps only the last value for a repeated key, so a row carrying two different
// bodies decodes to one and the other is gone — and this operation would then
// "normalize" the row while discarding an original value it never showed
// anyone. Escape forms are decoded by the tokenizer, so "body" and "body"
// are the same key, and an escaped id value compares equal to its plain form.
//
// ok is true only for a COMPLETE object: the closing brace is consumed and
// nothing but whitespace may follow, so a truncated or trailing-garbage row is
// reported as not an object rather than as a valid one.
func scanJSONObject(line []byte) (objectScan, bool) {
	var scan objectScan
	dec := json.NewDecoder(bytes.NewReader(line))
	tok, err := dec.Token()
	if err != nil {
		return objectScan{}, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return objectScan{}, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return objectScan{}, false
		}
		key, isString := keyTok.(string)
		if !isString {
			return objectScan{}, false
		}
		scan.Keys = append(scan.Keys, key)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return objectScan{}, false
		}
		if key == "id" {
			var id string
			if json.Unmarshal(value, &id) == nil {
				scan.IDs = append(scan.IDs, id)
			}
		}
	}
	// Consume the closing brace.
	closeTok, err := dec.Token()
	if err != nil {
		return objectScan{}, false
	}
	if delim, isDelim := closeTok.(json.Delim); !isDelim || delim != '}' {
		return objectScan{}, false
	}
	// Require end of input; JSON whitespace, including a trailing CR, is fine.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return objectScan{}, false
	}
	return scan, true
}

func duplicateKey(keys []string) (string, bool) {
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		if seen[key] {
			return key, true
		}
		seen[key] = true
	}
	return "", false
}

// maxSequenceInLines reports the highest sequence any well-formed row already
// carries, so a repair can never be filed beneath the existing history.
func maxSequenceInLines(lines []string) int64 {
	var max int64
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var env Envelope
		if json.Unmarshal([]byte(line), &env) != nil {
			continue
		}
		if env.Sequence > max {
			max = env.Sequence
		}
	}
	return max
}

// MaxRepairBatch bounds explicit operator selection. No repair discovers or
// automatically includes additional malformed rows.
const MaxRepairBatch = 32

// RepairMalformedRows repairs an explicit set in one mailbox transaction.
// Every request must use the same Act, Actor and Reason. Fingerprints bind each
// selected original independently; any refusal leaves every mailbox byte alone.
// A successful act durably retains every original before replacing the mailbox
// once, under its canonical lock. Sequence gaps after an I/O failure are safe.
// Plans are returned in request order, which also determines sequence order.
func (m *Mailbox) RepairMalformedRows(ctx context.Context, requests []RepairRequest) ([]*RepairPlan, error) {
	if m == nil {
		return nil, errors.New("mail repair: nil mailbox")
	}
	if len(requests) == 0 || len(requests) > MaxRepairBatch {
		return nil, fmt.Errorf("mail repair: select between 1 and %d exact ids", MaxRepairBatch)
	}
	selected := make(map[string]bool, len(requests))
	for _, req := range requests {
		if strings.TrimSpace(req.ID) == "" {
			return nil, errors.New("mail repair: an exact message id is required")
		}
		if selected[req.ID] {
			return nil, fmt.Errorf("%w: id %q selected more than once", ErrRepairAmbiguous, req.ID)
		}
		selected[req.ID] = true
		if req.Act && strings.TrimSpace(req.Fingerprint) == "" {
			return nil, ErrRepairFingerprintRequired
		}
		if req.Act && strings.TrimSpace(req.Actor) == "" {
			return nil, ErrRepairActorRequired
		}
		if req.Act != requests[0].Act || req.Actor != requests[0].Actor || req.Reason != requests[0].Reason {
			return nil, errors.New("mail repair: batch requests must share act, actor and reason")
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var plans []*RepairPlan
	err := m.withFileLockContext(ctx, func() error {
		data, err := os.ReadFile(m.MailFile)
		if err != nil {
			return fmt.Errorf("mail repair: read mailbox: %w", redactErr(err))
		}
		spans := lineSpans(data)
		lines := make([]string, len(spans))
		for i, span := range spans {
			lines[i] = string(data[span.start:span.end])
		}
		indices, otherBad, err := selectRepairRows(lines, selected)
		if err != nil {
			return err
		}
		repaired := make([]*Envelope, len(requests))
		for i, req := range requests {
			idx, ok := indices[req.ID]
			if !ok {
				return missingRepairRow(lines, req.ID, otherBad)
			}
			original := lines[idx]
			originalHash := sha256OfLine(original)
			if fp := strings.TrimSpace(req.Fingerprint); fp != "" && !strings.EqualFold(fp, originalHash) {
				return fmt.Errorf("%w: id %q has %s", ErrRepairStale, req.ID, originalHash)
			}
			if err := m.checkQuarantineIdentity(req.ID, originalHash); err != nil {
				return err
			}
			env, rawTimestamp, ts, err := buildRepairedEnvelope(original)
			if err != nil {
				return err
			}
			repaired[i] = env
			plans = append(plans, &RepairPlan{
				ID: req.ID, OriginalLine: original, OriginalSHA256: originalHash,
				OriginalTimestamp: rawTimestamp, RepairedTimestamp: ts,
				Actor: req.Actor, Reason: req.Reason,
			})
		}
		// All selected targets have passed their specific checks; unrelated
		// corruption still refuses before sequence reservation or any audit.
		if len(otherBad) > 0 {
			return unrelatedCorruptionError(otherBad)
		}
		nextSeq, err := m.peekNextSequence()
		if err != nil {
			return err
		}
		if maxSeq := maxSequenceInLines(lines); maxSeq >= nextSeq {
			nextSeq, err = nextSequenceValue(maxSeq)
			if err != nil {
				return fmt.Errorf("%w (highest existing row)", err)
			}
		}
		// Check the entire range before reserving any of it. Overflow in a
		// later selected row must not consume a sequence for an earlier row.
		for i, plan := range plans {
			if i > 0 {
				nextSeq, err = nextSequenceValue(nextSeq)
				if err != nil {
					return err
				}
			}
			repaired[i].Sequence = nextSeq
			encoded, err := json.Marshal(repaired[i])
			if err != nil {
				return fmt.Errorf("mail repair: encode repaired row: %w", err)
			}
			plan.AssignedSequence = nextSeq
			plan.RepairedLine = string(encoded)
			plan.RepairedSHA256 = sha256OfLine(plan.RepairedLine)
		}
		if !requests[0].Act {
			return nil
		}
		// Reserve the complete, checked range in one durable write. The lock
		// covers both peeking and reservation; a crash can leave only a gap.
		if err := m.setSequenceFloorLocked(nextSeq); err != nil {
			return err
		}
		for _, plan := range plans {
			plan.Phase = RepairPhasePrepare
			plan.PreparedAt = time.Now().UTC()
			if err := m.appendRepairRecord(plan); err != nil {
				return fmt.Errorf("mail repair: durable prepare record: %w", err)
			}
		}
		// Splice in physical file order, preserving all framing and every byte
		// outside selected spans even when request order differs from row order.
		replacements := make(map[int]string, len(plans))
		for _, plan := range plans {
			replacements[indices[plan.ID]] = plan.RepairedLine
		}
		expected := make([]byte, 0, len(data))
		cursor := 0
		for i, span := range spans {
			if replacement, ok := replacements[i]; ok {
				expected = append(expected, data[cursor:span.start]...)
				expected = append(expected, replacement...)
				cursor = span.end
			}
		}
		expected = append(expected, data[cursor:]...)
		if err := writeFileAtomic(m.MailFile, expected, 0644); err != nil {
			return m.recordRepairBatchFailure(plans, fmt.Errorf("mail repair: durable mailbox write: %w", err))
		}
		for _, env := range repaired {
			if err := m.verifyRepairedMailbox(expected, env); err != nil {
				return m.recordRepairBatchFailure(plans, err)
			}
		}
		// No result claims success until the entire replacement is durable and
		// verified. A later audit failure never rolls back retained evidence.
		for _, plan := range plans {
			plan.Phase = RepairPhaseResult
			plan.Outcome = RepairOutcomeApplied
			plan.Applied = true
			plan.CompletedAt = time.Now().UTC()
			if err := m.appendRepairRecord(plan); err != nil {
				return fmt.Errorf("%w: %v", ErrRepairCompletionUnrecorded, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plans, nil
}

func selectRepairRows(lines []string, selected map[string]bool) (map[string]int, []string, error) {
	indices := make(map[string]int, len(selected))
	var otherBad []string
	noteBad := func(i int, why string) {
		otherBad = append(otherBad, fmt.Sprintf("line %d: %s", i+1, why))
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		scan, isObject := scanJSONObject([]byte(line))
		if !isObject {
			noteBad(i, "not a complete JSON object")
			continue
		}
		if dupKey, dup := duplicateKey(scan.Keys); dup {
			for _, id := range scan.IDs {
				if selected[id] {
					return nil, nil, fmt.Errorf("%w: a row carrying id %q repeats top-level key %q", ErrRepairDuplicateKeys, id, dupKey)
				}
			}
			noteBad(i, fmt.Sprintf("repeated top-level key %q", dupKey))
			continue
		}
		if len(scan.IDs) != 1 || strings.TrimSpace(scan.IDs[0]) == "" {
			noteBad(i, "no readable unique identity")
			continue
		}
		id := scan.IDs[0]
		var env Envelope
		if json.Unmarshal([]byte(line), &env) == nil {
			if selected[id] {
				return nil, nil, fmt.Errorf("%w: a well-formed row already carries id %q", ErrRepairAmbiguous, id)
			}
			continue
		}
		if selected[id] {
			if _, exists := indices[id]; exists {
				return nil, nil, fmt.Errorf("%w: multiple malformed rows carry id %q", ErrRepairAmbiguous, id)
			}
			indices[id] = i
			continue
		}
		noteBad(i, "not a well-formed envelope")
	}
	return indices, otherBad, nil
}

func missingRepairRow(lines []string, id string, otherBad []string) error {
	// This heuristic chooses only a refusal message, never a row to mutate.
	for _, line := range lines {
		if !strings.Contains(line, id) {
			continue
		}
		var env Envelope
		if json.Unmarshal([]byte(line), &env) != nil && rawObjectID(line) == "" {
			return fmt.Errorf("%w: a row mentioning %q is present but does not parse as a JSON object, so only its bytes are recoverable", ErrRepairUnsupported, id)
		}
	}
	if len(otherBad) > 0 {
		return unrelatedCorruptionError(otherBad)
	}
	return fmt.Errorf("%w: %q", ErrRepairNotFound, id)
}

func (m *Mailbox) recordRepairBatchFailure(plans []*RepairPlan, cause error) error {
	var failures []error
	for _, plan := range plans {
		failures = append(failures, m.recordRepairFailure(plan, cause))
	}
	return errors.Join(failures...)
}
