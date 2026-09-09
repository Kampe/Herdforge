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

// QueuedEnvelopeID is the stable identity of one routine payload to one
// recipient. Retries of the same sender/recipient/body reuse the id so the
// mailbox append is idempotent. The seen-set that backs append is NOT a
// processing acknowledgment.
func QueuedEnvelopeID(sender, recipient, body string) string {
	canonical := strings.Join([]string{
		strings.TrimSpace(sender),
		strings.TrimSpace(recipient),
		body,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return "queued-" + hex.EncodeToString(sum[:16])
}

// QueueRoutine durably appends one routine payload. A repeated call with the
// same identity returns the existing envelope without writing a second line.
func (m *Mailbox) QueueRoutine(ctx context.Context, sender, recipient, body string) (*Envelope, error) {
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
	env := &Envelope{
		ID:        QueuedEnvelopeID(sender, recipient, body),
		Sender:    sender,
		Recipient: recipient,
		Subject:   QueuedDeliverySubject,
		Body:      body,
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
		return fmt.Errorf("mail: recipient and envelope id are required")
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
			return fmt.Errorf("mail: envelope %q is not an ordinary report", id)
		}
		return m.MarkHandled(recipient, id)
	}
	return fmt.Errorf("mail: ordinary envelope %q for recipient %q was not found", id, recipient)
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
	out := make([]*Envelope, 0, len(envs))
	for _, env := range envs {
		if env == nil || !eligible(env) {
			continue
		}
		handled, hErr := m.Handled(recipient, env.ID)
		if hErr != nil {
			return nil, hErr
		}
		if handled {
			continue
		}
		out = append(out, env)
	}
	return out, nil
}
