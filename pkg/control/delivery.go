// Package control separates durable coordinator orders from Herdr wakeups.
// The mailbox is the durable recipient-facing envelope; Herdr is only a
// retryable nudge that asks a lane to consume that envelope.
package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	Target    WakeTarget
}

type WakeReceipt struct {
	MessageID       string `json:"message_id"`
	Consumed        bool   `json:"consumed"`
	Verified        bool   `json:"verified"`
	SequenceToken   string `json:"sequence_token"`
	Baseline        string `json:"baseline_status"`
	Final           string `json:"final_status"`
	Target          string `json:"target"`
	Workspace       string `json:"workspace"`
	TabID           string `json:"tab_id"`
	PaneID          string `json:"pane_id"`
	AgentName       string `json:"agent_name"`
	Provider        string `json:"provider"`
	SessionID       string `json:"session_id"`
	LeaseGeneration int64  `json:"lease_generation"`
}

// Sender is implemented by both mail.Mailbox and mail.MessageBroker through
// the small adapters below.  Keeping this interface here makes file-only and
// Redis-backed conformance tests exercise identical delivery semantics.
type Sender interface {
	SendEnvelopeContext(context.Context, *mail.Envelope) error
}

type Waker interface {
	WakeTarget() WakeTarget
	ReadTarget(context.Context) (WakeTarget, error)
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
	Outcome         string `json:"outcome,omitempty"`
	FailureReason   string `json:"failure_reason,omitempty"`
	Retryable       bool   `json:"retryable,omitempty"`
	EnvelopeID      string `json:"envelope_id"`
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
	Wake           WakeReceipt   `json:"wake"`
}

var (
	ErrStaleIdentity    = errors.New("control: stale lane identity")
	ErrMissingReceipt   = errors.New("control: missing or corrupt wake receipt")
	ErrEvidenceNotFound = errors.New("control: durable evidence not found")
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
		e.MessageID, e.Sequence = item.MessageID, item.Sequence
		return e, nil
	}
	if wake, ok, err := loadWake(d.Outbox.DB(), item.ID); err != nil {
		return e, err
	} else if ok {
		current, readErr := d.Waker.ReadTarget(ctx)
		target := d.Waker.WakeTarget()
		if readErr != nil || current != target || !wake.Consumed || !wake.Verified || wake.MessageID != item.MessageID || wake.SequenceToken == "" || wake.Target != current.Target || wake.Workspace != current.Workspace || wake.TabID != current.TabID || wake.PaneID != current.PaneID || wake.AgentName != current.AgentName || wake.Provider != current.Provider || wake.SessionID != current.SessionID || wake.LeaseGeneration != current.LeaseGeneration || wake.LeaseGeneration != o.LeaseGeneration {
			return e, ErrMissingReceipt
		}
		e.MessageID, e.Sequence, e.Wake, e.State = item.MessageID, item.Sequence, wake, outbox.StatusSent
		return e, nil
	}
	if item.Status == outbox.StatusSent {
		if item.MessageID == "" || item.Sequence <= 0 {
			return e, ErrMissingReceipt
		}
		e.MessageID, e.Sequence = item.MessageID, item.Sequence
		if d.Evidence != nil {
			if _, err := d.Evidence.ReadEvidence(ctx, key, false); err == nil {
				return e, nil
			}
			if _, err := d.Evidence.ReadEvidence(ctx, key, true); err == nil {
				return e, nil
			}
		}
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
	// Re-read live authority immediately before the only wake. This closes the
	// window where a lease/candidate can drift after persistence.
	if err := d.validate(ctx, o); err != nil {
		return e, err
	}
	// Waker implementations must validate this freshly-authorized target, not
	// rely on a target captured when the factory was constructed.
	target := d.Waker.WakeTarget()
	r, err := d.Waker.Wake(ctx, WakeRequest{Order: o, MessageID: e.MessageID, Sequence: e.Sequence, Target: target})
	if err != nil {
		return e, fmt.Errorf("control wake failed (order retained for retry): %w", err)
	}
	if r.MessageID != e.MessageID || !r.Consumed || !r.Verified || r.Target != target.Target || r.Workspace != target.Workspace || r.TabID != target.TabID || r.PaneID != target.PaneID || r.AgentName != target.AgentName || r.Provider != target.Provider || r.SessionID != target.SessionID || r.LeaseGeneration != target.LeaseGeneration || target.LeaseGeneration != o.LeaseGeneration {
		return e, ErrMissingReceipt
	}
	e.Wake = r
	if err := saveWake(d.Outbox.DB(), e.ItemID, r); err != nil {
		return e, err
	}
	return e, nil
}

func ensureWakeTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS control_wake_receipts (item_id INTEGER PRIMARY KEY, receipt_json TEXT NOT NULL)`)
	return err
}

func saveWake(db *sql.DB, itemID int64, receipt WakeReceipt) error {
	if err := ensureWakeTable(db); err != nil {
		return fmt.Errorf("control: wake receipt schema: %w", err)
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO control_wake_receipts(item_id, receipt_json) VALUES(?, ?) ON CONFLICT(item_id) DO UPDATE SET receipt_json=excluded.receipt_json`, itemID, string(b))
	return err
}

func loadWake(db *sql.DB, itemID int64) (WakeReceipt, bool, error) {
	if err := ensureWakeTable(db); err != nil {
		return WakeReceipt{}, false, err
	}
	var raw string
	err := db.QueryRow(`SELECT receipt_json FROM control_wake_receipts WHERE item_id = ?`, itemID).Scan(&raw)
	if err == sql.ErrNoRows {
		return WakeReceipt{}, false, nil
	}
	if err != nil {
		return WakeReceipt{}, false, err
	}
	var r WakeReceipt
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return WakeReceipt{}, false, fmt.Errorf("control: corrupt wake receipt: %w", err)
	}
	return r, true, nil
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

// Reconcile is the coordinator restart path. It never creates an order and
// never infers terminal state from a wake; only structured ack or supersede
// evidence can drive the terminal CAS.
func (d *Delivery) Reconcile(ctx context.Context, o Order) (Evidence, error) {
	if d == nil || d.Outbox == nil || d.Evidence == nil {
		return Evidence{}, fmt.Errorf("control: reconciliation requires outbox and evidence reader")
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
	if item.Status == outbox.StatusAcknowledged || item.Status == outbox.StatusSuperseded {
		return Evidence{ItemID: item.ID, MessageID: item.MessageID, Sequence: item.Sequence, IdempotencyKey: key, State: item.Status}, nil
	}
	ev, ackErr := d.AcknowledgeEvidence(ctx, o)
	if ackErr == nil {
		return ev, nil
	}
	if !errors.Is(ackErr, ErrEvidenceNotFound) {
		return Evidence{}, ackErr
	}
	return d.SupersedeEvidence(ctx, o)
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
	_, digest, _, err := identityKey(stored)
	if err != nil {
		return Evidence{}, err
	}
	canonical := o
	if canonical.BodyDigest == "" {
		canonical.BodyDigest = digest
	}
	if stored != canonical || item.MessageID == "" || item.Sequence <= 0 {
		return Evidence{}, fmt.Errorf("control: stored order identity mismatch")
	}
	evidence, err := d.Evidence.ReadEvidence(ctx, key, supersede)
	if err != nil {
		return Evidence{}, err
	}
	wantEnvelopeID := "ack-" + shortID(key)
	if supersede {
		wantEnvelopeID = "supersede-" + shortID(key)
	}
	if evidence.IdempotencyKey != key || evidence.EnvelopeID != wantEnvelopeID || evidence.MessageID != item.MessageID || evidence.Sequence != item.Sequence || evidence.Repository != o.Repository || evidence.TaskRef != o.TaskRef || evidence.Lane != o.Lane || evidence.LeaseGeneration != o.LeaseGeneration || evidence.CandidateSHA != o.CandidateSHA || evidence.Kind != o.Kind || evidence.BodyDigest != digest {
		return Evidence{}, fmt.Errorf("control: corrupt or mismatched durable acknowledgement evidence")
	}
	if supersede && (evidence.Outcome != "superseded" || strings.TrimSpace(evidence.FailureReason) == "") {
		return Evidence{}, fmt.Errorf("control: supersession requires outcome and failure reason")
	}
	if !supersede && evidence.Outcome != "acknowledged" {
		return Evidence{}, fmt.Errorf("control: acknowledgement outcome mismatch")
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
