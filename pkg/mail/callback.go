package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// FAC-108: the agent-callback bus. Instead of the coordinator pane-scraping
// herdr status to guess when a builder finished, a completed (or blocked)
// agent POSTS a durable callback to the coordinator's inbox and the forge
// loop DRAINS it, reacting per-agent the instant it arrives. This is the
// nervous system the async loop (FAC-107) consumes.

// CoordinatorInbox is the recipient every agent callback is addressed to.
const CoordinatorInbox = "coordinator"

// DefaultMailFile is the conventional repo-relative mailbox path production
// callers share (FAC-145). One file, one bus: producers (herd verify,
// approve) and the forge-loop consumer must agree on it.
const DefaultMailFile = ".herd/mail.jsonl"

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
	// DedupeID is a caller-stable identity for effect-level dedupe
	// (FAC-145): PostCallback posts a given DedupeID at most once per bus,
	// so crash-reconcile replays converge on one delivered callback.
	DedupeID string `json:"dedupe_id,omitempty"`
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
	if cb.DedupeID == "" {
		return m.SendMessageContext(ctx, sender, CoordinatorInbox, subject, string(body))
	}

	// Effect-level dedupe (FAC-145): scan and append happen inside ONE
	// mailbox transaction (mutex + cross-process flock), so two concurrent
	// posters of the same DedupeID cannot both observe "absent" and both
	// append — a given DedupeID lands at most once per bus. The scan is
	// corruption-aware and effect-aware: a malformed line or a DedupeID
	// collision with a DIFFERENT bound effect fails closed instead of
	// silently appending or returning someone else's callback as success.
	env := newEnvelope(sender, CoordinatorInbox, subject, string(body))
	var existing *Envelope
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureSeenLoadedLocked()
	err = m.withFileLock(func() error {
		ex, sErr := m.scanCallbackDedupeLocked(cb)
		if sErr != nil {
			return fmt.Errorf("callback dedupe scan: %w", sErr)
		}
		if ex != nil {
			existing = ex
			return nil
		}
		seq, err := m.nextSequenceLocked()
		if err != nil {
			return err
		}
		env.Sequence = seq
		data, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("failed to marshal mail envelope: %w", err)
		}
		return appendLine(m.MailFile, data)
	})
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	m.markSeenLocked(env.ID)
	return env, nil
}

// scanCallbackDedupeLocked scans the raw mailbox for a callback envelope
// carrying cb.DedupeID. It FAILS CLOSED on any malformed envelope line or
// malformed callback body — mailbox corruption must never turn into a
// duplicate authority callback — and rejects a DedupeID that resolves to a
// DIFFERENT bound effect (repo/ref/kind/sha/generation) as a collision.
// Caller must hold the mailbox file lock.
func (m *Mailbox) scanCallbackDedupeLocked(cb Callback) (*Envelope, error) {
	data, err := os.ReadFile(m.MailFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for i, l := range splitLines(string(data)) {
		if len(l) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(l), &env); err != nil {
			return nil, fmt.Errorf("mailbox line %d is malformed — refusing dedupe decision on corrupt state (FAC-145 fail-closed): %w", i+1, err)
		}
		if !isCallbackSubject(env.Subject) {
			continue
		}
		var c Callback
		if err := json.Unmarshal([]byte(env.Body), &c); err != nil {
			return nil, fmt.Errorf("mailbox line %d callback body is malformed — refusing dedupe decision on corrupt state (FAC-145 fail-closed): %w", i+1, err)
		}
		if c.DedupeID != cb.DedupeID {
			continue
		}
		if c.Ref != cb.Ref || c.Kind != cb.Kind || c.SHA != cb.SHA ||
			c.Repo != cb.Repo || c.LeaseGeneration != cb.LeaseGeneration {
			return nil, fmt.Errorf("callback DedupeID %s collides with a different bound effect (have %s/%s@%s gen%d, posting %s/%s@%s gen%d) — refusing (FAC-145)",
				cb.DedupeID, c.Repo, c.Ref, c.SHA, c.LeaseGeneration, cb.Repo, cb.Ref, cb.SHA, cb.LeaseGeneration)
		}
		e := env
		return &e, nil
	}
	return nil, nil
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

// Verdict record identities (FAC-145). An INTENT is never consumable: only
// a DELIVERED record — written after confirmed provider delivery and
// readback — participates in EffectiveVerdict.
const (
	verdictDeliveredPrefix = "verdict-delivered:"
	verdictIntentPrefix    = "verdict-intent:"
)

// VerdictEffectID is the consumable delivered-verdict identity.
func VerdictEffectID(effect string) string { return verdictDeliveredPrefix + effect }

// VerdictIntentID is the non-consumable pre-delivery intent identity.
func VerdictIntentID(effect string) string { return verdictIntentPrefix + effect }

// HasDeliveredVerdict reports whether a delivered verdict with this exact
// identity already exists — the exactly-once guard for retries.
func (m *Mailbox) HasDeliveredVerdict(deliveredID string) (Callback, bool, error) {
	envs, err := m.ReadInbox(CoordinatorInbox)
	if err != nil {
		return Callback{}, false, err
	}
	// The WHOLE bus is validated before any answer is returned: a corrupt
	// body anywhere means the state is undecidable, even if a match was
	// already seen (FAC-145 fail-closed).
	var match Callback
	found := false
	for _, e := range envs {
		if !isCallbackSubject(e.Subject) {
			continue
		}
		var cb Callback
		if err := json.Unmarshal([]byte(e.Body), &cb); err != nil {
			return Callback{}, false, fmt.Errorf("corrupt callback body in envelope %s — refusing verdict-state decision (FAC-145 fail-closed): %w", e.ID, err)
		}
		if cb.DedupeID == deliveredID && !found {
			match, found = cb, true
		}
	}
	return match, found, nil
}

// EffectiveVerdict resolves the CURRENT verdict for (repo, ref, candidate)
// from the durable bus under deterministic supersession: verdict records
// (dedupe id prefix "verdict:", excluding delivery markers) are ordered by
// bus sequence and the LATEST wins — a later REJECTED vetoes an earlier
// APPROVED until a fresh admissible APPROVED lands after it (FAC-145).
// Returns the winning callback and true, or false when no verdict exists.
func (m *Mailbox) EffectiveVerdict(repo, ref, candidate string) (Callback, bool, error) {
	envs, err := m.ReadInbox(CoordinatorInbox)
	if err != nil {
		return Callback{}, false, err
	}
	var best Callback
	found := false
	for _, e := range envs {
		if !isCallbackSubject(e.Subject) {
			continue
		}
		var cb Callback
		if err := json.Unmarshal([]byte(e.Body), &cb); err != nil {
			// FAIL CLOSED: a corrupt body may be the veto that must block
			// approval — never silently drop it from the decision.
			return Callback{}, false, fmt.Errorf("corrupt callback body in envelope %s — refusing verdict decision (FAC-145 fail-closed): %w", e.ID, err)
		}
		// ONLY delivered records are consumable: an intent (or any other
		// callback) can never be read as a verdict.
		if !strings.HasPrefix(cb.DedupeID, verdictDeliveredPrefix) {
			continue
		}
		if cb.Repo != repo || cb.Ref != ref || cb.SHA != candidate {
			continue
		}
		cb.Sequence = e.Sequence
		if !found || cb.Sequence > best.Sequence {
			best = cb
			found = true
		}
	}
	return best, found, nil
}

func isCallbackSubject(subject string) bool {
	return strings.HasPrefix(subject, string(CallbackComplete)+":") ||
		strings.HasPrefix(subject, string(CallbackBlocked)+":")
}
