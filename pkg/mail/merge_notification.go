package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const MergeNotificationSubject = "merge/landed"

// MergeNotification is the durable, lane-addressed proof that a reviewed
// builder candidate landed on the integration branch. BuilderID is copied
// from the authenticated launch receipt and is also the mailbox recipient.
type MergeNotification struct {
	TaskRef      string `json:"task_ref"`
	CandidateSHA string `json:"candidate_sha"`
	LandedCommit string `json:"landed_commit"`
	BaseSHA      string `json:"base_sha"`
	Branch       string `json:"branch"`
	Repository   string `json:"repository"`
	BuilderID    string `json:"builder_id"`
}

func (n MergeNotification) validate() error {
	for name, value := range map[string]string{
		"task_ref": n.TaskRef, "candidate_sha": n.CandidateSHA,
		"landed_commit": n.LandedCommit, "base_sha": n.BaseSHA,
		"branch": n.Branch, "repository": n.Repository, "builder_id": n.BuilderID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("merge notification: %s is required", name)
		}
	}
	return nil
}

func mergeNotificationID(n MergeNotification) string {
	sum := sha256.Sum256([]byte(n.TaskRef + "\x00" + n.CandidateSHA + "\x00" + n.LandedCommit))
	return "merge-landed-" + hex.EncodeToString(sum[:])
}

// PostMergeNotification durably appends one notification. The stable ID is
// the effect identity, so retries and process restarts converge on one line.
func (m *Mailbox) PostMergeNotification(sender string, n MergeNotification) (*Envelope, error) {
	return m.PostMergeNotificationContext(context.Background(), sender, n)
}

func (m *Mailbox) PostMergeNotificationContext(ctx context.Context, sender string, n MergeNotification) (*Envelope, error) {
	if m == nil {
		return nil, fmt.Errorf("merge notification: mailbox is required")
	}
	if strings.TrimSpace(sender) == "" {
		return nil, fmt.Errorf("merge notification: sender is required")
	}
	if err := n.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(n)
	if err != nil {
		return nil, fmt.Errorf("merge notification: encode: %w", err)
	}
	env := &Envelope{
		ID:        mergeNotificationID(n),
		Sender:    sender,
		Recipient: n.BuilderID,
		Subject:   MergeNotificationSubject,
		Body:      string(body),
	}
	if err := m.SendEnvelopeContext(ctx, env); err != nil {
		return nil, err
	}
	return env, nil
}

// DecodeMergeNotification validates and decodes one mailbox envelope.
func DecodeMergeNotification(env *Envelope) (*MergeNotification, error) {
	if env == nil || env.Subject != MergeNotificationSubject {
		return nil, fmt.Errorf("merge notification: unexpected envelope")
	}
	var n MergeNotification
	if err := json.Unmarshal([]byte(env.Body), &n); err != nil {
		return nil, fmt.Errorf("merge notification: decode: %w", err)
	}
	if err := n.validate(); err != nil {
		return nil, err
	}
	if env.Recipient != n.BuilderID || env.ID != mergeNotificationID(n) {
		return nil, fmt.Errorf("merge notification: envelope identity mismatch")
	}
	return &n, nil
}
