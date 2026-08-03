// Package control separates durable coordinator orders from Herdr wakeups.
// The mailbox is the durable recipient-facing envelope; Herdr is only a
// retryable nudge that asks a lane to consume that envelope.
package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/outbox"
)

type Kind string

const (
	KindRebase           Kind = "rebase"
	KindRepair           Kind = "repair"
	KindReviewCorrection Kind = "review_correction"
	KindCallback         Kind = "callback"
)

type LaneIdentity struct {
	Repository      string `json:"repository"`
	TaskRef         string `json:"task_ref"`
	Lane            string `json:"lane"`
	LeaseGeneration int64  `json:"lease_generation"`
	CandidateSHA    string `json:"candidate_sha"`
}

type Order struct {
	LaneIdentity
	Kind       Kind   `json:"kind"`
	Body       string `json:"body"`
	BodyDigest string `json:"body_digest"`
}

type WakeRequest struct {
	Order     Order
	MessageID string
	Sequence  int64
}

type WakeReceipt struct {
	MessageID string
	Consumed  bool
}

// Sender is implemented by both mail.Mailbox and mail.MessageBroker through
// the small adapters below.  Keeping this interface here makes file-only and
// Redis-backed conformance tests exercise identical delivery semantics.
type Sender interface {
	SendEnvelopeContext(context.Context, *mail.Envelope) error
}

type Waker interface {
	Wake(context.Context, WakeRequest) (WakeReceipt, error)
}

// EvidenceReader reads the durable lane receipt. A Herdr wake receipt is not
// an acknowledgement and can never satisfy this interface by itself.
type EvidenceReader interface {
	ReadEvidence(context.Context, string, bool) (AckEvidence, error)
}

type AckEvidence struct {
	IdempotencyKey  string `json:"idempotency_key"`
	MessageID       string `json:"message_id"`
	Sequence        int64  `json:"sequence"`
	Repository      string `json:"repository"`
	TaskRef         string `json:"task_ref"`
	Lane            string `json:"lane"`
	LeaseGeneration int64  `json:"lease_generation"`
	CandidateSHA    string `json:"candidate_sha"`
	Kind            Kind   `json:"kind"`
	BodyDigest      string `json:"body_digest"`
}

type IdentityAuthority interface {
	Resolve(context.Context, Order) (LaneIdentity, error)
}

type Evidence struct {
	ItemID         int64         `json:"item_id"`
	MessageID      string        `json:"message_id"`
	Sequence       int64         `json:"sequence"`
	IdempotencyKey string        `json:"idempotency_key"`
	State          outbox.Status `json:"state"`
}

var (
	ErrStaleIdentity  = errors.New("control: stale lane identity")
	ErrMissingReceipt = errors.New("control: missing or corrupt wake receipt")
)

type Delivery struct {
	Outbox          *outbox.Store
	Sender          Sender
	Waker           Waker
	Authority       IdentityAuthority
	Owner           string
	ClaimStaleAfter time.Duration
	Evidence        EvidenceReader
}

func (d *Delivery) validate(ctx context.Context, o Order) error {
	if o.Repository == "" || o.TaskRef == "" || o.Lane == "" || o.CandidateSHA == "" || o.Kind == "" || o.Body == "" {
		return fmt.Errorf("control: repository, task ref, lane, candidate SHA, kind, and body are required")
	}
	if o.LeaseGeneration <= 0 {
		return fmt.Errorf("control: positive lease generation is required")
	}
	if d.Authority == nil {
		return fmt.Errorf("control: identity authority is required")
	}
	actual, err := d.Authority.Resolve(ctx, o)
	if err != nil {
		return fmt.Errorf("control: identity authority: %w", err)
	}
	if actual != o.LaneIdentity {
		return fmt.Errorf("%w: authoritative %+v got %+v", ErrStaleIdentity, actual, o.LaneIdentity)
	}
	digest := sha256.Sum256([]byte(o.Body))
	if o.BodyDigest != "" && o.BodyDigest != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("control: body digest does not match body")
	}
	return nil
}

func identityKey(o Order) (key, digest, payload string, err error) {
	d := sha256.Sum256([]byte(o.Body))
	digest = hex.EncodeToString(d[:])
	identity := struct {
		Repository      string `json:"repository"`
		TaskRef         string `json:"task_ref"`
		Lane            string `json:"lane"`
		LeaseGeneration int64  `json:"lease_generation"`
		CandidateSHA    string `json:"candidate_sha"`
		Kind            Kind   `json:"kind"`
	}{o.Repository, o.TaskRef, o.Lane, o.LeaseGeneration, o.CandidateSHA, o.Kind}
	body, err := json.Marshal(identity)
	if err != nil {
		return "", "", "", err
	}
	h := sha256.Sum256(body)
	key = "control:v1:" + hex.EncodeToString(h[:])
	stored := o
	if stored.BodyDigest == "" {
		stored.BodyDigest = digest
	}
	payloadBytes, err := json.Marshal(stored)
	if err != nil {
		return "", "", "", err
	}
	payload = string(payloadBytes)
	return key, digest, payload, nil
}

func messageID(key string) string { return "control-" + strings.TrimPrefix(key, "control:v1:") }

func (d *Delivery) Deliver(ctx context.Context, o Order) (Evidence, error) {
	if d == nil || d.Outbox == nil || d.Sender == nil || d.Waker == nil {
		return Evidence{}, fmt.Errorf("control: durable outbox, sender, and waker are required")
	}
	if d.Owner == "" {
		return Evidence{}, fmt.Errorf("control: unique owner token is required")
	}
	if err := d.validate(ctx, o); err != nil {
		return Evidence{}, err
	}
	key, digest, payload, err := identityKey(o)
	if err != nil {
		return Evidence{}, fmt.Errorf("control: identity: %w", err)
	}
	item, err := d.Outbox.Enqueue(outbox.Item{IdempotencyKey: key, TaskRef: o.TaskRef, Kind: "control/" + string(o.Kind), Payload: payload})
	if err != nil {
		return Evidence{}, err
	}
	e := Evidence{ItemID: item.ID, MessageID: messageID(key), IdempotencyKey: key, State: item.Status}
	if item.Status == outbox.StatusAcknowledged || item.Status == outbox.StatusSuperseded {
		return e, nil
	}
	if item.Status == outbox.StatusSent {
		if item.MessageID == "" || item.Sequence <= 0 {
			return e, ErrMissingReceipt
		}
		e.MessageID, e.Sequence = item.MessageID, item.Sequence
		return d.wake(ctx, o, e, digest)
	}
	owner := d.Owner
	stale := d.ClaimStaleAfter
	if stale <= 0 {
		stale = 5 * time.Minute
	}
	claimed, err := d.Outbox.ClaimOwned(item.ID, owner, stale, time.Now().UTC())
	if err != nil {
		return Evidence{}, err
	}
	env := &mail.Envelope{ID: e.MessageID, Sender: "coordinator", Recipient: o.Lane, Subject: "control/" + string(o.Kind), Body: payload, Timestamp: time.Now().UTC()}
	if err := d.Sender.SendEnvelopeContext(ctx, env); err != nil {
		return e, fmt.Errorf("control envelope not delivered: %w", err)
	}
	if env.Sequence <= 0 {
		return e, ErrMissingReceipt
	}
	if err := d.Outbox.RecordDelivery(claimed.ID, owner, env.ID, env.Sequence); err != nil {
		return e, err
	}
	if err := d.Outbox.MarkSentOwned(claimed.ID, owner); err != nil {
		return e, err
	}
	e.Sequence, e.State = env.Sequence, outbox.StatusSent
	return d.wake(ctx, o, e, digest)
}

func (d *Delivery) wake(ctx context.Context, o Order, e Evidence, _ string) (Evidence, error) {
	r, err := d.Waker.Wake(ctx, WakeRequest{Order: o, MessageID: e.MessageID, Sequence: e.Sequence})
	if err != nil {
		return e, fmt.Errorf("control wake failed (order retained for retry): %w", err)
	}
	if r.MessageID != e.MessageID || !r.Consumed {
		return e, ErrMissingReceipt
	}
	return e, nil
}

func (d *Delivery) Acknowledge(o Order) error {
	_, err := d.AcknowledgeEvidence(context.Background(), o)
	return err
}
func (d *Delivery) AcknowledgeContext(ctx context.Context, o Order) error {
	_, err := d.AcknowledgeEvidence(ctx, o)
	return err
}
func (d *Delivery) Supersede(o Order) error {
	_, err := d.SupersedeEvidence(context.Background(), o)
	return err
}
func (d *Delivery) SupersedeContext(ctx context.Context, o Order) error {
	_, err := d.SupersedeEvidence(ctx, o)
	return err
}
func (d *Delivery) AcknowledgeEvidence(ctx context.Context, o Order) (Evidence, error) {
	return d.terminal(ctx, o, false)
}
func (d *Delivery) SupersedeEvidence(ctx context.Context, o Order) (Evidence, error) {
	return d.terminal(ctx, o, true)
}
func (d *Delivery) terminal(ctx context.Context, o Order, supersede bool) (Evidence, error) {
	if d == nil || d.Outbox == nil || d.Evidence == nil || d.Authority == nil {
		return Evidence{}, fmt.Errorf("control: outbox, identity authority, and structured evidence reader are required")
	}
	if err := d.validate(ctx, o); err != nil {
		return Evidence{}, err
	}
	key, _, _, err := identityKey(o)
	if err != nil {
		return Evidence{}, err
	}
	item, err := d.Outbox.GetByKey(key)
	if err != nil {
		return Evidence{}, err
	}
	if item == nil {
		return Evidence{}, fmt.Errorf("control: no durable order for %s", key)
	}
	if item.Status != outbox.StatusSent && item.Status != outbox.StatusAcknowledged && item.Status != outbox.StatusSuperseded {
		return Evidence{}, fmt.Errorf("control: order is not sent (state %s)", item.Status)
	}
	var stored Order
	if err := json.Unmarshal([]byte(item.Payload), &stored); err != nil {
		return Evidence{}, fmt.Errorf("control: corrupt stored order: %w", err)
	}
	if stored != o || item.MessageID == "" || item.Sequence <= 0 {
		return Evidence{}, fmt.Errorf("control: stored order identity mismatch")
	}
	_, digest, _, err := identityKey(o)
	if err != nil {
		return Evidence{}, err
	}
	if o.BodyDigest == "" {
		o.BodyDigest = digest
	}
	evidence, err := d.Evidence.ReadEvidence(ctx, key, supersede)
	if err != nil {
		return Evidence{}, err
	}
	if evidence.IdempotencyKey != key || evidence.MessageID != item.MessageID || evidence.Sequence != item.Sequence || evidence.Repository != o.Repository || evidence.TaskRef != o.TaskRef || evidence.Lane != o.Lane || evidence.LeaseGeneration != o.LeaseGeneration || evidence.CandidateSHA != o.CandidateSHA || evidence.Kind != o.Kind || evidence.BodyDigest != digest {
		return Evidence{}, fmt.Errorf("control: corrupt or mismatched durable acknowledgement evidence")
	}
	if supersede {
		if item.Status == outbox.StatusSuperseded {
			return Evidence{ItemID: item.ID, MessageID: item.MessageID, Sequence: item.Sequence, IdempotencyKey: key, State: item.Status}, nil
		}
		if err := d.Outbox.MarkSuperseded(item.ID); err != nil {
			return Evidence{}, err
		}
		return Evidence{ItemID: item.ID, MessageID: item.MessageID, Sequence: item.Sequence, IdempotencyKey: key, State: outbox.StatusSuperseded}, nil
	}
	if item.Status == outbox.StatusAcknowledged {
		return Evidence{ItemID: item.ID, MessageID: item.MessageID, Sequence: item.Sequence, IdempotencyKey: key, State: item.Status}, nil
	}
	if err := d.Outbox.MarkAcknowledged(item.ID); err != nil {
		return Evidence{}, err
	}
	return Evidence{ItemID: item.ID, MessageID: item.MessageID, Sequence: item.Sequence, IdempotencyKey: key, State: outbox.StatusAcknowledged}, nil
}
