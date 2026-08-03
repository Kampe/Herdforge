package mail

import (
	"context"
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
//
// FAC-162: all mutations are copy-on-write. c.state is published only after
// a durable write under the mailbox flock succeeds. On any lock/write
// failure the live process keeps the prior state (same as disk).
// Cross-process writers reload disk state inside the flock transaction so
// concurrent consumers cannot clobber each other with lost updates.
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
		state:      emptyConsumerState(),
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

func emptyConsumerState() consumerState {
	return consumerState{
		Acked:   map[string]ackMark{},
		Pending: map[string]*pendingCallback{},
	}
}

func ackKey(repo, ref string) string {
	if repo == "" {
		return ref
	}
	return repo + "|" + ref
}

func cloneConsumerState(st consumerState) consumerState {
	out := emptyConsumerState()
	for k, v := range st.Acked {
		out.Acked[k] = v
	}
	for k, p := range st.Pending {
		if p == nil {
			continue
		}
		cp := *p
		out.Pending[k] = &cp
	}
	return out
}

func (c *CallbackConsumer) load() error {
	st, err := c.readStateFile()
	if err != nil {
		return err
	}
	c.state = st
	return nil
}

// readStateFile loads durable state without mutating c.state.
func (c *CallbackConsumer) readStateFile() (consumerState, error) {
	data, err := os.ReadFile(c.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyConsumerState(), nil
		}
		return consumerState{}, fmt.Errorf("failed to read callback consumer state: %w", err)
	}
	var st consumerState
	if err := json.Unmarshal(data, &st); err != nil {
		return consumerState{}, fmt.Errorf("corrupt callback consumer state: %w", err)
	}
	if st.Acked == nil {
		st.Acked = map[string]ackMark{}
	}
	if st.Pending == nil {
		st.Pending = map[string]*pendingCallback{}
	}
	return st, nil
}

// applyDrainTo mutates candidate only; returns drain results and dead-letter set.
func (c *CallbackConsumer) applyDrainTo(candidate *consumerState, envs []*Envelope) (out []DrainedCallback, deadLettered []Callback, settledIDs []string) {
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
		if mark, acked := candidate.Acked[key]; acked {
			if cb.LeaseGeneration < mark.LeaseGeneration {
				continue
			}
			if cb.LeaseGeneration == mark.LeaseGeneration && cb.Sequence <= mark.Sequence {
				continue
			}
		}

		pending, retried := candidate.Pending[e.ID]
		if !retried {
			pending = &pendingCallback{EnvelopeID: e.ID, Sequence: e.Sequence, FirstSeen: time.Now()}
			candidate.Pending[e.ID] = pending
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
	return out, deadLettered, settledIDs
}

func applyAckTo(candidate *consumerState, envelopeID string) error {
	pending, ok := candidate.Pending[envelopeID]
	if !ok {
		return fmt.Errorf("ack: unknown or already-settled envelope %s", envelopeID)
	}
	key := ackKey(pending.Callback.Repo, pending.Callback.Ref)
	mark := candidate.Acked[key]
	if pending.Callback.LeaseGeneration > mark.LeaseGeneration ||
		(pending.Callback.LeaseGeneration == mark.LeaseGeneration && pending.Sequence > mark.Sequence) {
		candidate.Acked[key] = ackMark{LeaseGeneration: pending.Callback.LeaseGeneration, Sequence: pending.Sequence}
	}
	delete(candidate.Pending, envelopeID)
	return nil
}

// writeDeadLettersLocked appends dead letters; caller must hold mailbox data flock.
func (c *CallbackConsumer) writeDeadLettersLocked(cbs []Callback) error {
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
}

// Drain returns every not-yet-acknowledged, not-stale callback currently in
// the coordinator inbox, tracking each as pending until Ack is called.
func (c *CallbackConsumer) Drain() ([]DrainedCallback, error) {
	return c.DrainContext(context.Background())
}

// DrainContext is Drain with deadline inheritance on ReadInbox, dead-letter
// append, and durable state save (all mailbox flock consumers).
//
// State is transactional: c.state is unchanged unless the durable write under
// the mailbox lock succeeds. Disk is reloaded inside the lock so concurrent
// consumers cannot lose updates.
func (c *CallbackConsumer) DrainContext(ctx context.Context) ([]DrainedCallback, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	envs, err := c.mb.ReadInboxContext(ctx, CoordinatorInbox)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var committed consumerState
	var out []DrainedCallback

	err = c.mb.withFileLockContext(ctx, func() error {
		// Authoritative base is disk under the flock (cross-process safe).
		base, rerr := c.readStateFile()
		if rerr != nil {
			return rerr
		}
		// Same-process pending not yet visible on a concurrent handle is on
		// disk after prior commits; memory is not merged over disk so a
		// second process never reintroduces stale acks.
		candidate := cloneConsumerState(base)
		var deadLettered []Callback
		var settledIDs []string
		out, deadLettered, settledIDs = c.applyDrainTo(&candidate, envs)

		// Dead-letter before forgetting pending: same CS as state write so
		// we never publish a state that dropped pending without a DL record.
		// On DL failure the whole transaction aborts; c.state stays prior.
		if len(deadLettered) > 0 {
			if dlErr := c.writeDeadLettersLocked(deadLettered); dlErr != nil {
				return dlErr
			}
			for _, id := range settledIDs {
				delete(candidate.Pending, id)
			}
		}

		data, merr := json.Marshal(candidate)
		if merr != nil {
			return fmt.Errorf("failed to marshal callback consumer state: %w", merr)
		}
		if werr := writeFileAtomic(c.statePath, data, 0644); werr != nil {
			return werr
		}
		committed = candidate
		return nil
	})
	if err != nil {
		// Fail-closed: live state must match last durable commit.
		return nil, err
	}
	c.state = committed
	return out, nil
}

// Ack acknowledges the callback identified by envelopeID.
func (c *CallbackConsumer) Ack(envelopeID string) error {
	return c.AckContext(context.Background(), envelopeID)
}

// AckContext is Ack with deadline inheritance on durable state save.
// Memory is published only after a successful flock + write of reloaded+acked state.
func (c *CallbackConsumer) AckContext(ctx context.Context, envelopeID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var committed consumerState
	err := c.mb.withFileLockContext(ctx, func() error {
		base, rerr := c.readStateFile()
		if rerr != nil {
			return rerr
		}
		candidate := cloneConsumerState(base)
		if aerr := applyAckTo(&candidate, envelopeID); aerr != nil {
			return aerr
		}
		data, merr := json.Marshal(candidate)
		if merr != nil {
			return fmt.Errorf("failed to marshal callback consumer state: %w", merr)
		}
		if werr := writeFileAtomic(c.statePath, data, 0644); werr != nil {
			return werr
		}
		committed = candidate
		return nil
	})
	if err != nil {
		return err
	}
	c.state = committed
	return nil
}

// appendDeadLetters is retained for tests/helpers; production Drain writes
// dead letters inside the same flock transaction as state.
func (c *CallbackConsumer) appendDeadLetters(cbs []Callback) error {
	return c.appendDeadLettersContext(context.Background(), cbs)
}

func (c *CallbackConsumer) appendDeadLettersContext(ctx context.Context, cbs []Callback) error {
	return c.mb.withFileLockContext(ctx, func() error {
		return c.writeDeadLettersLocked(cbs)
	})
}

// Stats reports pending count, queue age, retries, dead letters, and the
// highest sequence number acknowledged so far.
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
