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
//
// FAC-185 residual (live herdr 0.7.5): there is no server-side compare-and-
// prompt. Wakes use `herdr agent prompt` against the pane id when known, and
// treat session id as late-bound provenance (empty at AgentStart is normal
// for claude/opencode; grok never reports one).
type WakeTarget struct {
	Target          string
	Workspace       string
	TabID           string
	PaneID          string
	AgentName       string
	Provider        string
	LeaseGeneration int64
	SessionID       string // optional / late-bound; empty is not a distinct agent
}

// PromptTarget is the herdr CLI address for agent prompt. Prefer pane id —
// it is unique across workspaces; tab labels can collide.
func (t WakeTarget) PromptTarget() string {
	if t.PaneID != "" {
		return t.PaneID
	}
	return t.Target
}

// CoreIdentityEqual compares the fields herdr guarantees for every agent kind.
// Session is intentionally excluded (see SessionCompatible).
func CoreIdentityEqual(a, b WakeTarget) bool {
	return a.Target == b.Target && a.Workspace == b.Workspace && a.TabID == b.TabID &&
		a.PaneID == b.PaneID && a.AgentName == b.AgentName && a.Provider == b.Provider &&
		a.LeaseGeneration == b.LeaseGeneration
}

// SessionCompatible reports whether a bound session and a live session refer
// to the same agent. Empty bound means "not observed yet" (session UUIDs only
// appear after claude/opencode boot; bind runs immediately after AgentStart).
// A previously bound non-empty session must still match; disappearance or
// swap is drift.
func SessionCompatible(bound, live string) bool {
	if bound == "" {
		return true
	}
	return bound == live
}

// WakeTargetsCompatible is CoreIdentityEqual plus directional session check
// (bound a → live b).
func WakeTargetsCompatible(bound, live WakeTarget) bool {
	return CoreIdentityEqual(bound, live) && SessionCompatible(bound.SessionID, live.SessionID)
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
	if !WakeTargetsCompatible(w.Target, actual) {
		return WakeTarget{}, ErrStaleIdentity
	}
	return actual, nil
}

// wakeText is what the woken agent actually reads. The wake used to send the
// bare protocol string "consume durable control envelope <id> seq <n>", which
// is addressed to the control plane, not to a person — and the agent is a
// person-shaped reader. Observed on the FAC-103 lane: Claude treated it as an
// unexplained directive, refused to act on an envelope ID it could not resolve
// to any CLI verb, and stopped on a clarifying question. The lane then sat
// "blocked" at an empty prompt with its packet unread. That is the silent-stall
// class this fleet kept hitting.
//
// The envelope id and sequence stay in the text as provenance — they are what
// makes a delivery receipt auditable — but they are framed so the reader knows
// the wake is a nudge and the packet is the work.
func wakeText(messageID string, sequence int64) string {
	return wakeTextWithReference(fmt.Sprintf("control envelope %s seq %d", messageID, sequence))
}

// WakeTextForTask is the same wake for the non-durable path in pkg/dispatch,
// which has a task ref rather than an envelope. It is exported and shared
// rather than hand-copied: a reviewer pointed out that a second copy of this
// string is a second place that can regress to a bare protocol directive with
// a green suite, and the wording had already drifted between the two.
func WakeTextForTask(taskRef string) string {
	return wakeTextWithReference(fmt.Sprintf("task %s", taskRef))
}

func wakeTextWithReference(reference string) string {
	return fmt.Sprintf(
		"Wake-up from the Herdforge coordinator (%s — provenance only, nothing for you to do with it). "+
			"Read TASK-PACKET.md in your worktree root and continue that task.",
		reference)
}

func (w HerdrWaker) Wake(ctx context.Context, req WakeRequest) (WakeReceipt, error) {
	// Request must carry the same core identity the waker was bound with.
	// Session may differ only in the empty→filled direction (checked live below).
	if !CoreIdentityEqual(req.Target, w.Target) {
		return WakeReceipt{}, ErrStaleIdentity
	}
	// Target/workspace/tab/pane/agent/provider plus a positive lease generation
	// IS the exact addressable identity — herdr resolves a pane from those
	// alone. SessionID is provenance only some kinds report: grok never does;
	// claude/opencode fill it only after boot (after the launch-time bind).
	if w.Target.Target == "" || w.Target.Workspace == "" || w.Target.TabID == "" || w.Target.PaneID == "" || w.Target.AgentName == "" || w.Target.Provider == "" || w.Target.LeaseGeneration <= 0 {
		return WakeReceipt{}, fmt.Errorf("control: exact Herdr target is required")
	}
	if w.Validate == nil {
		return WakeReceipt{}, fmt.Errorf("control: exact Herdr target validator is required")
	}
	actual, err := w.Validate(ctx, w.Target)
	if err != nil {
		return WakeReceipt{}, fmt.Errorf("control: Herdr target drift: %w", err)
	}
	if !WakeTargetsCompatible(w.Target, actual) {
		return WakeReceipt{}, ErrStaleIdentity
	}
	// Prefer pane id: unique, and what herdr's compare paths actually key on.
	promptAddr := w.Target.PromptTarget()
	receipt, err := herdr.DeliverAndProve(promptAddr, wakeText(req.MessageID, req.Sequence), w.Timeout)
	if err != nil {
		return WakeReceipt{}, err
	}
	// Receipt.Target is the CLI address we prompted (pane or label), not
	// necessarily the human tab label stored on WakeTarget.Target.
	if receipt == nil || !receipt.Consumed || !receipt.Verified {
		return WakeReceipt{}, ErrMissingReceipt
	}
	if receipt.Target != promptAddr {
		return WakeReceipt{}, ErrMissingReceipt
	}
	return WakeReceipt{
		MessageID:       req.MessageID,
		Consumed:        receipt.Consumed,
		Verified:        receipt.Verified,
		SequenceToken:   receipt.SequenceToken,
		Baseline:        receipt.BaselineStatus,
		Final:           receipt.FinalStatus,
		Target:          w.Target.Target,
		Workspace:       actual.Workspace,
		TabID:           actual.TabID,
		PaneID:          actual.PaneID,
		AgentName:       actual.AgentName,
		Provider:        actual.Provider,
		SessionID:       actual.SessionID,
		LeaseGeneration: actual.LeaseGeneration,
	}, nil
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
