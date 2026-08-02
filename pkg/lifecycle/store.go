package lifecycle

import (
	"database/sql"
	"fmt"
	"time"
)

// Event is the immutable, append-only record of one task-lifecycle
// transition. The schema intentionally carries everything downstream
// tickets need to bind their own state to a specific durable revision:
// repo, provider revision, lease generation, branch, candidate SHA, actor,
// and an evidence digest.
type Event struct {
	ID               int64  `json:"id"`
	TaskRef          string `json:"task_ref"`
	Repo             string `json:"repo"`
	Seq              int64  `json:"seq"`
	FromState        State  `json:"from_state"`
	ToState          State  `json:"to_state"`
	ProviderRevision string `json:"provider_revision,omitempty"`
	LeaseGeneration  int64  `json:"lease_generation"`
	Branch           string `json:"branch,omitempty"`
	CandidateSHA     string `json:"candidate_sha,omitempty"`
	Actor            string `json:"actor"`
	EvidenceDigest   string `json:"evidence_digest,omitempty"`
	// Payload carries transition-specific evidence (e.g. a FAC-122
	// verification receipt) as opaque JSON.
	Payload        string    `json:"payload,omitempty"`
	IdempotencyKey string    `json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
}

// TaskState is the materialized read model of a task's current position:
// the latest applied event, folded forward.
type TaskState struct {
	TaskRef         string    `json:"task_ref"`
	Repo            string    `json:"repo"`
	State           State     `json:"state"`
	Seq             int64     `json:"seq"`
	LeaseGeneration int64     `json:"lease_generation"`
	Branch          string    `json:"branch,omitempty"`
	CandidateSHA    string    `json:"candidate_sha,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// AppendIntent is what a caller proposes. It deliberately has no
// FromState field: AppendTx alone decides the prior state, by reading it
// from inside the same transaction that will record the new event. A
// caller cannot inject a stale or fabricated FromState.
type AppendIntent struct {
	TaskRef          string
	Repo             string
	To               State
	Actor            string
	IdempotencyKey   string
	LeaseGeneration  int64
	ProviderRevision string
	Branch           string
	CandidateSHA     string
	EvidenceDigest   string
	Payload          string
}

// AppendResult is what AppendTx produces.
type AppendResult struct {
	Event Event
	// Replayed is true when IdempotencyKey had already been durably
	// recorded (visible inside this transaction) and no new row was
	// written.
	Replayed bool
}

// EventStore is the canonical SQLite-backed persistence for lifecycle
// events and their materialized task-state read model.
type EventStore struct {
	db *sql.DB
}

// NewEventStore opens (or creates) a SQLite database at path, applies the
// SQLite concurrency contract (see sqliteConcurrencyContract), and applies
// the lifecycle schema.
func NewEventStore(path string) (*EventStore, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open lifecycle store: %w", err)
	}
	return NewEventStoreWithDB(db)
}

// NewEventStoreWithDB wraps an already-open *sql.DB, applying the
// lifecycle schema to it. Used by Machine so events and outbox items share
// one connection and can be written in a single transaction.
func NewEventStoreWithDB(db *sql.DB) (*EventStore, error) {
	s := &EventStore{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// DB returns the underlying connection so callers (Machine) can compose a
// single transaction across the event store and the outbox store.
func (s *EventStore) DB() *sql.DB {
	return s.db
}

func (s *EventStore) Close() error {
	return s.db.Close()
}

func (s *EventStore) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS lifecycle_task_state (
			task_ref TEXT PRIMARY KEY,
			repo TEXT NOT NULL,
			state TEXT NOT NULL,
			seq INTEGER NOT NULL DEFAULT 0,
			lease_generation INTEGER NOT NULL DEFAULT 0,
			branch TEXT,
			candidate_sha TEXT,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS lifecycle_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_ref TEXT NOT NULL,
			repo TEXT NOT NULL,
			seq INTEGER NOT NULL,
			from_state TEXT NOT NULL,
			to_state TEXT NOT NULL,
			provider_revision TEXT,
			lease_generation INTEGER NOT NULL DEFAULT 0,
			branch TEXT,
			candidate_sha TEXT,
			actor TEXT NOT NULL,
			evidence_digest TEXT,
			payload TEXT,
			idempotency_key TEXT NOT NULL UNIQUE,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(task_ref, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_lifecycle_events_task_ref ON lifecycle_events(task_ref)`,
	}
	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migrate lifecycle schema: %w", err)
		}
	}
	return nil
}

// CurrentState returns the materialized state for a task, or nil (with no
// error) if the task has no recorded events yet. This is a plain,
// non-transactional read for callers that only need an eventually
// consistent view (e.g. the Reconciler's sweep, external inspection). The
// write path (AppendTx) never trusts this — it re-reads inside its own
// transaction.
func (s *EventStore) CurrentState(taskRef string) (*TaskState, error) {
	return s.currentStateQuerier(s.db, taskRef)
}

func (s *EventStore) currentStateQuerier(q querier, taskRef string) (*TaskState, error) {
	row := q.QueryRow(
		`SELECT task_ref, repo, state, seq, lease_generation, branch, candidate_sha, updated_at
		 FROM lifecycle_task_state WHERE task_ref = ?`, taskRef,
	)
	var ts TaskState
	var branch, candidateSHA sql.NullString
	err := row.Scan(&ts.TaskRef, &ts.Repo, &ts.State, &ts.Seq, &ts.LeaseGeneration, &branch, &candidateSHA, &ts.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("current state: %w", err)
	}
	ts.Branch = branch.String
	ts.CandidateSHA = candidateSHA.String
	return &ts, nil
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

// AppendTx is the ONLY path that can create a lifecycle_events row, and it
// is the sole authority for whether one is legal. Everything it decides —
// idempotency replay, FromState, sequence number, lease-generation
// fencing, transition legality — is derived from reads taken inside tx,
// never from caller-supplied state. That closes the race an outside-tx
// check leaves open: two callers (in this process or another) can each
// decide "the current state is X" concurrently and be wrong by the time
// they write.
//
// Two independent guards make a wrong write impossible rather than just
// unlikely:
//   - lifecycle_events has UNIQUE(task_ref, seq): if this transaction's
//     seq (derived from a now-stale read) was already taken by a writer
//     that committed first, the INSERT itself fails.
//   - lifecycle_task_state is updated with a compare-and-swap (`WHERE
//     task_ref = ? AND seq = ?` against the seq this transaction read);
//     zero rows affected means someone else advanced the task first, and
//     AppendTx returns ErrConcurrentModification instead of committing a
//     row whose FromState no longer matches reality.
//
// Both failure modes are caller-visible errors, not silent corruption.
// Callers (Machine.Transition) do not auto-retry; retry means re-running
// Transition so FromState is re-derived from fresh state.
func (s *EventStore) AppendTx(tx *sql.Tx, intent AppendIntent) (AppendResult, error) {
	if intent.TaskRef == "" {
		return AppendResult{}, fmt.Errorf("append event: task_ref is required")
	}
	if intent.IdempotencyKey == "" {
		return AppendResult{}, fmt.Errorf("append event: idempotency_key is required (fail-closed)")
	}
	if intent.Actor == "" {
		return AppendResult{}, fmt.Errorf("append event: actor is required")
	}

	// Idempotency check, INSIDE this transaction: a caller retrying the
	// exact same request sees whatever this transaction can already see.
	if existing, err := s.eventByIdempotencyKeyQuerier(tx, intent.IdempotencyKey); err != nil {
		return AppendResult{}, err
	} else if existing != nil {
		if existing.TaskRef != intent.TaskRef || existing.ToState != intent.To {
			return AppendResult{}, fmt.Errorf("%w: key=%s", ErrIdempotencyKeyConflict, intent.IdempotencyKey)
		}
		return AppendResult{Event: *existing, Replayed: true}, nil
	}

	current, err := s.currentStateQuerier(tx, intent.TaskRef)
	if err != nil {
		return AppendResult{}, err
	}

	from := StateDraft
	prevSeq := int64(0)
	prevLeaseGeneration := int64(0)
	if current != nil {
		from = current.State
		prevSeq = current.Seq
		prevLeaseGeneration = current.LeaseGeneration
	}
	if intent.LeaseGeneration < prevLeaseGeneration {
		return AppendResult{}, fmt.Errorf("%w: task=%s held=%d got=%d",
			ErrStaleLeaseGeneration, intent.TaskRef, prevLeaseGeneration, intent.LeaseGeneration)
	}
	if !ValidTransition(from, intent.To) {
		return AppendResult{}, fmt.Errorf("%w: task=%s %s -> %s", ErrInvalidTransition, intent.TaskRef, from, intent.To)
	}

	ev := Event{
		TaskRef:          intent.TaskRef,
		Repo:             intent.Repo,
		Seq:              prevSeq + 1,
		FromState:        from,
		ToState:          intent.To,
		ProviderRevision: intent.ProviderRevision,
		LeaseGeneration:  intent.LeaseGeneration,
		Branch:           intent.Branch,
		CandidateSHA:     intent.CandidateSHA,
		Actor:            intent.Actor,
		EvidenceDigest:   intent.EvidenceDigest,
		Payload:          intent.Payload,
		IdempotencyKey:   intent.IdempotencyKey,
		CreatedAt:        time.Now().UTC(),
	}

	res, err := tx.Exec(
		`INSERT INTO lifecycle_events (
			task_ref, repo, seq, from_state, to_state, provider_revision,
			lease_generation, branch, candidate_sha, actor, evidence_digest,
			payload, idempotency_key, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.TaskRef, ev.Repo, ev.Seq, string(ev.FromState), string(ev.ToState), ev.ProviderRevision,
		ev.LeaseGeneration, ev.Branch, ev.CandidateSHA, ev.Actor, ev.EvidenceDigest,
		ev.Payload, ev.IdempotencyKey, ev.CreatedAt,
	)
	if err != nil {
		return AppendResult{}, fmt.Errorf("%w: insert lifecycle event: %v", ErrConcurrentModification, err)
	}
	ev.ID, _ = res.LastInsertId()

	var cas sql.Result
	if current == nil {
		cas, err = tx.Exec(
			`INSERT INTO lifecycle_task_state (task_ref, repo, state, seq, lease_generation, branch, candidate_sha, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ev.TaskRef, ev.Repo, string(ev.ToState), ev.Seq, ev.LeaseGeneration, ev.Branch, ev.CandidateSHA, ev.CreatedAt,
		)
	} else {
		cas, err = tx.Exec(
			`UPDATE lifecycle_task_state SET
				repo = ?, state = ?, seq = ?, lease_generation = ?, branch = ?, candidate_sha = ?, updated_at = ?
			 WHERE task_ref = ? AND seq = ?`,
			ev.Repo, string(ev.ToState), ev.Seq, ev.LeaseGeneration, ev.Branch, ev.CandidateSHA, ev.CreatedAt,
			ev.TaskRef, prevSeq,
		)
	}
	if err != nil {
		return AppendResult{}, fmt.Errorf("%w: upsert lifecycle task state: %v", ErrConcurrentModification, err)
	}
	if n, _ := cas.RowsAffected(); n != 1 {
		return AppendResult{}, fmt.Errorf("%w: task=%s expected prior seq=%d", ErrConcurrentModification, intent.TaskRef, prevSeq)
	}

	return AppendResult{Event: ev}, nil
}

// AllTaskStates returns the materialized state of every known task. Used
// by the Reconciler to sweep for stalled tasks.
func (s *EventStore) AllTaskStates() ([]TaskState, error) {
	rows, err := s.db.Query(
		`SELECT task_ref, repo, state, seq, lease_generation, branch, candidate_sha, updated_at
		 FROM lifecycle_task_state ORDER BY task_ref ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("all task states: %w", err)
	}
	defer rows.Close()

	var states []TaskState
	for rows.Next() {
		var ts TaskState
		var branch, candidateSHA sql.NullString
		if err := rows.Scan(&ts.TaskRef, &ts.Repo, &ts.State, &ts.Seq, &ts.LeaseGeneration, &branch, &candidateSHA, &ts.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan task state: %w", err)
		}
		ts.Branch = branch.String
		ts.CandidateSHA = candidateSHA.String
		states = append(states, ts)
	}
	return states, rows.Err()
}

// EventByIdempotencyKey returns the event previously recorded under key,
// or nil (with no error) if it has never been seen. Plain, non-tx read
// for external inspection; AppendTx uses its own tx-scoped variant.
func (s *EventStore) EventByIdempotencyKey(key string) (*Event, error) {
	return s.eventByIdempotencyKeyQuerier(s.db, key)
}

func (s *EventStore) eventByIdempotencyKeyQuerier(q querier, key string) (*Event, error) {
	row := q.QueryRow(
		`SELECT id, task_ref, repo, seq, from_state, to_state, provider_revision,
			lease_generation, branch, candidate_sha, actor, evidence_digest,
			payload, idempotency_key, created_at
		 FROM lifecycle_events WHERE idempotency_key = ?`, key,
	)
	ev, err := scanEvent(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("event by idempotency key: %w", err)
	}
	return ev, nil
}

// Events returns every event for a task, ordered by sequence.
func (s *EventStore) Events(taskRef string) ([]Event, error) {
	rows, err := s.db.Query(
		`SELECT id, task_ref, repo, seq, from_state, to_state, provider_revision,
			lease_generation, branch, candidate_sha, actor, evidence_digest,
			payload, idempotency_key, created_at
		 FROM lifecycle_events WHERE task_ref = ? ORDER BY seq ASC`, taskRef,
	)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, *ev)
	}
	return events, rows.Err()
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(row rowScanner) (*Event, error) {
	var ev Event
	var providerRevision, branch, candidateSHA, evidenceDigest, payload sql.NullString
	err := row.Scan(
		&ev.ID, &ev.TaskRef, &ev.Repo, &ev.Seq, &ev.FromState, &ev.ToState, &providerRevision,
		&ev.LeaseGeneration, &branch, &candidateSHA, &ev.Actor, &evidenceDigest,
		&payload, &ev.IdempotencyKey, &ev.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	ev.ProviderRevision = providerRevision.String
	ev.Branch = branch.String
	ev.CandidateSHA = candidateSHA.String
	ev.EvidenceDigest = evidenceDigest.String
	ev.Payload = payload.String
	return &ev, nil
}
