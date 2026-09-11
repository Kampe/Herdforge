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

// RepairPlan is what a repair would do, or did. Report-only and applied runs
// return the same shape, so an operator reads identical evidence either way,
// and the applied form is what lands in the durable audit artifact.
type RepairPlan struct {
	ID                string    `json:"id"`
	OriginalLine      string    `json:"original_line"`
	OriginalSHA256    string    `json:"original_sha256"`
	RepairedLine      string    `json:"repaired_line"`
	RepairedSHA256    string    `json:"repaired_sha256"`
	OriginalTimestamp string    `json:"original_timestamp"`
	RepairedTimestamp time.Time `json:"repaired_timestamp"`
	AssignedSequence  int64     `json:"assigned_sequence"`
	Applied           bool      `json:"applied"`
	Actor             string    `json:"actor,omitempty"`
	Reason            string    `json:"reason,omitempty"`
	RepairedAt        time.Time `json:"repaired_at,omitempty"`
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

		// Audit before mutating: the original bytes must be durably recoverable
		// from this artifact even if everything after it fails.
		plan.Applied = true
		plan.RepairedAt = time.Now().UTC()
		auditRecord, err := json.Marshal(plan)
		if err != nil {
			return fmt.Errorf("mail repair: encode audit record: %w", err)
		}
		if err := appendLine(m.MailFile+".repair.jsonl", auditRecord); err != nil {
			plan.Applied = false
			return fmt.Errorf("mail repair: durable audit artifact: %w", err)
		}

		// Reserve the sequence only now that the repair is actually happening.
		reserved, err := m.nextSequenceLocked()
		if err != nil {
			plan.Applied = false
			return err
		}
		if reserved < nextSeq {
			// Advance the durable counter to the floor in ONE atomic write, so
			// the next ordinary send cannot collide with the repaired row.
			// Reserving one at a time would fsync once per skipped number.
			if err := m.setSequenceFloorLocked(nextSeq); err != nil {
				plan.Applied = false
				return err
			}
			reserved = nextSeq
		}
		if reserved != nextSeq {
			repaired.Sequence = reserved
			encoded, err = json.Marshal(repaired)
			if err != nil {
				plan.Applied = false
				return fmt.Errorf("mail repair: encode repaired row: %w", err)
			}
			repairedLine = string(encoded)
			plan.AssignedSequence = reserved
			plan.RepairedLine = repairedLine
			plan.RepairedSHA256 = sha256OfLine(repairedLine)
		}

		out := make([]string, len(lines))
		copy(out, lines)
		out[targetIdx] = repairedLine
		body := strings.Join(trimTrailingEmpty(out), "\n") + "\n"
		if err := writeFileAtomic(m.MailFile, []byte(body), 0644); err != nil {
			plan.Applied = false
			return fmt.Errorf("mail repair: durable mailbox write: %w", err)
		}
		if err := m.verifyRepairedRow(targetIdx, repaired); err != nil {
			plan.Applied = false
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
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
