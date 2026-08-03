package control

// Consumer is the recipient-side boundary for coordinator control mail. It
// makes the durable envelope, rather than Herdr, the source of work and emits
// structured evidence back to the coordinator. Re-running Consume after a
// restart observes the existing evidence and does not process the order again.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/mail"
)

var ErrProcessingUnresolved = errors.New("control: durable processing state requires reconciliation")

type Consumer struct {
	Mailbox     *mail.Mailbox
	Identity    LaneIdentity
	Coordinator string
	Process     func(context.Context, Order) error
}

func (c *Consumer) Consume(ctx context.Context) error {
	if c == nil || c.Mailbox == nil || c.Process == nil {
		return fmt.Errorf("control: consumer mailbox and processor are required")
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
		terminal, err := c.findEvidence(ctx, key, env, order)
		if err != nil {
			return err
		}
		if terminal {
			continue
		}
		if c.processingSeen(ctx, key, env) {
			return ErrProcessingUnresolved
		}
		if err := c.markProcessing(ctx, key, env); err != nil {
			return err
		}
		if err := c.Process(ctx, order); err != nil {
			return fmt.Errorf("control: processor failed for %s: %w", key, err)
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
		if e.IdempotencyKey == key {
			if e.MessageID != orderEnv.ID || e.Sequence != orderEnv.Sequence || e.Repository != o.Repository || e.TaskRef != o.TaskRef || e.Lane != o.Lane || e.LeaseGeneration != o.LeaseGeneration || e.CandidateSHA != o.CandidateSHA || e.Kind != o.Kind || e.BodyDigest != o.BodyDigest {
				return false, fmt.Errorf("control: terminal evidence identity mismatch")
			}
			return true, nil
		}
	}
	return false, nil
}

func (c *Consumer) processingSeen(ctx context.Context, key string, orderEnv *mail.Envelope) bool {
	envs, err := c.Mailbox.ReadInboxContext(ctx, c.Coordinator)
	if err != nil {
		return true
	}
	for _, env := range envs {
		if env.Subject == "control/processing" && env.ID == "processing-"+shortID(key) && env.Body == orderEnv.ID {
			return true
		}
	}
	return false
}

func (c *Consumer) markProcessing(ctx context.Context, key string, env *mail.Envelope) error {
	return c.Mailbox.SendEnvelopeContext(ctx, &mail.Envelope{ID: "processing-" + shortID(key), Sender: c.Identity.Lane, Recipient: c.Coordinator, Subject: "control/processing", Body: env.ID})
}

// Supersede is explicit and preserves why the order was not applied.
func (c *Consumer) Supersede(ctx context.Context, order Order, messageID string, sequence int64, reason string, retryable bool) error {
	key, digest, _, err := identityKey(order)
	if err != nil {
		return err
	}
	if order.BodyDigest == "" {
		order.BodyDigest = digest
	}
	e := AckEvidence{IdempotencyKey: key, MessageID: messageID, Sequence: sequence, Repository: order.Repository, TaskRef: order.TaskRef, Lane: order.Lane, LeaseGeneration: order.LeaseGeneration, CandidateSHA: order.CandidateSHA, Kind: order.Kind, BodyDigest: order.BodyDigest, Outcome: "superseded", FailureReason: reason, Retryable: retryable}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.Mailbox.SendEnvelopeContext(ctx, &mail.Envelope{ID: "supersede-" + shortID(key), Sender: c.Identity.Lane, Recipient: c.Coordinator, Subject: "control/supersede", Body: string(b)})
}

func (c *Consumer) emit(ctx context.Context, subject, id, key string, env *mail.Envelope, o Order) error {
	e := AckEvidence{IdempotencyKey: key, MessageID: env.ID, Sequence: env.Sequence, Repository: o.Repository, TaskRef: o.TaskRef, Lane: o.Lane, LeaseGeneration: o.LeaseGeneration, CandidateSHA: o.CandidateSHA, Kind: o.Kind, BodyDigest: o.BodyDigest}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.Mailbox.SendEnvelopeContext(ctx, &mail.Envelope{ID: id, Sender: c.Identity.Lane, Recipient: c.Coordinator, Subject: subject, Body: string(b)})
}
