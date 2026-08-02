package lifecycle

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/Kampe/Herdforge/pkg/outbox"

	_ "modernc.org/sqlite"
)

var (
	// ErrInvalidTransition is returned when the requested target state is
	// not reachable from the task's current durable state.
	ErrInvalidTransition = errors.New("lifecycle: invalid state transition")
	// ErrStaleLeaseGeneration is returned when a caller presents a lease
	// generation older than the one already recorded for the task. This is
	// the fencing guarantee FAC-120's lease manager relies on: a stale
	// generation can never move a task forward, mutate the board, dispatch,
	// verify, review, or merge.
	ErrStaleLeaseGeneration = errors.New("lifecycle: stale lease generation")
	// ErrIdempotencyKeyConflict is returned when an idempotency key is
	// reused for a transition whose target state differs from the one
	// originally recorded under that key.
	ErrIdempotencyKeyConflict = errors.New("lifecycle: idempotency key reused for a different transition")
)

// TransitionRequest describes one attempted state change. Every mutating
// verb (pulse, dispatch, daemon, forge, review, approve, harvest, cleanup)
// calls Machine.Transition with one of these instead of writing its own
// command-local shortcut.
type TransitionRequest struct {
	TaskRef string
	Repo    string
	To      State
	Actor   string
	// IdempotencyKey makes replaying the same command a no-op. Required —
	// fail-closed.
	IdempotencyKey string
	// LeaseGeneration is the generation the caller currently holds for this
	// task (see FAC-120). A generation older than the task's recorded
	// generation is rejected.
	LeaseGeneration  int64
	ProviderRevision string
	Branch           string
	CandidateSHA     string
	EvidenceDigest   string
	Payload          string
	// OutboxItems are enqueued in the SAME transaction as the event append,
	// so the durable intent to perform a side effect (provider mutation,
	// Herdr dispatch, Git operation) can never be lost or duplicated.
	OutboxItems []outbox.Item
}

// TransitionResult is what Machine.Transition returns.
type TransitionResult struct {
	Event Event
	// Replayed is true when IdempotencyKey had already been recorded and
	// no new durable state change occurred.
	Replayed bool
}

// Machine ties the event store and the transactional outbox together
// behind one atomic operation: validate, append, enqueue side effects,
// commit.
type Machine struct {
	mu     sync.Mutex
	db     *sql.DB
	events *EventStore
	out    *outbox.Store
}

// NewMachine opens (or creates) a SQLite database at path and applies both
// the lifecycle and outbox schemas to it.
func NewMachine(path string) (*Machine, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open lifecycle machine store: %w", err)
	}
	return NewMachineWithDB(db)
}

// NewMachineWithDB wraps an already-open *sql.DB.
func NewMachineWithDB(db *sql.DB) (*Machine, error) {
	events, err := NewEventStoreWithDB(db)
	if err != nil {
		return nil, err
	}
	out, err := outbox.NewStoreWithDB(db)
	if err != nil {
		return nil, err
	}
	return &Machine{db: db, events: events, out: out}, nil
}

// EventStore returns the underlying event store (read model + history).
func (m *Machine) EventStore() *EventStore { return m.events }

// Outbox returns the underlying transactional outbox (for a Relay to
// drain, or for tests/inspection).
func (m *Machine) Outbox() *outbox.Store { return m.out }

func (m *Machine) Close() error {
	return m.db.Close()
}

// Transition validates and durably applies one state change. It is safe
// to call concurrently: transitions for the whole Machine are serialized,
// and replays (matching IdempotencyKey) are idempotent no-ops.
//
// Cross-process mutual exclusion over WHO may attempt a transition for a
// given task is FAC-120's lease/fencing responsibility; Machine only
// guarantees that whatever is durably recorded is atomic, ordered, and
// never silently duplicated or skipped.
func (m *Machine) Transition(req TransitionRequest) (TransitionResult, error) {
	if req.TaskRef == "" {
		return TransitionResult{}, fmt.Errorf("transition: task_ref is required")
	}
	if req.IdempotencyKey == "" {
		return TransitionResult{}, fmt.Errorf("transition: idempotency_key is required (fail-closed)")
	}
	if req.Actor == "" {
		return TransitionResult{}, fmt.Errorf("transition: actor is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, err := m.events.EventByIdempotencyKey(req.IdempotencyKey); err != nil {
		return TransitionResult{}, err
	} else if existing != nil {
		if existing.TaskRef != req.TaskRef || existing.ToState != req.To {
			return TransitionResult{}, fmt.Errorf("%w: key=%s", ErrIdempotencyKeyConflict, req.IdempotencyKey)
		}
		if err := m.enqueueOutboxOnly(req.OutboxItems); err != nil {
			return TransitionResult{}, err
		}
		return TransitionResult{Event: *existing, Replayed: true}, nil
	}

	current, err := m.events.CurrentState(req.TaskRef)
	if err != nil {
		return TransitionResult{}, err
	}
	from := StateDraft
	if current != nil {
		from = current.State
		if req.LeaseGeneration < current.LeaseGeneration {
			return TransitionResult{}, fmt.Errorf("%w: task=%s held=%d got=%d",
				ErrStaleLeaseGeneration, req.TaskRef, current.LeaseGeneration, req.LeaseGeneration)
		}
	}
	if !ValidTransition(from, req.To) {
		return TransitionResult{}, fmt.Errorf("%w: task=%s %s -> %s", ErrInvalidTransition, req.TaskRef, from, req.To)
	}

	tx, err := m.db.Begin()
	if err != nil {
		return TransitionResult{}, fmt.Errorf("transition: begin tx: %w", err)
	}
	defer tx.Rollback()

	ev, err := m.events.AppendTx(tx, Event{
		TaskRef:          req.TaskRef,
		Repo:             req.Repo,
		FromState:        from,
		ToState:          req.To,
		ProviderRevision: req.ProviderRevision,
		LeaseGeneration:  req.LeaseGeneration,
		Branch:           req.Branch,
		CandidateSHA:     req.CandidateSHA,
		Actor:            req.Actor,
		EvidenceDigest:   req.EvidenceDigest,
		Payload:          req.Payload,
		IdempotencyKey:   req.IdempotencyKey,
	})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("transition: append event: %w", err)
	}

	for _, item := range req.OutboxItems {
		if item.TaskRef == "" {
			item.TaskRef = req.TaskRef
		}
		if _, err := m.out.EnqueueTx(tx, item); err != nil {
			return TransitionResult{}, fmt.Errorf("transition: enqueue outbox item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return TransitionResult{}, fmt.Errorf("transition: commit: %w", err)
	}

	return TransitionResult{Event: ev}, nil
}

// enqueueOutboxOnly is used on the replay path: the event itself already
// exists, but outbox items carry their own idempotency keys, so replaying
// them is still safe and ensures a side effect that was decided but never
// durably enqueued (e.g. a crash between event commit and outbox enqueue
// in some earlier version) still gets recorded.
func (m *Machine) enqueueOutboxOnly(items []outbox.Item) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := m.db.Begin()
	if err != nil {
		return fmt.Errorf("enqueue outbox on replay: begin tx: %w", err)
	}
	defer tx.Rollback()
	for _, item := range items {
		if _, err := m.out.EnqueueTx(tx, item); err != nil {
			return fmt.Errorf("enqueue outbox on replay: %w", err)
		}
	}
	return tx.Commit()
}
