package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// Explicit operator supersession for the durable routine queue.
//
// The incident this fixes: a coordinator retasked at a boundary consumed an
// OLD queued prompt (long since merged work) ahead of its newer correction,
// because herd send's durable queue is strictly FIFO with no way to retire a
// stale pending payload when a replacement exists. The operator's only
// recoveries were session churn and blind drains, both of which replay MORE
// stale mail. The same thing recurred after a host reboot: two long-lived
// sessions came back and resumed antique assignments while the live
// corrections sat behind them.
//
// SupersedePendingRoutine is the native fix. Under ONE data-flock hold it:
//
//  1. selects the pending queued routine envelopes for the exact recipient,
//     from the exact issuer (sender), in the exact target binding — the
//     opaque identity token covering canonical repository, workspace, agent
//     name and terminal generation that the issuer resolved from the LIVE
//     fleet. Any other subject (authenticated control, callbacks, ordinary
//     report mail) is structurally outside this selection, already-consumed
//     envelopes are never touched, and an envelope with no binding or a
//     foreign binding is never retired;
//
//  2. durably appends the replacement payload FIRST, with the stable queued
//     identity, so a retry of the same supersession is idempotent;
//
//  3. then commits the victims' disposition — the handled ids plus a
//     SupersessionRecord carrying reason, replacement id and issuer — in ONE
//     atomic, fsync'd write. The envelope lines themselves are never
//     rewritten or deleted, so the historical payload survives alongside the
//     reason it stopped being scheduled.
//
// CRASH CONTRACT. The two durable facts are written replacement-first, and
// readers honour a supersession only when its replacement is actually in the
// mailbox (see supersessionCommitted). That leaves exactly two observable
// outcomes, and neither loses deliverable work:
//
//   - interrupted before the disposition commits: every old envelope is
//     still eligible, and the replacement is queued but purely additive.
//     Re-running the identical command converges — the replacement identity
//     is stable, so it cannot be duplicated, and the second run commits the
//     dispositions.
//   - disposition committed: exactly the replacement is eligible, and every
//     victim carries a durable reason.
//
// There is no window in which an envelope is ineligible without a recovered
// replacement, and nothing is ever rolled back, so a concurrent
// acknowledgement's marks can never be clobbered.
type SupersedeOutcome struct {
	// SupersededIDs lists the pending envelopes made ineligible, in mailbox
	// order. Empty when there was nothing pending from this issuer in this
	// binding — the replacement is still queued.
	SupersededIDs []string
	// EnvelopeID is the stable queued identity of the replacement payload.
	EnvelopeID string
	// Idempotent is true when the replacement was already durably queued by
	// an earlier attempt of the same supersession.
	Idempotent bool
}

// SupersedePendingReason is the recorded disposition reason when the caller
// supplies none.
const SupersedePendingReason = "superseded-by-operator-replacement"

// SupersedePendingRoutine retires this issuer's stale pending prompts for one
// exactly-identified live target and queues replacementBody in their place.
//
// binding is mandatory: retiring another session's work requires proof of
// WHICH session, and a recipient name is not that proof. A caller that cannot
// resolve the live target's binding must not call this at all.
func (m *Mailbox) SupersedePendingRoutine(ctx context.Context, sender, recipient, binding, replacementBody, reason string) (*SupersedeOutcome, error) {
	if m == nil {
		return nil, fmt.Errorf("mail: nil mailbox")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sender = strings.TrimSpace(sender)
	recipient = strings.TrimSpace(recipient)
	binding = strings.TrimSpace(binding)
	if sender == "" || recipient == "" {
		return nil, fmt.Errorf("mail: supersession requires sender and recipient")
	}
	// The unbound default is shared by every coordinator that exports no lane
	// identity, so equality on it would let any of them retire any other's
	// queued work. Refusing it here also permanently preserves mail already
	// queued anonymously: no issuer can ever match it.
	if sender == AnonymousIssuer {
		return nil, fmt.Errorf("mail: %q is the shared unbound sender, not an issuer identity; supersession requires a bound coordinator", AnonymousIssuer)
	}
	if binding == "" {
		return nil, fmt.Errorf("mail: supersession requires a resolved target binding; an unbound target cannot authorize retiring queued work")
	}
	if strings.TrimSpace(replacementBody) == "" {
		return nil, fmt.Errorf("mail: supersession requires a non-empty replacement payload")
	}
	if strings.TrimSpace(reason) == "" {
		reason = SupersedePendingReason
	}

	var outcome *SupersedeOutcome
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.withFileLockContext(ctx, func() error {
		outcome = nil
		replID := QueuedEnvelopeID(sender, recipient, binding, replacementBody)

		envs, err := m.readQueueLocked()
		if err != nil {
			return err
		}
		st, err := loadAck(m.MailFile)
		if err != nil {
			return fmt.Errorf("supersession: %w", err)
		}

		present := map[string]*Envelope{}
		for _, env := range envs {
			present[env.ID] = env
		}
		handled := map[string]struct{}{}
		for _, id := range st.Handled[recipient] {
			handled[id] = struct{}{}
		}

		_, replacementDurable := present[replID]
		if _, consumed := handled[replID]; replacementDurable && consumed {
			// The identical payload was already delivered and acknowledged.
			// Re-appending is impossible (the identity is stable) and
			// reporting success would silently drop a genuinely new
			// assignment, so say so instead.
			return fmt.Errorf("supersession: this exact replacement (%s) was already delivered to %s and acknowledged; vary the payload to queue new work", replID, recipient)
		}

		var victims []string
		for _, env := range envs {
			if env.ID == replID {
				continue
			}
			if env.Recipient != recipient || env.Sender != sender || env.Subject != QueuedDeliverySubject {
				continue
			}
			// Identity, not just a name: an unbound (legacy) envelope or one
			// addressed to a different session of the same agent is ambiguous
			// evidence, and ambiguous evidence never retires work.
			if env.Binding == "" || env.Binding != binding {
				continue
			}
			if _, ok := handled[env.ID]; ok && markStandsLocal(st, recipient, env, present) {
				continue
			}
			victims = append(victims, env.ID)
		}

		// Replacement first. Until it is durable nothing is retired, so an
		// interruption here leaves the old work exactly as deliverable as it
		// was.
		if !replacementDurable {
			if err := m.appendReplacementLocked(sender, recipient, binding, replacementBody, replID); err != nil {
				return err
			}
		}

		if len(victims) > 0 {
			applySupersedeMarks(st, recipient, victims, sender, replID, reason)
			if err := saveAck(m.MailFile, st); err != nil {
				return fmt.Errorf("supersession: persist disposition: %w", err)
			}
		}

		outcome = &SupersedeOutcome{
			SupersededIDs: victims,
			EnvelopeID:    replID,
			Idempotent:    replacementDurable,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// readQueueLocked parses every mailbox envelope. Caller holds m.mu and the
// data flock. Malformed lines fail closed: a queue whose contents are partly
// unreadable must not be selectively retired.
func (m *Mailbox) readQueueLocked() ([]*Envelope, error) {
	if _, err := os.Stat(m.MailFile); os.IsNotExist(err) {
		return nil, nil
	}
	data, err := os.ReadFile(m.MailFile)
	if err != nil {
		return nil, fmt.Errorf("supersession: read mailbox: %w", err)
	}
	var out []*Envelope
	for _, line := range splitLines(string(data)) {
		if len(line) == 0 {
			continue
		}
		var env Envelope
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			return nil, fmt.Errorf("supersession: mailbox line is unreadable, refusing a partial retirement: %w", err)
		}
		out = append(out, &env)
	}
	return out, nil
}

// applySupersedeMarks records every victim's disposition in st. Idempotent:
// an id already marked, or already carrying a record, is left alone so a
// resumed attempt cannot rewrite an earlier reason.
func applySupersedeMarks(st *ackState, recipient string, victims []string, issuer, replacementID, reason string) {
	now := time.Now().UTC()
	if st.Superseded == nil {
		st.Superseded = map[string]map[string]SupersessionRecord{}
	}
	if st.Superseded[recipient] == nil {
		st.Superseded[recipient] = map[string]SupersessionRecord{}
	}
	for _, id := range victims {
		markHandledLocked(st, recipient, id)
		if _, exists := st.Superseded[recipient][id]; !exists {
			st.Superseded[recipient][id] = SupersessionRecord{
				SupersededAt: now, Reason: reason, ReplacementID: replacementID, Issuer: issuer,
			}
		}
	}
}

// appendReplacementLocked durably appends the replacement payload with the
// stable queued identity. Caller holds m.mu and the data flock.
func (m *Mailbox) appendReplacementLocked(sender, recipient, binding, body, id string) error {
	seq, err := m.nextSequenceLocked()
	if err != nil {
		return err
	}
	env := &Envelope{
		ID: id, Sequence: seq, Sender: sender, Recipient: recipient,
		Subject: QueuedDeliverySubject, Body: body, Binding: binding,
		Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("supersession replacement: %w", err)
	}
	if err := appendLine(m.MailFile, data); err != nil {
		return fmt.Errorf("supersession replacement: %w", err)
	}
	m.markSeenLocked(id)
	return nil
}
