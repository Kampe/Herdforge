package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Audit phases. A repair writes at most two durable records: a PREPARE record
// before it touches the mailbox, and a RESULT record once the outcome is
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
	// and verified but the completion record could not be durably written. The
	// repair DID happen; what is missing is the evidence that it happened. That
	// is reported as a failure rather than a success, because an operator who
	// is told "applied" must be able to find the record that says so.
	ErrRepairCompletionUnrecorded = errors.New("mail repair: row was repaired and verified but the completion record could not be written")
)

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
	data, err := os.ReadFile(m.MailFile + ".seq")
	switch {
	case err == nil:
		cur, parseErr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("corrupt sequence file: %w", parseErr)
		}
		return cur + 1, nil
	case os.IsNotExist(err):
		return 1, nil
	default:
		return 0, fmt.Errorf("failed to read sequence file: %w", err)
	}
}

// setSequenceFloorLocked raises the durable sequence counter to seq in a single
// atomic write. Caller must hold the file lock. It only ever moves the counter
// forward: the same reserve-before-use ordering nextSequenceLocked relies on,
// so a crash can leave a gap but never a duplicate.
func (m *Mailbox) setSequenceFloorLocked(seq int64) error {
	return writeFileAtomic(m.MailFile+".seq", []byte(strconv.FormatInt(seq, 10)), 0644)
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
// the message id, payload and every other row unchanged.
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
	if m == nil {
		return nil, errors.New("mail repair: nil mailbox")
	}
	if strings.TrimSpace(req.ID) == "" {
		return nil, errors.New("mail repair: an exact message id is required")
	}
	if req.Act && strings.TrimSpace(req.Fingerprint) == "" {
		return nil, ErrRepairFingerprintRequired
	}
	if req.Act && strings.TrimSpace(req.Actor) == "" {
		return nil, ErrRepairActorRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var plan *RepairPlan
	err := m.withFileLockContext(ctx, func() error {
		data, err := os.ReadFile(m.MailFile)
		if err != nil {
			return fmt.Errorf("mail repair: read mailbox: %w", err)
		}
		lines := splitLines(string(data))

		targetIdx := -1
		malformedHits := 0
		for i, line := range lines {
			if len(line) == 0 {
				continue
			}
			var env Envelope
			if json.Unmarshal([]byte(line), &env) == nil {
				if env.ID == req.ID {
					// A well-formed row already owns this id. Repairing the
					// malformed twin would create a duplicate delivery.
					return fmt.Errorf("%w: a well-formed row already carries id %q", ErrRepairAmbiguous, req.ID)
				}
				continue
			}
			if rawObjectID(line) != req.ID {
				continue
			}
			malformedHits++
			targetIdx = i
		}
		if malformedHits > 1 {
			return fmt.Errorf("%w: %d malformed rows carry id %q", ErrRepairAmbiguous, malformedHits, req.ID)
		}
		if targetIdx < 0 {
			// The row may be present but so damaged that its id cannot be read
			// as JSON at all. Say that precisely instead of "not found", while
			// still refusing: a row we cannot parse is a row we must not
			// rewrite. This branch only ever produces a better refusal.
			unparseable := 0
			for _, line := range lines {
				if len(line) == 0 || !strings.Contains(line, req.ID) {
					continue
				}
				var env Envelope
				if json.Unmarshal([]byte(line), &env) == nil {
					continue
				}
				if rawObjectID(line) == "" {
					unparseable++
				}
			}
			switch {
			case unparseable == 1:
				return fmt.Errorf("%w: a row mentioning %q is present but does not parse as a JSON object, so only its bytes are recoverable", ErrRepairUnsupported, req.ID)
			case unparseable > 1:
				return fmt.Errorf("%w: %d unparseable rows mention %q", ErrRepairAmbiguous, unparseable, req.ID)
			}
			return fmt.Errorf("%w: %q", ErrRepairNotFound, req.ID)
		}

		original := lines[targetIdx]
		originalHash := sha256OfLine(original)
		if fp := strings.TrimSpace(req.Fingerprint); fp != "" && !strings.EqualFold(fp, originalHash) {
			return fmt.Errorf("%w: have %s", ErrRepairStale, originalHash)
		}
		if err := m.checkQuarantineIdentity(req.ID, originalHash); err != nil {
			return err
		}

		repaired, rawTimestamp, ts, err := buildRepairedEnvelope(original)
		if err != nil {
			return err
		}

		nextSeq, err := m.peekNextSequence()
		if err != nil {
			return err
		}
		// A repair must never file BELOW rows that already exist. The .seq
		// sidecar is the normal authority, but it can be absent or behind (a
		// copied mailbox, a lost sidecar), and a repaired row that sorts under
		// 3000 existing messages is not a recovery. Floor it on what the file
		// actually holds.
		if maxSeq := maxSequenceInLines(lines); maxSeq >= nextSeq {
			nextSeq = maxSeq + 1
		}
		repaired.Sequence = nextSeq
		encoded, err := json.Marshal(repaired)
		if err != nil {
			return fmt.Errorf("mail repair: encode repaired row: %w", err)
		}
		repairedLine := string(encoded)

		plan = &RepairPlan{
			ID:                req.ID,
			OriginalLine:      original,
			OriginalSHA256:    originalHash,
			RepairedLine:      repairedLine,
			RepairedSHA256:    sha256OfLine(repairedLine),
			OriginalTimestamp: rawTimestamp,
			RepairedTimestamp: ts,
			AssignedSequence:  nextSeq,
			Actor:             req.Actor,
			Reason:            req.Reason,
		}
		if !req.Act {
			return nil
		}

		// Settle the sequence BEFORE anything is recorded or written, so the
		// prepare record describes the exact bytes that will land. Reserving
		// early can only ever leave a gap in the counter, never a duplicate —
		// that is nextSequenceLocked's existing contract.
		reserved, err := m.nextSequenceLocked()
		if err != nil {
			return err
		}
		if reserved < nextSeq {
			// Advance the durable counter to the floor in ONE atomic write, so
			// the next ordinary send cannot collide with the repaired row.
			// Reserving one at a time would fsync once per skipped number.
			if err := m.setSequenceFloorLocked(nextSeq); err != nil {
				return err
			}
			reserved = nextSeq
		}
		if reserved != nextSeq {
			repaired.Sequence = reserved
			encoded, err = json.Marshal(repaired)
			if err != nil {
				return fmt.Errorf("mail repair: encode repaired row: %w", err)
			}
			repairedLine = string(encoded)
			plan.AssignedSequence = reserved
			plan.RepairedLine = repairedLine
			plan.RepairedSHA256 = sha256OfLine(repairedLine)
		}

		// PREPARE: the original bytes, and exactly what is about to replace
		// them, are durable BEFORE the mailbox is touched. This record claims
		// nothing about the outcome — Applied stays false until a readback has
		// proven it.
		plan.Phase = RepairPhasePrepare
		plan.PreparedAt = time.Now().UTC()
		if err := m.appendRepairRecord(plan); err != nil {
			return fmt.Errorf("mail repair: durable prepare record: %w", err)
		}

		// From here on every exit writes a RESULT record, so a failed attempt
		// is legible in the artifact rather than merely absent.
		out := make([]string, len(lines))
		copy(out, lines)
		out[targetIdx] = repairedLine
		body := strings.Join(trimTrailingEmpty(out), "\n") + "\n"
		if err := writeFileAtomic(m.MailFile, []byte(body), 0644); err != nil {
			return m.recordRepairFailure(plan, fmt.Errorf("mail repair: durable mailbox write: %w", err))
		}
		if err := m.verifyRepairedRow(targetIdx, repaired); err != nil {
			return m.recordRepairFailure(plan, err)
		}

		// RESULT: only now is the repair durable AND verified.
		plan.Phase = RepairPhaseResult
		plan.Outcome = RepairOutcomeApplied
		plan.Applied = true
		plan.CompletedAt = time.Now().UTC()
		if err := m.appendRepairRecord(plan); err != nil {
			// The row IS repaired and verified, but the evidence saying so is
			// missing. Reporting success here would be the exact lie this
			// phase split exists to prevent: an operator told "applied" must be
			// able to find the record. The prepare record and the original
			// bytes both remain on disk, so the state is recoverable.
			plan.Applied = false
			plan.Outcome = RepairOutcomeFailed
			return fmt.Errorf("%w: %v", ErrRepairCompletionUnrecorded, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
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
	return appendLine(m.MailFile+".repair.jsonl", data)
}

// recordRepairFailure writes the RESULT record for an attempt that did not
// succeed, then returns the original cause.
//
// The recorded reason is REDACTED. The causes that reach here wrap
// *os.PathError — verifyRepairedRow's reread and the mailbox write both carry
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
	data, err := os.ReadFile(m.MailFile + ".quarantine.jsonl")
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

// verifyRepairedRow re-reads the durable file and proves the row it just wrote
// is there and parses to the intended envelope. A write that cannot be read
// back is a failure, not a success.
func (m *Mailbox) verifyRepairedRow(idx int, want *Envelope) error {
	data, err := os.ReadFile(m.MailFile)
	if err != nil {
		return fmt.Errorf("%w: reread: %v", ErrRepairReadbackFailed, err)
	}
	lines := splitLines(string(data))
	if idx >= len(lines) {
		return fmt.Errorf("%w: repaired row index %d is beyond the durable file", ErrRepairReadbackFailed, idx)
	}
	var got Envelope
	if err := json.Unmarshal([]byte(lines[idx]), &got); err != nil {
		return fmt.Errorf("%w: repaired row does not parse: %v", ErrRepairReadbackFailed, err)
	}
	if got.ID != want.ID || got.Sender != want.Sender || got.Recipient != want.Recipient ||
		got.Subject != want.Subject || got.Body != want.Body || got.Sequence != want.Sequence ||
		!got.Timestamp.Equal(want.Timestamp) {
		return fmt.Errorf("%w: durable row differs from the repaired row", ErrRepairReadbackFailed)
	}
	return nil
}

// buildRepairedEnvelope proves the ONLY defect is the legacy timestamp. It
// re-encodes through the canonical Envelope marshaller, which is also what
// makes the row visible to the dedupe scan again: fileHasID looks for the
// compact `"id":"..."` needle that a hand-written spacing style does not
// produce.
func buildRepairedEnvelope(line string) (*Envelope, string, time.Time, error) {
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
// by its exact id.
func rawObjectID(line string) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &raw) != nil {
		return ""
	}
	var id string
	if json.Unmarshal(raw["id"], &id) != nil {
		return ""
	}
	return id
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

func trimTrailingEmpty(lines []string) []string {
	for len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}
