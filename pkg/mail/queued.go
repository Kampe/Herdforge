package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// QueuedDeliverySubject marks a routine message that was filed because the
// recipient was not at an idle/done turn boundary. Drainers filter on this
// prefix so control, callback, and help traffic stay on their own paths.
const QueuedDeliverySubject = "herd.queued/v1"

// AnonymousIssuer is the sender every UNBOUND caller of herd send shares when
// no lane identity is exported. It is a default, not an identity: two
// unrelated coordinators both appear as this, so equality on it proves
// nothing about who queued what. Identity-scoped operations must refuse it,
// and mail already queued under it is permanently anonymous and preserved.
const AnonymousIssuer = "herd-send"

// QueuedEnvelopeID is the stable identity of one routine payload to one
// recipient in one target binding. Retries of the same
// sender/recipient/binding/body reuse the id so the mailbox append is
// idempotent. The seen-set that backs append is NOT a processing
// acknowledgment.
//
// binding participates in the identity because a recipient NAME is not a
// recipient. The same lane name relaunched after a reboot is a different
// session with different work in flight; without the binding, an identical
// payload addressed to the new session collides with the old session's
// pending id and the append is silently skipped, losing the new assignment.
//
// ponytail: an identical payload already pending from before bindings existed
// gets one new line rather than deduplicating against the unbound id. That is
// a one-time rollout cost, and duplicating a live assignment is the safe side
// of that trade.
func QueuedEnvelopeID(sender, recipient, binding, body string) string {
	canonical := strings.Join([]string{
		strings.TrimSpace(sender),
		strings.TrimSpace(recipient),
		strings.TrimSpace(binding),
		body,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return "queued-" + hex.EncodeToString(sum[:16])
}

// QueueRoutine durably appends one routine payload bound to the exact live
// target the issuer resolved. A repeated call with the same identity returns
// the existing envelope without writing a second line. An empty binding is
// accepted (an issuer that cannot resolve one must still be able to queue),
// but it permanently marks the envelope as unbound: identity-scoped
// operations such as supersession will never retire it.
func (m *Mailbox) QueueRoutine(ctx context.Context, sender, recipient, binding, body string) (*Envelope, error) {
	if m == nil {
		return nil, fmt.Errorf("mail: nil mailbox")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sender = strings.TrimSpace(sender)
	recipient = strings.TrimSpace(recipient)
	if sender == "" || recipient == "" {
		return nil, fmt.Errorf("mail: queued delivery requires sender and recipient")
	}
	if body == "" {
		return nil, fmt.Errorf("mail: queued delivery requires a payload")
	}
	binding = strings.TrimSpace(binding)
	env := &Envelope{
		ID:        QueuedEnvelopeID(sender, recipient, binding, body),
		Sender:    sender,
		Recipient: recipient,
		Subject:   QueuedDeliverySubject,
		Body:      body,
		Binding:   binding,
	}
	if err := m.AppendEnvelopeContext(ctx, env); err != nil {
		return nil, err
	}
	return env, nil
}

// PendingQueued returns unacknowledged routine envelopes for recipient, in
// mailbox order. Handled ids are skipped; the append seen-set is not consulted
// as a processing ack.
func (m *Mailbox) PendingQueued(recipient string) ([]*Envelope, error) {
	return m.pendingRoutine(recipient, func(env *Envelope) bool {
		return env.Subject == QueuedDeliverySubject
	})
}

// PendingRoutine returns unacknowledged ordinary report mail for recipient.
// Authenticated control and callback envelopes stay on their own consumers;
// everything else is eligible for the safe-boundary routine surface. The
// handled sidecar is the consumption acknowledgement, not the mailbox read.
func (m *Mailbox) PendingRoutine(recipient string) ([]*Envelope, error) {
	return m.pendingRoutine(recipient, func(env *Envelope) bool {
		if env.Subject == QueuedDeliverySubject {
			return true
		}
		if env.Read {
			return false
		}
		if IsControlSubject(env.Subject) {
			return false
		}
		return !strings.HasPrefix(env.Subject, "complete:") &&
			!strings.HasPrefix(env.Subject, "blocked:")
	})
}

// AcknowledgeOrdinary marks one exact ordinary report handled after its
// recipient has consumed it. Reads remain read-only; this is the explicit
// durable disposition operation. Queued routine, control, and callback
// envelopes stay on their native consumers.
func (m *Mailbox) AcknowledgeOrdinary(recipient, id string) error {
	if m == nil {
		return fmt.Errorf("mail: nil mailbox")
	}
	recipient, id = strings.TrimSpace(recipient), strings.TrimSpace(id)
	if recipient == "" || id == "" {
		return ErrRecipientAndEnvelopeIDRequired
	}
	envs, err := m.ReadInbox(recipient)
	if err != nil {
		return err
	}
	for _, env := range envs {
		if env == nil || env.ID != id {
			continue
		}
		if env.Subject == QueuedDeliverySubject || IsControlSubject(env.Subject) ||
			strings.HasPrefix(env.Subject, "complete:") || strings.HasPrefix(env.Subject, "blocked:") {
			return ordinaryEnvelopeClassError(id)
		}
		return m.MarkHandled(recipient, id)
	}
	return ordinaryEnvelopeNotFound(id, recipient)
}

func (m *Mailbox) pendingRoutine(recipient string, eligible func(*Envelope) bool) ([]*Envelope, error) {
	if m == nil {
		return nil, fmt.Errorf("mail: nil mailbox")
	}
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return nil, fmt.Errorf("mail: recipient is required")
	}
	envs, err := m.ReadInbox(recipient)
	if err != nil {
		return nil, err
	}
	st, err := loadAck(m.MailFile)
	if err != nil {
		return nil, err
	}
	present := make(map[string]*Envelope, len(envs))
	for _, env := range envs {
		if env != nil {
			present[env.ID] = env
		}
	}
	handled := make(map[string]struct{}, len(st.Handled[recipient]))
	for _, id := range st.Handled[recipient] {
		handled[id] = struct{}{}
	}
	out := make([]*Envelope, 0, len(envs))
	for _, env := range envs {
		if env == nil || !eligible(env) {
			continue
		}
		if _, ok := handled[env.ID]; ok && markStandsLocal(st, recipient, env, present) {
			continue
		}
		out = append(out, env)
	}
	return out, nil
}

// markStandsLocal answers the same question as Handled — may this mark be
// honoured? — from a batch already in hand, so listing a mailbox does not
// re-read it once per envelope. The RULE itself lives in exactly one place,
// supersessionHonoured; only the lookup differs.
func markStandsLocal(st *ackState, recipient string, victim *Envelope, present map[string]*Envelope) bool {
	rec, superseded := st.Superseded[recipient][victim.ID]
	if !superseded {
		return true
	}
	return supersessionHonoured(rec, victim, present[rec.ReplacementID])
}
