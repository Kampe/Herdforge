package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FAC-569: Envelope.Read exists but nothing ever set it, so a "pending" filter
// on !env.Read matched everything forever. A queue whose acknowledgement is a
// field nobody writes is not a queue -- it is a log that looks like one.
//
// The mailbox is append-only, so handled state lives beside it in a small
// side-file, the same shape the callback consumer already uses for its own
// progress. One definition, so "handled" cannot come to mean two things.

// HandledStatePath is the handled-state file for a mailbox.
//
// Exported because callers need to DISPLAY it: the handoffs command printed the
// suffix itself, which duplicated this rule and was caught by the duplicate-rule
// gate. A path only this package knows how to build should only be built here.
func HandledStatePath(mailFile string) string {
	return mailFile + ".handled.json"
}

func ackPath(mailFile string) string { return HandledStatePath(mailFile) }

type ackState struct {
	// Handled maps recipient -> envelope ids that reached a disposition.
	Handled map[string][]string `json:"handled"`
	// Superseded maps recipient -> envelope id -> the supersession record
	// that made the envelope ineligible. The envelope line itself is never
	// rewritten or deleted: the durable disposition lives beside it, so the
	// historical payload and the reason it stopped being scheduled are both
	// preserved evidence.
	Superseded map[string]map[string]SupersessionRecord `json:"superseded,omitempty"`
}

// SupersessionRecord is the durable reason one queued envelope was made
// ineligible by an explicit operator supersession.
type SupersessionRecord struct {
	SupersededAt  time.Time `json:"superseded_at"`
	Reason        string    `json:"reason"`
	ReplacementID string    `json:"replacement_id"`
	Issuer        string    `json:"issuer"`
}

func loadAck(mailFile string) (*ackState, error) {
	data, err := os.ReadFile(ackPath(mailFile))
	if err != nil {
		if os.IsNotExist(err) {
			return &ackState{Handled: map[string][]string{}}, nil
		}
		return nil, err
	}
	var st ackState
	if err := json.Unmarshal(data, &st); err != nil {
		// Fail closed: a corrupt ack file must not read as "nothing handled"
		// and re-deliver settled work, nor as "all handled" and drop live work.
		return nil, fmt.Errorf("parse handled state: %w", err)
	}
	if st.Handled == nil {
		st.Handled = map[string][]string{}
	}
	return &st, nil
}

// saveAck durably and atomically replaces the handled-state file. Callers
// MUST hold the mailbox data flock: this is a read-modify-write of shared
// state, and the flock is what serializes it across processes.
//
// The earlier implementation renamed a temp file into place without fsyncing
// either the temp file or the directory, so an atomic-looking rename could
// still evaporate in a crash. A disposition that does not survive a crash is
// not a disposition, so this goes through writeFileAtomic, the same durable
// primitive the sequence counter and dead-letter state already use.
func saveAck(mailFile string, st *ackState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	path := ackPath(mailFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// MarkHandled records that one envelope reached a disposition.
//
// This is deliberately NOT "mark read". Reading a handoff is not finishing it,
// and conflating the two is how a queue silently drains itself: an entry must
// stay pending until its work has an outcome.
func (m *Mailbox) MarkHandled(recipient, id string) error {
	return m.MarkHandledContext(context.Background(), recipient, id)
}

// MarkHandledContext is MarkHandled with deadline inheritance for lock
// acquisition.
//
// The handled sidecar is shared state and this is a read-modify-write of it.
// It used to run under the per-instance mutex alone, which serializes nothing
// across processes: two Mailbox instances acknowledging different envelopes
// concurrently could each load the same state, add their own id, and write --
// and the loser's acknowledgement silently vanished, re-delivering settled
// work. Every disposition writer now takes the same cross-process data flock,
// so "handled" means the same thing to all of them.
//
// Callers must not already hold m.mu or the data flock; the mailbox lock is a
// ticketed queue, not a reentrant one. In-package callers that already hold
// both use markHandledLocked instead.
func (m *Mailbox) MarkHandledContext(ctx context.Context, recipient, id string) error {
	if m == nil {
		return fmt.Errorf("mail: nil mailbox")
	}
	recipient, id = strings.TrimSpace(recipient), strings.TrimSpace(id)
	if recipient == "" || id == "" {
		return ErrRecipientAndEnvelopeIDRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.withFileLockContext(ctx, func() error {
		st, err := loadAck(m.MailFile)
		if err != nil {
			return err
		}
		if !markHandledLocked(st, recipient, id) {
			return nil // idempotent
		}
		return saveAck(m.MailFile, st)
	})
}

// markHandledLocked adds id to recipient's handled set in st, reporting
// whether it was newly added. Caller holds m.mu and the data flock and owns
// persisting st.
func markHandledLocked(st *ackState, recipient, id string) bool {
	for _, known := range st.Handled[recipient] {
		if known == id {
			return false
		}
	}
	st.Handled[recipient] = append(st.Handled[recipient], id)
	sort.Strings(st.Handled[recipient])
	return true
}

// Handled reports whether an envelope already reached a disposition.
//
// This is the one chokepoint every read and delivery path asks, so the
// supersession commit gate lives HERE rather than in each caller: a mark that
// came from an incomplete supersession must read as "still pending"
// everywhere, not only on the drain. See supersessionHonoured.
func (m *Mailbox) Handled(recipient, id string) (bool, error) {
	recipient, id = strings.TrimSpace(recipient), strings.TrimSpace(id)
	st, err := loadAck(m.MailFile)
	if err != nil {
		return false, err
	}
	marked := false
	for _, known := range st.Handled[recipient] {
		if known == id {
			marked = true
			break
		}
	}
	if !marked {
		return false, nil
	}
	rec, superseded := st.Superseded[recipient][id]
	if !superseded {
		return true, nil // an ordinary acknowledgement always stands
	}
	// Only an envelope that a supersession actually touched pays for the
	// mailbox read; plain acknowledgements stay a single ack-file load.
	envs, err := m.ReadInbox(recipient)
	if err != nil {
		return false, err
	}
	var victim, replacement *Envelope
	for _, env := range envs {
		if env == nil {
			continue
		}
		if env.ID == id {
			victim = env
		}
		if env.ID == rec.ReplacementID {
			replacement = env
		}
	}
	return supersessionHonoured(rec, victim, replacement), nil
}

// supersessionHonoured is the single definition of "this supersession
// actually committed", asked by every disposition reader.
//
// An id match alone is not proof. The replacement has to BE what the record
// claims: the same durable queued class, addressed to the same recipient, from
// the same issuer that retired the work, and carrying the same target session
// binding as the envelope it replaced. Anything else — a missing replacement
// from an interrupted commit, a record pointing at an ordinary report or a
// control envelope, a replacement for some other session — describes a
// supersession that never completed, so the retired envelope stays
// deliverable and nothing is lost.
func supersessionHonoured(rec SupersessionRecord, victim, replacement *Envelope) bool {
	if victim == nil || replacement == nil || rec.ReplacementID == "" {
		return false
	}
	if replacement.ID != rec.ReplacementID || replacement.Subject != QueuedDeliverySubject {
		return false
	}
	if replacement.Recipient != victim.Recipient {
		return false
	}
	if rec.Issuer == "" || replacement.Sender != rec.Issuer || victim.Sender != rec.Issuer {
		return false
	}
	return replacement.Binding != "" && replacement.Binding == victim.Binding
}
