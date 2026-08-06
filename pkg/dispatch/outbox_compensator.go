package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Kampe/Herdforge/pkg/outbox"
)

// OutboxCompensator is the production Compensator: it persists every launch
// side-effect into the durable FAC-119 outbox so a crashed dispatch can be
// replayed or compensated.
//
// The Compensator interface and its fail-closed gate already existed, but the
// only implementations were test doubles — so every production dispatch was
// rejected with "dispatch compensator is required". This adapts the existing
// outbox.Store rather than introducing a second persistence mechanism.
type OutboxCompensator struct {
	Store *outbox.Store
}

// NewOutboxCompensator opens (or creates) the durable outbox at path.
func NewOutboxCompensator(path string) (*OutboxCompensator, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("outbox compensator: path is required")
	}
	store, err := outbox.NewStore(path)
	if err != nil {
		return nil, fmt.Errorf("outbox compensator: open %s: %w", path, err)
	}
	return &OutboxCompensator{Store: store}, nil
}

func (c *OutboxCompensator) Close() error {
	if c == nil || c.Store == nil {
		return nil
	}
	return c.Store.Close()
}

// stepKey is the idempotency key for one (ticket, step) side-effect.
//
// It must cover EVERY field carried in the payload. The outbox rejects a reused
// key whose payload differs, so any payload field left out of the key turns a
// legitimate retry into ErrIdempotencyConflict — and because the key is
// deterministic per ticket, that wedges the ticket permanently until someone
// hand-edits the SQLite file. The first version keyed on only four fields while
// marshalling the whole record, so a re-dispatch after main advanced (new
// BaseSHA, same branch) was unrecoverable.
//
// Encoding is fixed-arity `field=value` with an escaped separator: positional
// concatenation lost field identity, so {TabID:"x"} and {Branch:"x"} produced
// the same key, and pane/tab ids legitimately contain ':'.
func stepKey(rec StepRecord) string {
	esc := func(v string) string {
		v = strings.ReplaceAll(v, `\`, `\\`)
		return strings.ReplaceAll(v, "|", `\|`)
	}
	fields := []struct{ name, value string }{
		{"ticket", rec.TicketRef},
		{"step", string(rec.Step)},
		{"worktree", rec.Worktree},
		{"branch", rec.Branch},
		{"base", rec.BaseSHA},
		{"anchor", rec.AnchorRef},
		{"tab", rec.TabID},
		{"pane", rec.PaneID},
		{"agent", rec.AgentName},
		{"receipt", rec.Receipt},
		{"message", rec.MessageID},
		{"seq", strconv.FormatInt(rec.Sequence, 10)},
	}
	parts := make([]string, 0, len(fields)+1)
	parts = append(parts, "dispatch")
	for _, f := range fields {
		parts = append(parts, f.name+"="+esc(f.value))
	}
	return strings.Join(parts, "|")
}

// RecordStep durably persists one successful launch side-effect.
func (c *OutboxCompensator) RecordStep(_ context.Context, rec StepRecord) error {
	if c == nil || c.Store == nil {
		return fmt.Errorf("outbox compensator: store is not open")
	}
	if strings.TrimSpace(rec.TicketRef) == "" {
		return fmt.Errorf("outbox compensator: RecordStep requires a ticket ref")
	}
	if strings.TrimSpace(string(rec.Step)) == "" {
		return fmt.Errorf("outbox compensator: RecordStep requires a step")
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("outbox compensator: marshal step %s: %w", rec.Step, err)
	}
	if _, err := c.Store.Enqueue(outbox.Item{
		IdempotencyKey: stepKey(rec),
		TaskRef:        rec.TicketRef,
		Kind:           "dispatch.step." + string(rec.Step),
		Payload:        string(payload),
		MessageID:      rec.MessageID,
		Sequence:       rec.Sequence,
	}); err != nil {
		return fmt.Errorf("outbox compensator: RecordStep %s/%s: %w", rec.TicketRef, rec.Step, err)
	}
	return nil
}

// Compensate durably records that a dispatch left partial state behind, so the
// relay can undo it or a coordinator can see the ticket is Recovering.
func (c *OutboxCompensator) Compensate(_ context.Context, ticketRef, reason string) error {
	if c == nil || c.Store == nil {
		return fmt.Errorf("outbox compensator: store is not open")
	}
	if strings.TrimSpace(ticketRef) == "" {
		return fmt.Errorf("outbox compensator: Compensate requires a ticket ref")
	}
	payload, err := json.Marshal(map[string]string{"ticket_ref": ticketRef, "reason": reason})
	if err != nil {
		return fmt.Errorf("outbox compensator: marshal compensation: %w", err)
	}
	// Keyed on the reason so distinct failures each get a durable record while
	// a retry of the same failure stays idempotent.
	if _, err := c.Store.Enqueue(outbox.Item{
		IdempotencyKey: strings.Join([]string{"dispatch", ticketRef, "compensate", reason}, ":"),
		TaskRef:        ticketRef,
		Kind:           "dispatch.compensate",
		Payload:        string(payload),
	}); err != nil {
		return fmt.Errorf("outbox compensator: Compensate %s: %w", ticketRef, err)
	}
	return nil
}
