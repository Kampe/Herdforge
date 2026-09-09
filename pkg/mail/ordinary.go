package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// MaxOrdinaryImportBytes bounds one collector payload, including JSON.
	MaxOrdinaryImportBytes = 8 << 20
	maxOrdinaryLabelBytes  = 256
)

// ImportedOrdinaryID is a local stable identity. The source id is metadata,
// not authentication; the local mailbox still controls delivery and ack.
func ImportedOrdinaryID(sourceHost, sourceID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sourceHost) + "\x00" + strings.TrimSpace(sourceID)))
	return "relay-" + hex.EncodeToString(sum[:16])
}

// IsOrdinaryReport excludes queued, authenticated control, and callback mail.
// Read is intentionally not part of classification: import ignores a source
// read assertion, while PendingOrdinary applies local pending state.
func IsOrdinaryReport(env *Envelope) bool {
	if env == nil || env.Subject == QueuedDeliverySubject || IsControlSubject(env.Subject) {
		return false
	}
	return !strings.HasPrefix(env.Subject, "complete:") &&
		!strings.HasPrefix(env.Subject, "blocked:")
}

// PendingOrdinary returns exact ordinary reports not locally acknowledged.
func (m *Mailbox) PendingOrdinary(recipient string) ([]*Envelope, error) {
	return m.pendingRoutine(recipient, func(env *Envelope) bool {
		return !env.Read && IsOrdinaryReport(env)
	})
}

// OrdinaryStatus is the read-only disposition view used by collectors.
type OrdinaryStatus struct {
	ID        string `json:"id"`
	Recipient string `json:"recipient"`
	Ordinary  bool   `json:"ordinary"`
	Pending   bool   `json:"pending"`
	Handled   bool   `json:"handled"`
}

// StatusOrdinary reports local state for one exact ordinary envelope.
func (m *Mailbox) StatusOrdinary(recipient, id string) (OrdinaryStatus, error) {
	if m == nil {
		return OrdinaryStatus{}, fmt.Errorf("mail: nil mailbox")
	}
	recipient, id = strings.TrimSpace(recipient), strings.TrimSpace(id)
	if recipient == "" || id == "" {
		return OrdinaryStatus{}, ErrRecipientAndEnvelopeIDRequired
	}
	envs, err := m.ReadInbox(recipient)
	if err != nil {
		return OrdinaryStatus{}, err
	}
	for _, env := range envs {
		if env == nil || env.ID != id {
			continue
		}
		if !IsOrdinaryReport(env) {
			return OrdinaryStatus{}, ordinaryEnvelopeClassError(id)
		}
		handled, err := m.Handled(recipient, id)
		if err != nil {
			return OrdinaryStatus{}, err
		}
		return OrdinaryStatus{ID: id, Recipient: recipient, Ordinary: true, Pending: !handled, Handled: handled}, nil
	}
	return OrdinaryStatus{}, ordinaryEnvelopeNotFound(id, recipient)
}

// ImportOrdinary appends one source-host-labelled ordinary envelope. Retries
// with the same source identity and bytes are no-ops; changed bytes or target
// identity refuse before any append. Source Read/handled assertions never
// become local disposition state.
func (m *Mailbox) ImportOrdinary(ctx context.Context, sourceHost, recipient string, source *Envelope) (*Envelope, error) {
	if m == nil {
		return nil, fmt.Errorf("mail: nil mailbox")
	}
	sourceHost, recipient = strings.TrimSpace(sourceHost), strings.TrimSpace(recipient)
	if source == nil {
		return nil, fmt.Errorf("mail: ordinary import requires an envelope")
	}
	if sourceHost == "" || recipient == "" || len(sourceHost) > maxOrdinaryLabelBytes || len(recipient) > maxOrdinaryLabelBytes {
		return nil, fmt.Errorf("mail: source host and recipient are required and bounded")
	}
	if source.ID == "" || len(source.ID) > maxOrdinaryLabelBytes {
		return nil, fmt.Errorf("mail: source envelope id is required and bounded")
	}
	if source.Recipient != recipient {
		return nil, fmt.Errorf("mail: source recipient %q does not match exact recipient %q", source.Recipient, recipient)
	}
	if source.OriginalSourceHost != "" || source.OriginalSourceID != "" {
		return nil, fmt.Errorf("mail: source metadata cannot be supplied by an ordinary import")
	}
	if !IsOrdinaryReport(source) {
		return nil, fmt.Errorf("mail: source envelope %q is not an ordinary report", source.ID)
	}
	if source.Sender == "" || len(source.Body) > MaxOrdinaryImportBytes {
		return nil, fmt.Errorf("mail: ordinary import sender/body is invalid or oversized")
	}
	localID := ImportedOrdinaryID(sourceHost, source.ID)
	existing, err := m.ReadInbox("")
	if err != nil {
		return nil, err
	}
	for _, env := range existing {
		if env == nil {
			continue
		}
		if env.OriginalSourceHost == sourceHost && env.OriginalSourceID == source.ID {
			if !sameImportedOrdinary(env, source, recipient) {
				return nil, fmt.Errorf("mail: source identity %q changed bytes or recipient", source.ID)
			}
			return env, nil
		}
		if env.ID == localID {
			return nil, fmt.Errorf("mail: local relay identity collision for source %q", source.ID)
		}
	}
	local := &Envelope{
		ID:                 localID,
		Sender:             source.Sender,
		Recipient:          recipient,
		Subject:            source.Subject,
		Body:               source.Body,
		Read:               false,
		Timestamp:          source.Timestamp,
		OriginalSourceHost: sourceHost,
		OriginalSourceID:   source.ID,
	}
	if err := m.AppendEnvelopeContext(ctx, local); err != nil {
		return nil, err
	}
	return local, nil
}

func sameImportedOrdinary(local, source *Envelope, recipient string) bool {
	return local.Recipient == recipient && local.Sender == source.Sender &&
		local.Subject == source.Subject && local.Body == source.Body
}
