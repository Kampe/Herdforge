package lifecycle

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/Kampe/Herdforge/pkg/outbox"
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
	// ErrConcurrentModification is returned when another transaction (in
	// this process or another) committed a transition for the same task
	// between this attempt's read and its write. The caller must retry
	// with a fresh Machine.Transition call — never resume this one, since
	// whatever it decided (FromState, seq) was based on stale data.
	ErrConcurrentModification = errors.New("lifecycle: concurrent modification detected")
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
	// Herdr dispatch, Git operation) can never be lost or duplicated. They
	// are enqueued on BOTH a fresh transition and a replay, using the
	// exact same TaskRef-fill logic either way (see the single loop
	// below) — a retried caller and a first-time caller enqueue
	// byte-identical items, so outbox.Store's own idempotency dedup is
	// what makes the replay a no-op, not a second code path here.
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

// NewMachine opens (or creates) a SQLite database at path, applies the
// SQLite concurrency contract (see sqliteConcurrencyContract), and
// applies both the lifecycle and outbox schemas to it.
func NewMachine(path string) (*Machine, error) {
	db, err := openSQLite(path)
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

// Transition validates and durably applies one state change in a single
// SQLite transaction: EventStore.AppendTx decides FromState, sequence,
// lease fencing, and transition legality from data it reads inside that
// same transaction (never from anything Machine computed beforehand), then
// this method enqueues any outbox side effects in the same transaction
// before committing.
//
// The in-process mutex below serializes same-process callers as an
// optimization (it avoids wasted transaction attempts under local
// contention) — it is NOT what makes this safe. Correctness against
// concurrent callers, including a second Machine on the same database
// file from another process, comes entirely from AppendTx's tx-scoped
// reads plus the UNIQUE/CAS guards described there. Acquiring the actual
// right to attempt a transition for a given task across processes is
// FAC-120's lease/fencing responsibility, via the LeaseGeneration this
// method fences on.
//
// On ErrConcurrentModification, ErrStaleLeaseGeneration, or
// ErrInvalidTransition, nothing was durably changed — callers should
// re-read current state and retry with a fresh request rather than
// resuming this one.
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

	tx, err := m.db.Begin()
	if err != nil {
		return TransitionResult{}, fmt.Errorf("transition: begin tx: %w", err)
	}
	defer tx.Rollback()

	appended, err := m.events.AppendTx(tx, AppendIntent{
		TaskRef:          req.TaskRef,
		Repo:             req.Repo,
		To:               req.To,
		Actor:            req.Actor,
		IdempotencyKey:   req.IdempotencyKey,
		LeaseGeneration:  req.LeaseGeneration,
		ProviderRevision: req.ProviderRevision,
		Branch:           req.Branch,
		CandidateSHA:     req.CandidateSHA,
		EvidenceDigest:   req.EvidenceDigest,
		Payload:          req.Payload,
	})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("transition: append event: %w", err)
	}

	// Enqueue outbox side effects on BOTH a fresh append and a replay —
	// one code path, so a retried caller's items are filled in exactly
	// like the first attempt's. outbox.Store's own idempotency dedup
	// makes the replay a no-op instead of a duplicate side effect.
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

	return TransitionResult{Event: appended.Event, Replayed: appended.Replayed}, nil
}
