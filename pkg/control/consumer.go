package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/mail"
)

var ErrProcessorUnavailable = fmt.Errorf("control: idempotency-aware processor is required")

type ProcessorState struct {
	Key       string `json:"key"`
	State     string `json:"state"`
	Result    string `json:"result,omitempty"`
	MessageID string `json:"message_id"`
	Sequence  int64  `json:"sequence"`
}

type ProcessorStateStore interface {
	Load(context.Context, string) (ProcessorState, error)
	Save(context.Context, ProcessorState) error
}

// IdempotentProcessor is the side-effect boundary. Apply must durably dedupe
// by key in its reconstructed implementation; Consumer never claims that an
// arbitrary callback is exactly-once.
type IdempotentProcessor interface {
	Apply(context.Context, Order, string) (string, error)
}

// MailboxProcessorStore persists processing and applied state as stable,
// content-addressed mailbox records, so a reconstructed consumer can recover
// the key/result without an in-memory map.
type MailboxProcessorStore struct {
	Mailbox     *mail.Mailbox
	Coordinator string
	Sender      string
}

func (s MailboxProcessorStore) Load(ctx context.Context, key string) (ProcessorState, error) {
	if s.Mailbox == nil {
		return ProcessorState{}, ErrProcessorUnavailable
	}
	recipient := s.Coordinator
	if recipient == "" {
		recipient = mail.CoordinatorInbox
	}
	envs, err := s.Mailbox.ReadInboxContext(ctx, recipient)
	if err != nil {
		return ProcessorState{}, err
	}
	var found ProcessorState
	for _, env := range envs {
		if env.Subject != "control/processor_state" || !strings.HasPrefix(env.ID, "processor-"+shortID(key)+"-") {
			continue
		}
		var state ProcessorState
		if err := json.Unmarshal([]byte(env.Body), &state); err != nil {
			return ProcessorState{}, fmt.Errorf("control: corrupt processor state: %w", err)
		}
		if state.Key != key || state.State == "" || env.ID != "processor-"+shortID(key)+"-"+state.State || state.MessageID == "" || state.Sequence <= 0 || env.Sender != s.Sender || env.Recipient != recipient || env.Sequence <= 0 {
			return ProcessorState{}, fmt.Errorf("control: processor state identity mismatch")
		}
		if found.State == "" || state.State == "applied" {
			found = state
		}
	}
	return found, nil
}

func (s MailboxProcessorStore) Save(ctx context.Context, state ProcessorState) error {
	if s.Mailbox == nil || state.Key == "" || state.State == "" || state.MessageID == "" || state.Sequence <= 0 {
		return fmt.Errorf("control: invalid processor state")
	}
	recipient := s.Coordinator
	if recipient == "" {
		recipient = mail.CoordinatorInbox
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if strings.TrimSpace(s.Sender) == "" {
		return ErrProcessorUnavailable
	}
	return s.Mailbox.SendEnvelopeContext(ctx, &mail.Envelope{ID: "processor-" + shortID(state.Key) + "-" + state.State, Sender: s.Sender, Recipient: recipient, Subject: "control/processor_state", Body: string(b)})
}

type Consumer struct {
	Mailbox     *mail.Mailbox
	Identity    LaneIdentity
	Coordinator string
	Processor   IdempotentProcessor
	State       ProcessorStateStore
}

func (c *Consumer) Consume(ctx context.Context) error {
	if c == nil || c.Mailbox == nil || c.Processor == nil || c.State == nil {
		return ErrProcessorUnavailable
	}
	if c.Coordinator == "" {
		c.Coordinator = mail.CoordinatorInbox
	}
	envs, err := c.Mailbox.ReadInboxContext(ctx, c.Identity.Lane)
	if err != nil {
		return err
	}
	for _, env := range envs {
		if !strings.HasPrefix(env.Subject, "control/") || env.Subject == "control/ack" || env.Subject == "control/supersede" {
			continue
		}
		var order Order
		if err := json.Unmarshal([]byte(env.Body), &order); err != nil {
			return fmt.Errorf("control: corrupt order %s: %w", env.ID, err)
		}
		if order.LaneIdentity != c.Identity {
			return ErrStaleIdentity
		}
		key, digest, _, err := identityKey(order)
		if err != nil {
			return err
		}
		if order.BodyDigest != digest {
			return fmt.Errorf("control: body digest mismatch")
		}
		if env.Sender != c.Coordinator || env.Recipient != c.Identity.Lane || env.ID != messageID(key) || env.Subject != "control/"+string(order.Kind) || env.Sequence <= 0 {
			return fmt.Errorf("control: control envelope identity mismatch")
		}
		terminal, err := c.findEvidence(ctx, key, env, order)
		if err != nil {
			return err
		}
		if terminal {
			continue
		}
		state, err := c.State.Load(ctx, key)
		if err != nil {
			return err
		}
		if state.State != "" && (state.MessageID != env.ID || state.Sequence != env.Sequence) {
			return fmt.Errorf("control: processor state does not match current envelope")
		}
		if state.State == "applied" {
			if err := c.emit(ctx, "control/ack", "ack-"+shortID(key), key, env, order); err != nil {
				return err
			}
			continue
		}
		if state.State == "" {
			state = ProcessorState{Key: key, State: "processing", MessageID: env.ID, Sequence: env.Sequence}
			if err := c.State.Save(ctx, state); err != nil {
				return err
			}
		} else if state.State != "processing" {
			return fmt.Errorf("control: unknown processor state %q", state.State)
		}
		result, err := c.Processor.Apply(ctx, order, key)
		if err != nil {
			return fmt.Errorf("control: processor failed for %s: %w", key, err)
		}
		if err := c.State.Save(ctx, ProcessorState{Key: key, State: "applied", Result: result, MessageID: env.ID, Sequence: env.Sequence}); err != nil {
			return err
		}
		if err := c.emit(ctx, "control/ack", "ack-"+shortID(key), key, env, order); err != nil {
			return err
		}
	}
	return nil
}

func shortID(key string) string { h := sha256.Sum256([]byte(key)); return hex.EncodeToString(h[:]) }

func (c *Consumer) findEvidence(ctx context.Context, key string, orderEnv *mail.Envelope, o Order) (bool, error) {
	envs, err := c.Mailbox.ReadInboxContext(ctx, c.Coordinator)
	if err != nil {
		return false, err
	}
	for _, env := range envs {
		if env.Subject != "control/ack" && env.Subject != "control/supersede" {
			continue
		}
		var e AckEvidence
		if err := json.Unmarshal([]byte(env.Body), &e); err != nil {
			return false, fmt.Errorf("control: corrupt terminal evidence: %w", err)
		}
		if e.IdempotencyKey != key {
			continue
		}
		want := "ack-" + shortID(key)
		if env.Subject == "control/supersede" {
			want = "supersede-" + shortID(key)
		}
		if env.Sender != o.Lane || env.Recipient != c.Coordinator || env.ID != want || env.Sequence <= 0 || e.MessageID != orderEnv.ID || e.Sequence != orderEnv.Sequence || e.Repository != o.Repository || e.TaskRef != o.TaskRef || e.Lane != o.Lane || e.LeaseGeneration != o.LeaseGeneration || e.CandidateSHA != o.CandidateSHA || e.Kind != o.Kind || e.BodyDigest != o.BodyDigest {
			return false, fmt.Errorf("control: terminal evidence identity mismatch")
		}
		return true, nil
	}
	return false, nil
}

func (c *Consumer) Supersede(ctx context.Context, order Order, messageID string, sequence int64, reason string, retryable bool) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("control: supersession reason is required")
	}
	key, digest, _, err := identityKey(order)
	if err != nil {
		return err
	}
	if order.BodyDigest == "" {
		order.BodyDigest = digest
	}
	e := AckEvidence{IdempotencyKey: key, MessageID: messageID, Sequence: sequence, Repository: order.Repository, TaskRef: order.TaskRef, Lane: order.Lane, LeaseGeneration: order.LeaseGeneration, CandidateSHA: order.CandidateSHA, Kind: order.Kind, BodyDigest: order.BodyDigest, Outcome: "superseded", FailureReason: reason, Retryable: retryable, EnvelopeID: "supersede-" + shortID(key)}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.Mailbox.SendEnvelopeContext(ctx, &mail.Envelope{ID: e.EnvelopeID, Sender: c.Identity.Lane, Recipient: c.Coordinator, Subject: "control/supersede", Body: string(b)})
}

func (c *Consumer) emit(ctx context.Context, subject, id, key string, env *mail.Envelope, o Order) error {
	e := AckEvidence{IdempotencyKey: key, MessageID: env.ID, Sequence: env.Sequence, Repository: o.Repository, TaskRef: o.TaskRef, Lane: o.Lane, LeaseGeneration: o.LeaseGeneration, CandidateSHA: o.CandidateSHA, Kind: o.Kind, BodyDigest: o.BodyDigest, EnvelopeID: id, Outcome: "acknowledged"}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.Mailbox.SendEnvelopeContext(ctx, &mail.Envelope{ID: id, Sender: c.Identity.Lane, Recipient: c.Coordinator, Subject: subject, Body: string(b)})
}
