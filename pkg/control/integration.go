package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/mail"
)

type ScopedAuthority struct{ Identity LaneIdentity }

func (a ScopedAuthority) Resolve(_ context.Context, _ Order) (LaneIdentity, error) {
	return a.Identity, nil
}

// FencedAuthority re-checks the live lease at every delivery/terminal call.
// The identity captured when a lane was started is not authority by itself.
type FencedAuthority struct {
	Identity LaneIdentity
	Check    func(context.Context, Order) error
}

func (a FencedAuthority) Resolve(ctx context.Context, o Order) (LaneIdentity, error) {
	if a.Check == nil {
		return LaneIdentity{}, fmt.Errorf("control: live identity check is required")
	}
	if err := a.Check(ctx, o); err != nil {
		return LaneIdentity{}, err
	}
	return a.Identity, nil
}

// NewOwnerToken creates a unique token per delivery instance. It is never a
// process-wide default, so an expired owner cannot mutate a later takeover.
func NewOwnerToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "control-" + hex.EncodeToString(b[:]), nil
}

// MailboxEvidenceReader reads only durable coordinator-inbox receipts. It
// never derives acknowledgement from Herdr status or a local boolean.
type MailboxEvidenceReader struct{ Mailbox *mail.Mailbox }

func (r MailboxEvidenceReader) ReadEvidence(ctx context.Context, key string, supersede bool) (AckEvidence, error) {
	if r.Mailbox == nil {
		return AckEvidence{}, fmt.Errorf("control: mailbox evidence reader requires mailbox")
	}
	envs, err := r.Mailbox.ReadInboxContext(ctx, mail.CoordinatorInbox)
	if err != nil {
		return AckEvidence{}, err
	}
	want := "control/ack"
	if supersede {
		want = "control/supersede"
	}
	for _, env := range envs {
		if env.Subject != want {
			continue
		}
		if env.Sender == "" || env.Recipient != mail.CoordinatorInbox || env.ID == "" || env.Sequence <= 0 {
			return AckEvidence{}, fmt.Errorf("control: malformed durable evidence envelope")
		}
		var evidence AckEvidence
		if err := json.Unmarshal([]byte(env.Body), &evidence); err != nil {
			return AckEvidence{}, fmt.Errorf("control: corrupt durable evidence: %w", err)
		}
		if evidence.IdempotencyKey == key {
			if env.Sender != evidence.Lane || env.Recipient != mail.CoordinatorInbox || env.ID != "ack-"+shortID(key) && env.ID != "supersede-"+shortID(key) {
				return AckEvidence{}, fmt.Errorf("control: durable evidence envelope identity mismatch")
			}
			evidence.EnvelopeID = env.ID
			return evidence, nil
		}
	}
	return AckEvidence{}, fmt.Errorf("%w: %s", ErrEvidenceNotFound, key)
}

// HerdrWaker is the only Herdr integration used by the durable adapter. Its
// receipt proves prompt consumption only; it never acknowledges or finalizes
// the durable order.
type WakeTarget struct {
	Target          string
	Workspace       string
	TabID           string
	PaneID          string
	AgentName       string
	Provider        string
	LeaseGeneration int64
	SessionID       string
}

type HerdrWaker struct {
	Target   WakeTarget
	Timeout  time.Duration
	Validate func(context.Context, WakeTarget) (WakeTarget, error)
}

func (w HerdrWaker) WakeTarget() WakeTarget { return w.Target }

func (w HerdrWaker) ReadTarget(ctx context.Context) (WakeTarget, error) {
	if w.Validate == nil {
		return WakeTarget{}, fmt.Errorf("control: exact Herdr target validator is required")
	}
	actual, err := w.Validate(ctx, w.Target)
	if err != nil {
		return WakeTarget{}, err
	}
	if actual != w.Target {
		return WakeTarget{}, ErrStaleIdentity
	}
	return actual, nil
}

func (w HerdrWaker) Wake(ctx context.Context, req WakeRequest) (WakeReceipt, error) {
	if req.Target != w.Target {
		return WakeReceipt{}, ErrStaleIdentity
	}
	if w.Target.Target == "" || w.Target.Workspace == "" || w.Target.TabID == "" || w.Target.PaneID == "" || w.Target.AgentName == "" || w.Target.Provider == "" || w.Target.SessionID == "" || w.Target.LeaseGeneration <= 0 {
		return WakeReceipt{}, fmt.Errorf("control: exact Herdr target is required")
	}
	if w.Validate == nil {
		return WakeReceipt{}, fmt.Errorf("control: exact Herdr target validator is required")
	}
	actual, err := w.Validate(ctx, w.Target)
	if err != nil {
		return WakeReceipt{}, fmt.Errorf("control: Herdr target drift: %w", err)
	}
	if actual != w.Target {
		return WakeReceipt{}, ErrStaleIdentity
	}
	receipt, err := herdr.DeliverAndProve(w.Target.Target, fmt.Sprintf("consume durable control envelope %s seq %d", req.MessageID, req.Sequence), w.Timeout)
	if err != nil {
		return WakeReceipt{}, err
	}
	if receipt == nil || !receipt.Consumed || !receipt.Verified || receipt.Target != w.Target.Target {
		return WakeReceipt{}, ErrMissingReceipt
	}
	return WakeReceipt{MessageID: req.MessageID, Consumed: receipt.Consumed, Verified: receipt.Verified, SequenceToken: receipt.SequenceToken, Baseline: receipt.BaselineStatus, Final: receipt.FinalStatus, Target: receipt.Target, Workspace: actual.Workspace, TabID: actual.TabID, PaneID: actual.PaneID, AgentName: actual.AgentName, Provider: actual.Provider, SessionID: actual.SessionID, LeaseGeneration: actual.LeaseGeneration}, nil
}

// CoordinatorOrders is the production-facing order port used by dispatch,
// review, and review-supervisor flows. It carries the authoritative identity
// context once, so callers cannot manufacture a wake-only prompt without a
// durable order.
type CoordinatorOrders struct {
	Delivery *Delivery
	Identity LaneIdentity
	Consumer *Consumer
}

// Consume is the recipient-side production entrypoint bound to this exact
// lane identity. The caller's standing loop invokes it after a wake.
func (c *CoordinatorOrders) Consume(ctx context.Context) error {
	if c == nil || c.Consumer == nil {
		return fmt.Errorf("control: recipient consumer is required")
	}
	return (RecipientLoop{Consumer: c.Consumer}).RunOnce(ctx)
}

func (c *CoordinatorOrders) Reconcile(ctx context.Context, orders func(context.Context) ([]Order, error)) error {
	if c == nil {
		return ErrMissingReceipt
	}
	return (CoordinatorLoop{Delivery: c.Delivery, Orders: orders}).RunOnce(ctx)
}

func (c *CoordinatorOrders) send(ctx context.Context, kind Kind, body string) (Evidence, error) {
	if c == nil || c.Delivery == nil {
		return Evidence{}, ErrMissingReceipt
	}
	o := Order{LaneIdentity: c.Identity, Kind: kind, Body: body}
	return c.Delivery.Deliver(ctx, o)
}

func (c *CoordinatorOrders) Rebase(ctx context.Context, body string) (Evidence, error) {
	return c.send(ctx, KindRebase, body)
}
func (c *CoordinatorOrders) Repair(ctx context.Context, body string) (Evidence, error) {
	return c.send(ctx, KindRepair, body)
}
func (c *CoordinatorOrders) ReviewCorrection(ctx context.Context, body string) (Evidence, error) {
	return c.send(ctx, KindReviewCorrection, body)
}
func (c *CoordinatorOrders) Callback(ctx context.Context, body string) (Evidence, error) {
	return c.send(ctx, KindCallback, body)
}
