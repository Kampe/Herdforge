package mail

import (
	"bytes"
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
// Operator review uses a compact plan file (ids, line indexes, per-row
// original digests, assigned sequences) plus a whole-store digest. Acting
// is compare-and-swap on those digests. Full original bodies are not copied
// into the plan, so a 517-inversion store stays within bounded memory.

const (
	SequenceOrderPlanSchema = "mail.sequence-order.v1"
	// MaxSequenceOrderRows / MaxSequenceOrderBytes are finite configured
	// ceilings. They are large enough for the observed 4447-row / 517-inversion
	// control mailbox and refuse anything bigger instead of scanning forever.
	MaxSequenceOrderRows  = MaxBoundedLimit
	MaxSequenceOrderBytes = MaxBoundedPageBytes
)

var (
	ErrSequenceOrderBound       = errors.New("mail repair: sequence-order store exceeds configured row or byte bound")
	ErrSequenceOrderStaleCursor = errors.New("mail repair: paging cursor is stale for this store")
	ErrSequenceOrderStaleStore  = errors.New("mail repair: store digest does not match the reviewed plan")
	ErrSequenceOrderPlanDigest  = errors.New("mail repair: plan-file digest does not match the reviewed bytes")
)

// SequenceOrderRequest is one whole-store sequence-order recovery.
type SequenceOrderRequest struct {
	Act         bool
	Actor       string
	Reason      string
	Plan        []byte
	PlanDigest  string
	MaxRows     int
	MaxBytes    int
	Cursor      string
	Recipient   string
	FeedbackDir string
}

// SequenceOrderEdit is one inverting row in the compact plan. It never carries
// original_line: the live row plus original_sha256 is the CAS.
type SequenceOrderEdit struct {
	Index            int    `json:"index"`
	ID               string `json:"id"`
	OriginalSHA256   string `json:"original_sha256"`
	AssignedSequence int64  `json:"assigned_sequence"`
}

// SequenceOrderPlan is the durable reviewed artifact. json.Marshal of this
// struct is what --plan-digest hashes.
type SequenceOrderPlan struct {
	Schema      string              `json:"schema"`
	StoreSHA256 string              `json:"store_sha256"`
	TotalRows   int                 `json:"total_rows"`
	Changed     int                 `json:"changed"`
	Kept        int                 `json:"kept"`
	MaxRows     int                 `json:"max_rows"`
	MaxBytes    int                 `json:"max_bytes"`
	Edits       []SequenceOrderEdit `json:"edits"`
}

func (p SequenceOrderPlan) MarshalPlan() ([]byte, error) {
	if p.Edits == nil {
		p.Edits = []SequenceOrderEdit{}
	}
	return json.Marshal(p)
}

func DecodeSequenceOrderPlan(raw []byte) (SequenceOrderPlan, error) {
	var plan SequenceOrderPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return SequenceOrderPlan{}, fmt.Errorf("mail repair: plan file is not JSON: %w", err)
	}
	if plan.Schema != SequenceOrderPlanSchema {
		return SequenceOrderPlan{}, fmt.Errorf("mail repair: plan schema %q is not %s", plan.Schema, SequenceOrderPlanSchema)
	}
	if strings.TrimSpace(plan.StoreSHA256) == "" {
		return SequenceOrderPlan{}, errors.New("mail repair: plan is missing store_sha256")
	}
	return plan, nil
}

func SequenceOrderPlanDigest(raw []byte) string {
	return sha256OfLine(string(raw))
}

type sequenceEdit struct {
	index          int
	id             string
	assigned       int64
	originalSHA256 string
}

// RepairSequenceOrder restores file-order monotonic sequences without
// reordering or dropping rows. Duplicate ids, privileged signed rows that
// would be rewritten, stale plan/store digests, stale paging cursors, and
// stores over the configured row/byte bound all refuse with the mailbox
// untouched.
func (m *Mailbox) RepairSequenceOrder(ctx context.Context, req SequenceOrderRequest) (*SequenceOrderPlan, error) {
	if m == nil {
		return nil, errors.New("mail repair: nil mailbox")
	}
	if req.Act && strings.TrimSpace(req.Actor) == "" {
		return nil, ErrRepairActorRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	maxRows := req.MaxRows
	if maxRows <= 0 {
		maxRows = MaxSequenceOrderRows
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxSequenceOrderBytes
	}
	var out *SequenceOrderPlan
	err := m.withFileLockContext(ctx, func() error {
		data, err := os.ReadFile(m.MailFile)
		if err != nil {
			return fmt.Errorf("mail repair: read mailbox: %w", redactErr(err))
		}
		if len(data) > maxBytes {
			return fmt.Errorf("%w: store is %d bytes (max %d)", ErrSequenceOrderBound, len(data), maxBytes)
		}
		storeSHA := sha256OfLine(string(data))
		spans := lineSpans(data)
		lines := make([]string, len(spans))
		for i, span := range spans {
			lines[i] = string(data[span.start:span.end])
		}
		if err := m.checkSequenceOrderCursor(req, lines); err != nil {
			return err
		}
		edits, kept, total, err := planSequenceOrderEdits(lines, maxRows)
		if err != nil {
			return err
		}
		plan := SequenceOrderPlan{
			Schema: SequenceOrderPlanSchema, StoreSHA256: storeSHA,
			TotalRows: total, Changed: len(edits), Kept: kept,
			MaxRows: maxRows, MaxBytes: maxBytes, Edits: compactSequenceEdits(edits),
		}
		out = &plan
		if !req.Act || len(edits) == 0 {
			return nil
		}
		if len(req.Plan) == 0 || strings.TrimSpace(req.PlanDigest) == "" {
			return ErrSequenceOrderPlanDigest
		}
		if !strings.EqualFold(strings.TrimSpace(req.PlanDigest), SequenceOrderPlanDigest(req.Plan)) {
			return fmt.Errorf("%w: computed %s", ErrSequenceOrderPlanDigest, SequenceOrderPlanDigest(req.Plan))
		}
		reviewed, err := DecodeSequenceOrderPlan(req.Plan)
		if err != nil {
			return err
		}
		if !strings.EqualFold(reviewed.StoreSHA256, storeSHA) {
			return fmt.Errorf("%w: live %s", ErrSequenceOrderStaleStore, storeSHA)
		}
		if err := bindSequenceOrderPlan(edits, reviewed); err != nil {
			return err
		}
		return m.applySequenceOrderLocked(data, spans, lines, edits, req)
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

func compactSequenceEdits(edits []sequenceEdit) []SequenceOrderEdit {
	out := make([]SequenceOrderEdit, len(edits))
	for i, e := range edits {
		out[i] = SequenceOrderEdit{Index: e.index, ID: e.id, OriginalSHA256: e.originalSHA256, AssignedSequence: e.assigned}
	}
	return out
}

func bindSequenceOrderPlan(live []sequenceEdit, reviewed SequenceOrderPlan) error {
	if reviewed.Changed != len(live) || len(reviewed.Edits) != len(live) {
		return fmt.Errorf("%w: plan changed=%d edits=%d live=%d", ErrSequenceOrderStaleStore, reviewed.Changed, len(reviewed.Edits), len(live))
	}
	for i, edit := range live {
		want := reviewed.Edits[i]
		if want.Index != edit.index || want.ID != edit.id || !strings.EqualFold(want.OriginalSHA256, edit.originalSHA256) || want.AssignedSequence != edit.assigned {
			return fmt.Errorf("%w: edit %d id %q does not match the reviewed plan", ErrSequenceOrderStaleStore, i, edit.id)
		}
	}
	return nil
}

func (m *Mailbox) applySequenceOrderLocked(data []byte, spans []lineSpan, lines []string, edits []sequenceEdit, req SequenceOrderRequest) error {
	if len(edits) == 0 {
		return nil
	}
	audit := map[string]any{
		"schema": SequenceOrderPlanSchema, "phase": RepairPhasePrepare,
		"actor": req.Actor, "reason": req.Reason,
		"store_sha256": sha256OfLine(string(data)), "changed": len(edits),
		"prepared_at": time.Now().UTC(),
	}
	if err := m.appendRepairJSON(audit); err != nil {
		return fmt.Errorf("mail repair: durable prepare record: %w", err)
	}
	replacements := make(map[int]string, len(edits))
	want := make([]*Envelope, 0, len(edits))
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
		repaired, err := rewriteSequenceLine(lines[edit.index], edit.assigned)
		if err != nil {
			return err
		}
		var env Envelope
		if err := json.Unmarshal([]byte(repaired), &env); err != nil {
			return fmt.Errorf("mail repair: repaired row %q does not parse: %w", edit.id, err)
		}
		replacements[edit.index] = repaired
		want = append(want, &env)
		if edit.assigned > floor {
			floor = edit.assigned
		}
	}
	if err := m.setSequenceFloorLocked(floor); err != nil {
		return err
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
		return fmt.Errorf("mail repair: durable mailbox write: %w", redactErr(err))
	}
	got, err := os.ReadFile(m.MailFile)
	if err != nil {
		return fmt.Errorf("%w: reread: %v", ErrRepairReadbackFailed, redactErr(err))
	}
	if !bytes.Equal(got, expected) {
		return fmt.Errorf("%w: durable mailbox is not the exact bytes this repair wrote", ErrRepairReadbackFailed)
	}
	for _, env := range want {
		line, ok := findLineByID(expected, env.ID)
		if !ok {
			return fmt.Errorf("%w: repaired row %q is not present", ErrRepairReadbackFailed, env.ID)
		}
		var durable Envelope
		if err := json.Unmarshal(line, &durable); err != nil {
			return fmt.Errorf("%w: repaired row does not parse: %v", ErrRepairReadbackFailed, err)
		}
		if !sameEnvelope(&durable, env) {
			return repairReadbackRowMismatch()
		}
	}
	result := map[string]any{
		"schema": SequenceOrderPlanSchema, "phase": RepairPhaseResult,
		"outcome": RepairOutcomeApplied, "applied": true,
		"actor": req.Actor, "changed": len(edits),
		"completed_at": time.Now().UTC(),
	}
	if err := m.appendRepairJSON(result); err != nil {
		return fmt.Errorf("%w: %v", ErrRepairCompletionUnrecorded, err)
	}
	return nil
}

func (m *Mailbox) appendRepairJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}
	return appendLine(m.RepairAuditPath(), data)
}

func planSequenceOrderEdits(lines []string, maxRows int) ([]sequenceEdit, int, int, error) {
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
		if total > maxRows {
			return nil, 0, 0, fmt.Errorf("%w: store has more than %d rows", ErrSequenceOrderBound, maxRows)
		}
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
		maxSeen = next
		saw = true
		edits = append(edits, sequenceEdit{
			index: i, id: id, assigned: next, originalSHA256: sha256OfLine(line),
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
		return ErrRepairCursorNeedsRecipient
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
