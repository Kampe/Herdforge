package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Sequence-order recovery for a store whose sequences do not ascend in file
// order. Bounded paging refuses that shape because a seq high-water mark
// would skip later lower-numbered rows. This walk keeps every row in place,
// rewrites only inverting seq values, and never touches signed controls.
//
// It is REPORT-ONLY unless Act is set. Changed rows are bounded: a store
// that would rewrite more than MaxSequenceOrderRepairs cannot be applied in
// one transaction, so a large live mailbox cannot be mutated by accident.

// MaxSequenceOrderRepairs is the maximum number of rows one sequence-order
// act may rewrite. It matches MaxRepairBatch so operator selection stays
// reviewable. Report-only still reports the full changed count.
const MaxSequenceOrderRepairs = MaxRepairBatch

var (
	// ErrSequenceOrderBound fires when applying would rewrite more rows than
	// the operator bound. Report-only still returns the truncated plan.
	ErrSequenceOrderBound = errors.New("mail repair: sequence-order plan exceeds the 32-row bound")
	// ErrSequenceOrderStaleCursor fires when the operator-supplied paging
	// cursor does not bind to this store's current prefix. Acting anyway
	// would rewrite underneath a position the operator thinks is live.
	ErrSequenceOrderStaleCursor = errors.New("mail repair: paging cursor is stale for this store")
)

// SequenceOrderRequest is one whole-store sequence-order recovery.
type SequenceOrderRequest struct {
	Act          bool
	Actor        string
	Reason       string
	Fingerprints []string
	// Cursor, Recipient and FeedbackDir bind an optional paging cursor.
	// Empty cursor is ignored. A non-empty cursor must parse for Recipient
	// against this mailbox and still match the current recipient prefix.
	Cursor      string
	Recipient   string
	FeedbackDir string
}

// SequenceOrderReport is the operator evidence for a sequence-order walk.
type SequenceOrderReport struct {
	Defect      string        `json:"defect"`
	TotalRows   int           `json:"total_rows"`
	Changed     int           `json:"changed"`
	Kept        int           `json:"kept"`
	Bound       int           `json:"bound"`
	Truncated   bool          `json:"truncated"`
	StoreSHA256 string        `json:"store_sha256"`
	Plans       []*RepairPlan `json:"plans"`
}

type sequenceEdit struct {
	index int
	id    string
	env   *Envelope
	plan  *RepairPlan
}

// RepairSequenceOrder restores file-order monotonic sequences without
// reordering or dropping rows. Duplicate ids, privileged signed rows that
// would be rewritten, stale fingerprints, stale paging cursors, and plans
// over MaxSequenceOrderRepairs all refuse with the mailbox untouched.
func (m *Mailbox) RepairSequenceOrder(ctx context.Context, req SequenceOrderRequest) (*SequenceOrderReport, error) {
	if m == nil {
		return nil, errors.New("mail repair: nil mailbox")
	}
	if req.Act && strings.TrimSpace(req.Actor) == "" {
		return nil, ErrRepairActorRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	report := &SequenceOrderReport{
		Defect: "sequence-order",
		Bound:  MaxSequenceOrderRepairs,
		Plans:  []*RepairPlan{},
	}
	err := m.withFileLockContext(ctx, func() error {
		data, err := os.ReadFile(m.MailFile)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("mail repair: read mailbox: %w", err)
			}
			return fmt.Errorf("mail repair: read mailbox: %w", redactErr(err))
		}
		report.StoreSHA256 = sha256OfLine(string(data))
		spans := lineSpans(data)
		lines := make([]string, len(spans))
		for i, span := range spans {
			lines[i] = string(data[span.start:span.end])
		}
		if err := m.checkSequenceOrderCursor(req, lines); err != nil {
			return err
		}
		edits, kept, total, err := planSequenceOrderEdits(lines, req.Actor, req.Reason)
		if err != nil {
			return err
		}
		report.TotalRows = total
		report.Kept = kept
		report.Changed = len(edits)
		if len(edits) > MaxSequenceOrderRepairs {
			report.Truncated = true
			report.Plans = make([]*RepairPlan, MaxSequenceOrderRepairs)
			for i := 0; i < MaxSequenceOrderRepairs; i++ {
				report.Plans[i] = edits[i].plan
			}
			if req.Act {
				return fmt.Errorf("%w: %d rows would change", ErrSequenceOrderBound, len(edits))
			}
			return nil
		}
		if err := bindSequenceOrderFingerprints(edits, req.Fingerprints, req.Act); err != nil {
			return err
		}
		report.Plans = make([]*RepairPlan, len(edits))
		for i := range edits {
			report.Plans[i] = edits[i].plan
		}
		if !req.Act || len(edits) == 0 {
			return nil
		}
		var floor int64
		for _, line := range lines {
			if line == "" {
				continue
			}
			var env Envelope
			if json.Unmarshal([]byte(line), &env) == nil && env.Sequence > floor {
				floor = env.Sequence
			}
		}
		for _, edit := range edits {
			if edit.env.Sequence > floor {
				floor = edit.env.Sequence
			}
		}
		if err := m.setSequenceFloorLocked(floor); err != nil {
			return err
		}
		for _, edit := range edits {
			edit.plan.Phase = RepairPhasePrepare
			edit.plan.PreparedAt = time.Now().UTC()
			if err := m.appendRepairRecord(edit.plan); err != nil {
				return fmt.Errorf("mail repair: durable prepare record: %w", err)
			}
		}
		expected := make([]byte, 0, len(data))
		cursor := 0
		replacements := make(map[int]string, len(edits))
		want := make([]*Envelope, 0, len(edits))
		for _, edit := range edits {
			replacements[edit.index] = edit.plan.RepairedLine
			want = append(want, edit.env)
		}
		for i, span := range spans {
			if replacement, ok := replacements[i]; ok {
				expected = append(expected, data[cursor:span.start]...)
				expected = append(expected, replacement...)
				cursor = span.end
			}
		}
		expected = append(expected, data[cursor:]...)
		if err := writeFileAtomic(m.MailFile, expected, 0644); err != nil {
			plans := make([]*RepairPlan, len(edits))
			for i, edit := range edits {
				plans[i] = edit.plan
			}
			return m.recordRepairBatchFailure(plans, fmt.Errorf("mail repair: durable mailbox write: %w", err))
		}
		for _, env := range want {
			if err := m.verifyRepairedMailbox(expected, env); err != nil {
				plans := make([]*RepairPlan, len(edits))
				for i, edit := range edits {
					plans[i] = edit.plan
				}
				return m.recordRepairBatchFailure(plans, err)
			}
		}
		for _, edit := range edits {
			edit.plan.Phase = RepairPhaseResult
			edit.plan.Outcome = RepairOutcomeApplied
			edit.plan.Applied = true
			edit.plan.CompletedAt = time.Now().UTC()
			if err := m.appendRepairRecord(edit.plan); err != nil {
				return fmt.Errorf("%w: %v", ErrRepairCompletionUnrecorded, err)
			}
		}
		return nil
	})
	if err != nil {
		return report, err
	}
	return report, nil
}

func bindSequenceOrderFingerprints(edits []sequenceEdit, fingerprints []string, act bool) error {
	if act && len(edits) > 0 && len(fingerprints) == 0 {
		return ErrRepairFingerprintRequired
	}
	if len(fingerprints) == 0 {
		return nil
	}
	if len(fingerprints) != len(edits) {
		return errors.New("mail repair: provide one --fingerprint per changed row, in file order")
	}
	for i, edit := range edits {
		fp := strings.TrimSpace(fingerprints[i])
		if fp == "" {
			return errors.New("mail repair: --fingerprint is required to be nonblank when supplied")
		}
		if !strings.EqualFold(fp, edit.plan.OriginalSHA256) {
			return fmt.Errorf("%w: id %q has %s", ErrRepairStale, edit.id, edit.plan.OriginalSHA256)
		}
	}
	return nil
}

func planSequenceOrderEdits(lines []string, actor, reason string) ([]sequenceEdit, int, int, error) {
	seenID := map[string]int{}
	var edits []sequenceEdit
	var maxSeen int64
	saw := false
	kept := 0
	total := 0
	for i, line := range lines {
		if line == "" {
			continue
		}
		total++
		scan, isObject := scanJSONObject([]byte(line))
		if !isObject {
			return nil, 0, 0, fmt.Errorf("%w: line %d is not a complete JSON object", ErrRepairUnsupported, i+1)
		}
		if key, dup := duplicateKey(scan.Keys); dup {
			return nil, 0, 0, fmt.Errorf("%w: line %d repeats top-level key %q", ErrRepairDuplicateKeys, i+1, key)
		}
		if len(scan.IDs) != 1 || strings.TrimSpace(scan.IDs[0]) == "" {
			return nil, 0, 0, fmt.Errorf("%w: line %d has no readable unique identity", ErrRepairUnsupported, i+1)
		}
		id := scan.IDs[0]
		if prev, ok := seenID[id]; ok {
			return nil, 0, 0, fmt.Errorf("%w: id %q at line %d and line %d", ErrRepairAmbiguous, id, prev+1, i+1)
		}
		seenID[id] = i
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, 0, 0, fmt.Errorf("%w: line %d is not a well-formed JSON object: %v", ErrRepairUnsupported, i+1, err)
		}
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return nil, 0, 0, fmt.Errorf("%w: line %d does not decode as an envelope: %v", ErrRepairUnsupported, i+1, err)
		}
		if env.Sequence <= 0 {
			return nil, 0, 0, fmt.Errorf("%w: record %q carries sequence %d", ErrStorageUnordered, env.ID, env.Sequence)
		}
		inverts := saw && env.Sequence <= maxSeen
		if !inverts {
			if env.Sequence > maxSeen {
				maxSeen = env.Sequence
			}
			saw = true
			kept++
			continue
		}
		for _, key := range privilegedJSONKeys {
			if _, ok := raw[key]; ok {
				return nil, 0, 0, fmt.Errorf("%w: row %q carries %q", ErrRepairPrivileged, id, key)
			}
		}
		for key := range raw {
			if !envelopeJSONKeys[key] {
				return nil, 0, 0, fmt.Errorf("%w: row %q carries unknown field %q that a re-encode would drop", ErrRepairUnsupported, id, key)
			}
		}
		next, err := nextSequenceValue(maxSeen)
		if err != nil {
			return nil, 0, 0, err
		}
		repaired, err := rewriteSequenceLine(line, next)
		if err != nil {
			return nil, 0, 0, err
		}
		copied := env
		copied.Sequence = next
		maxSeen = next
		saw = true
		edits = append(edits, sequenceEdit{
			index: i,
			id:    id,
			env:   &copied,
			plan: &RepairPlan{
				ID:               id,
				OriginalLine:     line,
				OriginalSHA256:   sha256OfLine(line),
				RepairedLine:     repaired,
				RepairedSHA256:   sha256OfLine(repaired),
				AssignedSequence: next,
				Actor:            actor,
				Reason:           reason,
			},
		})
	}
	return edits, kept, total, nil
}

func rewriteSequenceLine(line string, seq int64) (string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return "", fmt.Errorf("%w: re-encode: %v", ErrRepairUnsupported, err)
	}
	encoded, err := json.Marshal(seq)
	if err != nil {
		return "", fmt.Errorf("mail repair: encode sequence: %w", err)
	}
	raw["seq"] = encoded
	rebuilt, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("%w: re-encode: %v", ErrRepairUnsupported, err)
	}
	return string(rebuilt), nil
}

func (m *Mailbox) checkSequenceOrderCursor(req SequenceOrderRequest, lines []string) error {
	raw := strings.TrimSpace(req.Cursor)
	if raw == "" {
		return nil
	}
	recipient := strings.TrimSpace(req.Recipient)
	if recipient == "" {
		return errors.New("mail repair: --after-cursor requires --recipient")
	}
	source, err := SourceFingerprint(m.MailFile, req.FeedbackDir)
	if err != nil {
		return err
	}
	cur, err := ParseCursor(raw, recipient, source)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSequenceOrderStaleCursor, err)
	}
	if cur.Control == 0 {
		return nil
	}
	watermark := EmptyAnchor
	var maxSeen int64
	saw := false
	matched := false
	for _, line := range lines {
		if line == "" {
			continue
		}
		var env Envelope
		if json.Unmarshal([]byte(line), &env) != nil {
			continue
		}
		if env.Recipient != recipient && env.Recipient != "all" {
			continue
		}
		if saw && env.Sequence <= maxSeen {
			return fmt.Errorf("%w: store is unordered under recipient %q", ErrSequenceOrderStaleCursor, recipient)
		}
		if env.Sequence > maxSeen {
			maxSeen = env.Sequence
		}
		saw = true
		watermark = FoldWatermark(watermark, ControlWatermarkFields(&env)...)
		if env.Sequence == cur.Control {
			if cur.ControlAnchor != watermark && cur.ControlAnchor != "" && cur.ControlAnchor != EmptyAnchor {
				return fmt.Errorf("%w: control prefix at sequence %d changed", ErrSequenceOrderStaleCursor, cur.Control)
			}
			matched = true
		}
		if env.Sequence > cur.Control && !matched {
			return fmt.Errorf("%w: cursor sequence %d is not on the current prefix", ErrSequenceOrderStaleCursor, cur.Control)
		}
	}
	if !matched {
		return fmt.Errorf("%w: cursor is ahead of the store", ErrSequenceOrderStaleCursor)
	}
	return nil
}
