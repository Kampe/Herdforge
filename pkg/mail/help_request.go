package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// HelpRequest is the durable, structured signal emitted when a lane is
// blocked. RequestID is stable for a lane/task/reason/capability tuple, so a
// retry after an unchanged block cannot create broadcast churn.
type HelpRequest struct {
	RequestID       string `json:"request_id"`
	Lane            string `json:"lane"`
	TaskRef         string `json:"task_ref"`
	Reason          string `json:"reason"`
	Capability      string `json:"capability"`
	SuggestedHelper string `json:"suggested_helper"`
	SuggestedFamily string `json:"suggested_family,omitempty"`
}

const helpRequestSubjectPrefix = "help-request:"

// HelpRequestID returns the stable identity used for blocked-reason dedupe.
func HelpRequestID(req HelpRequest) string {
	canonical := strings.Join([]string{
		normalizeHelpPart(req.Lane), normalizeHelpPart(req.TaskRef),
		normalizeHelpPart(req.Reason), normalizeHelpPart(req.Capability),
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return "help-" + hex.EncodeToString(sum[:])
}

// PostHelpRequest durably fans one request out to the blocked lane, its
// selected helper, and both standing authority inboxes. Each recipient gets
// the same request id and a recipient-bound envelope id, making retries
// idempotent while preserving visibility at every required control surface.
func (m *Mailbox) PostHelpRequest(sender string, req HelpRequest) ([]*Envelope, error) {
	return m.PostHelpRequestContext(context.Background(), sender, req)
}

func (m *Mailbox) PostHelpRequestContext(ctx context.Context, sender string, req HelpRequest) ([]*Envelope, error) {
	if m == nil {
		return nil, fmt.Errorf("mail: nil mailbox")
	}
	req.Lane = strings.TrimSpace(req.Lane)
	req.TaskRef = strings.TrimSpace(req.TaskRef)
	req.Reason = strings.TrimSpace(req.Reason)
	req.Capability = strings.TrimSpace(req.Capability)
	req.SuggestedHelper = strings.TrimSpace(req.SuggestedHelper)
	req.SuggestedFamily = strings.TrimSpace(req.SuggestedFamily)
	if req.Lane == "" || req.TaskRef == "" || req.Reason == "" || req.Capability == "" {
		return nil, fmt.Errorf("mail: help request requires lane, task ref, reason, and capability")
	}
	wantID := HelpRequestID(req)
	if req.RequestID != "" && req.RequestID != wantID {
		return nil, fmt.Errorf("mail: help request id %q does not match blocked reason identity %q", req.RequestID, wantID)
	}
	req.RequestID = wantID
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mail: marshal help request: %w", err)
	}
	recipients := uniqueHelpRecipients(req.Lane, req.SuggestedHelper, "supervisor", CoordinatorInbox)
	out := make([]*Envelope, 0, len(recipients))
	for _, recipient := range recipients {
		env := &Envelope{
			ID:        req.RequestID + ":" + recipient,
			Sender:    sender,
			Recipient: recipient,
			Subject:   helpRequestSubjectPrefix + req.RequestID,
			Body:      string(body),
		}
		if err := m.AppendEnvelopeContext(ctx, env); err != nil {
			return nil, fmt.Errorf("mail: persist help request for %s: %w", recipient, err)
		}
		out = append(out, env)
	}
	return out, nil
}

// DrainHelpRequests returns structured help requests addressed to recipient.
// Mailbox durability and recipient filtering remain the single source of
// truth; duplicate recipient reads are collapsed by request id.
func (m *Mailbox) DrainHelpRequests(recipient string) ([]HelpRequest, error) {
	envs, err := m.ReadInbox(recipient)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	out := make([]HelpRequest, 0)
	for _, env := range envs {
		if env == nil || !strings.HasPrefix(env.Subject, helpRequestSubjectPrefix) {
			continue
		}
		var req HelpRequest
		if err := json.Unmarshal([]byte(env.Body), &req); err != nil {
			return nil, fmt.Errorf("mail: malformed help request %s: %w", env.ID, err)
		}
		if req.RequestID == "" {
			return nil, fmt.Errorf("mail: help request %s has no request id", env.ID)
		}
		if _, ok := seen[req.RequestID]; ok {
			continue
		}
		seen[req.RequestID] = struct{}{}
		out = append(out, req)
	}
	return out, nil
}

func uniqueHelpRecipients(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func normalizeHelpPart(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}
