package mail

import (
	"context"
	"encoding/json"
	"strings"
)

// FAC-108: the agent-callback bus. Instead of the coordinator pane-scraping
// herdr status to guess when a builder finished, a completed (or blocked)
// agent POSTS a durable callback to the coordinator's inbox and the forge
// loop DRAINS it, reacting per-agent the instant it arrives. This is the
// nervous system the async loop (FAC-107) consumes.

// CoordinatorInbox is the recipient every agent callback is addressed to.
const CoordinatorInbox = "coordinator"

// CallbackKind is what an agent is reporting.
type CallbackKind string

const (
	// CallbackComplete: the agent finished and committed (SHA set).
	CallbackComplete CallbackKind = "complete"
	// CallbackBlocked: the agent needs coordinator eyes (Detail says why).
	CallbackBlocked CallbackKind = "blocked"
)

// Callback is one agent report about a card. FAC-126 binds each callback to
// the repo, lease generation, and sender role it was raised under so a
// CallbackConsumer can tell a stale or duplicate redelivery from real
// progress without reaching back into the envelope that carried it; SHA
// doubles as the candidate SHA referenced by the ticket's acceptance
// criteria. Sequence is set by the consumer from the envelope at drain
// time, not by the sender — it isn't meaningful until the message has
// actually been appended to a mailbox.
type Callback struct {
	Ref             string       `json:"ref"`
	Kind            CallbackKind `json:"kind"`
	SHA             string       `json:"sha,omitempty"`
	Detail          string       `json:"detail,omitempty"`
	Repo            string       `json:"repo,omitempty"`
	LeaseGeneration int64        `json:"lease_generation,omitempty"`
	SenderRole      string       `json:"sender_role,omitempty"`
	Sequence        int64        `json:"sequence,omitempty"`
}

// PostCallback delivers an agent callback to the coordinator inbox. The
// subject is the ref so a human scanning the mailbox sees what it is; the
// body is the JSON-encoded Callback the loop parses. SenderRole defaults to
// sender when the caller hasn't set it explicitly.
func (m *Mailbox) PostCallback(sender string, cb Callback) (*Envelope, error) {
	return m.PostCallbackContext(context.Background(), sender, cb)
}

// PostCallbackContext is PostCallback with caller-deadline inheritance on
// the mailbox flock (SendMessageContext).
func (m *Mailbox) PostCallbackContext(ctx context.Context, sender string, cb Callback) (*Envelope, error) {
	if cb.SenderRole == "" {
		cb.SenderRole = sender
	}
	body, err := json.Marshal(cb)
	if err != nil {
		return nil, err
	}
	subject := string(cb.Kind) + ": " + cb.Ref
	return m.SendMessageContext(ctx, sender, CoordinatorInbox, subject, string(body))
}

// DrainCallbacks reads and parses every callback in the coordinator inbox.
// Non-callback envelopes (plain messages) are skipped, not errors — the
// inbox is shared. Malformed callback bodies are skipped too.
func (m *Mailbox) DrainCallbacks() ([]Callback, error) {
	return m.DrainCallbacksContext(context.Background())
}

// DrainCallbacksContext is DrainCallbacks with deadline inheritance on
// ReadInbox quarantine lock acquisition.
func (m *Mailbox) DrainCallbacksContext(ctx context.Context) ([]Callback, error) {
	envs, err := m.ReadInboxContext(ctx, CoordinatorInbox)
	if err != nil {
		return nil, err
	}
	var out []Callback
	for _, e := range envs {
		if !isCallbackSubject(e.Subject) {
			continue
		}
		var cb Callback
		if json.Unmarshal([]byte(e.Body), &cb) != nil || cb.Ref == "" {
			continue
		}
		out = append(out, cb)
	}
	return out, nil
}

func isCallbackSubject(subject string) bool {
	return strings.HasPrefix(subject, string(CallbackComplete)+":") ||
		strings.HasPrefix(subject, string(CallbackBlocked)+":")
}
