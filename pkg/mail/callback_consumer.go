package mail

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// DefaultMaxCallbackRetries bounds how many times an unacknowledged
// callback is redelivered by Drain before it is moved to the dead-letter
// file and dropped from all future delivery.
const DefaultMaxCallbackRetries = 5

// DrainedCallback is one callback handed to a consumer, bound to the
// envelope and sequence it arrived on so the caller can Ack it precisely.
type DrainedCallback struct {
	EnvelopeID string   `json:"envelope_id"`
	Sequence   int64    `json:"sequence"`
	Attempt    int      `json:"attempt"`
	Callback   Callback `json:"callback"`
}

// ackMark is the high-water (lease generation, sequence) already
// acknowledged for one (repo, ref) key. A callback at or below this mark
// can never advance state again — this is what makes redelivery of an
// already-acked message, or a callback from a superseded lease generation,
// inert instead of a duplicate state transition.
type ackMark struct {
	LeaseGeneration int64 `json:"lease_generation"`
	Sequence        int64 `json:"sequence"`
}

type pendingCallback struct {
	EnvelopeID string    `json:"envelope_id"`
	Sequence   int64     `json:"sequence"`
	Callback   Callback  `json:"callback"`
	Attempts   int       `json:"attempts"`
	FirstSeen  time.Time `json:"first_seen"`
}

type consumerState struct {
	Acked   map[string]ackMark          `json:"acked"`
	Pending map[string]*pendingCallback `json:"pending"`
}

// CallbackConsumer durably drains the coordinator inbox with
// acknowledgement, retry, dedupe, and dead-letter semantics. State
// (pending deliveries and acked high-water marks) is persisted to
// <MailFile>.callback-state.json under the mailbox's own file lock, so a
// crash and restart resumes exactly where it left off: an unacked callback
// is retried, not lost or re-processed as new; an acked one never comes
// back; a callback whose lease generation the ref has already moved past
// can never advance state.
type CallbackConsumer struct {
	mb         *Mailbox
	statePath  string
	deadPath   string
	maxRetries int

	mu    sync.Mutex
	state consumerState
}

// NewCallbackConsumer loads (or initializes) durable consumer state for mb.
// maxRetries <= 0 uses DefaultMaxCallbackRetries.
func NewCallbackConsumer(mb *Mailbox, maxRetries int) (*CallbackConsumer, error) {
	if maxRetries <= 0 {
		maxRetries = DefaultMaxCallbackRetries
	}
	c := &CallbackConsumer{
		mb:         mb,
		statePath:  mb.MailFile + ".callback-state.json",
		deadPath:   mb.MailFile + ".dead-letters.jsonl",
		maxRetries: maxRetries,
		state: consumerState{
			Acked:   map[string]ackMark{},
			Pending: map[string]*pendingCallback{},
		},
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func ackKey(repo, ref string) string {
	if repo == "" {
		return ref
	}
	return repo + "|" + ref
}

func (c *CallbackConsumer) load() error {
	data, err := os.ReadFile(c.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read callback consumer state: %w", err)
	}
	var st consumerState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("corrupt callback consumer state %s: %w", c.statePath, err)
	}
	if st.Acked == nil {
		st.Acked = map[string]ackMark{}
	}
	if st.Pending == nil {
		st.Pending = map[string]*pendingCallback{}
	}
	c.state = st
	return nil
}

// saveLocked persists state under the mailbox's cross-process file lock.
// Caller must hold c.mu.
func (c *CallbackConsumer) saveLocked() error {
	data, err := json.Marshal(c.state)
	if err != nil {
		return fmt.Errorf("failed to marshal callback consumer state: %w", err)
	}
	return c.mb.withFileLock(func() error {
		return writeFileAtomic(c.statePath, data, 0644)
	})
}

// Drain returns every not-yet-acknowledged, not-stale callback currently in
// the coordinator inbox, tracking each as pending until Ack is called.
//
//   - A callback whose (repo, ref) is already acked at an equal-or-higher
//     lease generation/sequence is a stale or duplicate redelivery: it is
//     skipped entirely and never returned, so it can never advance state.
//   - A callback delivered again before being acked (crash, restart, Redis
//     redelivery) is returned again with Attempt incremented, so the caller
//     can see it's a retry rather than treating it as fresh work.
//   - A callback that exceeds maxRetries without ever being acked is moved
//     to the dead-letter file and permanently dropped — never silently
//     lost, never retried forever.
func (c *CallbackConsumer) Drain() ([]DrainedCallback, error) {
	envs, err := c.mb.ReadInbox(CoordinatorInbox)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var out []DrainedCallback
	var deadLettered []Callback
	var settledIDs []string

	for _, e := range envs {
		if !isCallbackSubject(e.Subject) {
			continue
		}
		var cb Callback
		if json.Unmarshal([]byte(e.Body), &cb) != nil || cb.Ref == "" {
			continue
		}
		cb.Sequence = e.Sequence

		key := ackKey(cb.Repo, cb.Ref)
		if mark, acked := c.state.Acked[key]; acked {
			if cb.LeaseGeneration < mark.LeaseGeneration {
				continue // stale lease generation: can never advance state
			}
			if cb.LeaseGeneration == mark.LeaseGeneration && cb.Sequence <= mark.Sequence {
				continue // already-processed duplicate redelivery
			}
		}

		pending, retried := c.state.Pending[e.ID]
		if !retried {
			pending = &pendingCallback{EnvelopeID: e.ID, Sequence: e.Sequence, FirstSeen: time.Now()}
			c.state.Pending[e.ID] = pending
		}
		pending.Callback = cb
		pending.Attempts++

		if pending.Attempts > c.maxRetries {
			deadLettered = append(deadLettered, cb)
			settledIDs = append(settledIDs, e.ID)
			continue
		}

		out = append(out, DrainedCallback{
			EnvelopeID: e.ID,
			Sequence:   e.Sequence,
			Attempt:    pending.Attempts,
			Callback:   cb,
		})
	}

	// Order matters for crash consistency: the durable dead-letter record
	// must land before the pending entry is ever forgotten. If we saved
	// state (clearing pending) first and then crashed or failed before the
	// dead-letter append, the callback would vanish with no record of it
	// anywhere — and worse, the next Drain would treat the still-in-the-
	// inbox envelope as brand new and reset its attempt count, silently
	// defeating maxRetries. Writing the dead-letter record first means the
	// worst a crash here can do is a duplicate dead-letter entry on retry,
	// never a silent loss or a reset counter.
	if len(deadLettered) > 0 {
		if err := c.appendDeadLetters(deadLettered); err != nil {
			return out, err
		}
		for _, id := range settledIDs {
			delete(c.state.Pending, id)
		}
	}

	if err := c.saveLocked(); err != nil {
		return nil, err
	}
	return out, nil
}

// Ack acknowledges the callback identified by envelopeID, advancing its
// (repo, ref) high-water mark so nothing at or below this lease
// generation/sequence is ever delivered again. Acking an envelope whose
// generation/sequence doesn't exceed the current mark (a stale retry that
// slipped through as pending before a fresher one was acked) is a no-op on
// the mark — it still clears the pending entry, but can never regress state.
func (c *CallbackConsumer) Ack(envelopeID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	pending, ok := c.state.Pending[envelopeID]
	if !ok {
		return fmt.Errorf("ack: unknown or already-settled envelope %s", envelopeID)
	}

	key := ackKey(pending.Callback.Repo, pending.Callback.Ref)
	mark := c.state.Acked[key]
	if pending.Callback.LeaseGeneration > mark.LeaseGeneration ||
		(pending.Callback.LeaseGeneration == mark.LeaseGeneration && pending.Sequence > mark.Sequence) {
		c.state.Acked[key] = ackMark{LeaseGeneration: pending.Callback.LeaseGeneration, Sequence: pending.Sequence}
	}
	delete(c.state.Pending, envelopeID)
	return c.saveLocked()
}

// appendDeadLetters durably (fsync'd, one write per record) appends every
// cb to the dead-letter file under the mailbox's cross-process lock. A
// marshal failure aborts and propagates instead of silently skipping that
// record — a dead-lettered callback that can't even be written down is a
// fail-closed condition, not something to swallow.
func (c *CallbackConsumer) appendDeadLetters(cbs []Callback) error {
	return c.mb.withFileLock(func() error {
		for _, cb := range cbs {
			data, err := json.Marshal(cb)
			if err != nil {
				return fmt.Errorf("failed to marshal dead-lettered callback %s: %w", cb.Ref, err)
			}
			if err := appendLine(c.deadPath, data); err != nil {
				return err
			}
		}
		return nil
	})
}

// Stats reports pending count, queue age (how long the oldest unacked
// callback has been waiting), retry count, dead letters, and the highest
// sequence number acknowledged so far — the observability surface FAC-126
// requires on top of the durability guarantees above.
type Stats struct {
	PendingCount    int
	QueueAge        time.Duration
	Retries         int
	DeadLetters     int
	LastConsumedSeq int64
	Quarantined     int
}

func (c *CallbackConsumer) Stats() (Stats, error) {
	c.mu.Lock()
	var st Stats
	st.PendingCount = len(c.state.Pending)
	var oldest time.Time
	for _, p := range c.state.Pending {
		st.Retries += p.Attempts - 1
		if oldest.IsZero() || p.FirstSeen.Before(oldest) {
			oldest = p.FirstSeen
		}
	}
	if !oldest.IsZero() {
		st.QueueAge = time.Since(oldest)
	}
	for _, mark := range c.state.Acked {
		if mark.Sequence > st.LastConsumedSeq {
			st.LastConsumedSeq = mark.Sequence
		}
	}
	c.mu.Unlock()

	st.Quarantined = c.mb.QuarantineCount()

	deadCount, err := countLines(c.deadPath)
	if err != nil {
		return st, err
	}
	st.DeadLetters = deadCount
	return st, nil
}

func countLines(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, l := range splitLines(string(data)) {
		if l != "" {
			n++
		}
	}
	return n, nil
}
