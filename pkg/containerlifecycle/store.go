// Package containerlifecycle persists a durable receipt for every
// container a Herdforge hermetic/containerized verification launches, so
// cleanup identifies and removes exactly its own containers across
// success, failure, timeout, cancellation, and process interruption, and
// a restarted coordinator can reconcile stale receipts without resorting
// to name substrings, globs, docker prune, image ancestry, or mutable
// task status (FAC-200).
//
// BLOCKED on this branch: nothing here has a production caller yet,
// because no hermetic/containerized verification launch code exists on
// main or this branch — it exists only on unmerged
// task/fac-198-hermetic-receipt-r1/r2. See
// docs/fac-200-integration-status.md for the evidence and the exact
// integration checklist for wiring this package in once that lands.
package containerlifecycle

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// State is a container receipt's lifecycle stage.
type State string

const (
	// StateRegistered is recorded immediately after `docker create`
	// succeeds, before start or any later failure — so a crash right
	// after create still leaves a receipt reconciliation can find.
	StateRegistered State = "registered"
	// StateStarted means the container is running.
	StateStarted State = "started"
	// StateAwaitingCleanup means the run reached a terminal outcome
	// (success, failed test, timeout, cancellation) but removal has not
	// yet been confirmed.
	StateAwaitingCleanup State = "awaiting_cleanup"
	// StateRemoved means the exact container ID was removed and its
	// absence was independently confirmed (not merely that `docker rm`
	// returned success).
	StateRemoved State = "removed"
	// StateQuarantined means cleanup was attempted and failed, or
	// absence could not be confirmed; the receipt needs operator review
	// rather than further automatic retries.
	StateQuarantined State = "quarantined"
)

var (
	// ErrUnknownContainer is returned for any operation on a container
	// ID this store has no receipt for — the read side of "never
	// identify cleanup targets by anything but an exact durable
	// receipt".
	ErrUnknownContainer = errors.New("containerlifecycle: no receipt for this container id")
	// ErrIdentityConflict is returned when a container ID is registered
	// again under a different task_ref/generation than its existing
	// receipt — an ID cannot silently change owners.
	ErrIdentityConflict = errors.New("containerlifecycle: container id already registered under a different identity")
	// ErrInvalidTransition is returned when a state change isn't legal
	// from the receipt's current state.
	ErrInvalidTransition = errors.New("containerlifecycle: invalid state transition")
	// ErrAbsenceNotProved is returned by MarkRemoved when the caller
	// hasn't independently confirmed the container is actually gone.
	ErrAbsenceNotProved = errors.New("containerlifecycle: cannot mark removed without absence proof")
)

// Receipt is the durable record of exactly one container launch.
type Receipt struct {
	ID                    int64
	ContainerID           string
	TaskRef               string
	Generation            string
	ImageDigest           string
	CleanupOwner          string
	ExpectedTerminalState string
	State                 State
	AbsenceProved         bool
	LastError             string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	RemovedAt             *time.Time
}

const sqliteBusyTimeoutMillis = 5000

// Store is the SQLite-backed container receipt persistence.
type Store struct{ db *sql.DB }

// NewStore opens (or creates) a SQLite database at path and applies the
// container_receipts schema, matching pkg/outbox's concurrency contract:
// WAL journal mode, a bounded busy_timeout, and a single-connection pool.
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open container lifecycle store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set journal_mode: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", sqliteBusyTimeoutMillis)); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	return NewStoreWithDB(db)
}

// NewStoreWithDB wraps an already-open *sql.DB, applying the schema.
func NewStoreWithDB(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// DB returns the underlying connection.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS container_receipts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		container_id TEXT NOT NULL UNIQUE,
		task_ref TEXT NOT NULL,
		generation TEXT NOT NULL,
		image_digest TEXT,
		cleanup_owner TEXT,
		expected_terminal_state TEXT,
		state TEXT NOT NULL DEFAULT 'registered',
		absence_proved INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		removed_at DATETIME
	)`)
	if err != nil {
		return fmt.Errorf("migrate container_receipts schema: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_container_receipts_state ON container_receipts(state)`); err != nil {
		return fmt.Errorf("migrate container_receipts schema: %w", err)
	}
	return nil
}

// Register durably records containerID's launch. Callers must call this
// immediately after `docker create` succeeds, before start or any later
// failure. Registering the same container_id again with the same
// task_ref/generation is an idempotent no-op (replay); registering it
// under a different task_ref/generation fails closed with
// ErrIdentityConflict instead of silently taking over ownership of an ID
// another run believes it owns.
func (s *Store) Register(r Receipt) (Receipt, error) {
	if r.ContainerID == "" || r.TaskRef == "" || r.Generation == "" {
		return Receipt{}, fmt.Errorf("containerlifecycle: container id, task ref, and generation are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Receipt{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, r.ContainerID)
	if err != nil {
		return Receipt{}, err
	}
	if existing != nil {
		if existing.TaskRef != r.TaskRef || existing.Generation != r.Generation {
			return Receipt{}, fmt.Errorf("%w: id=%s", ErrIdentityConflict, r.ContainerID)
		}
		if err := tx.Commit(); err != nil {
			return Receipt{}, fmt.Errorf("commit: %w", err)
		}
		return *existing, nil
	}
	now := time.Now().UTC()
	_, err = tx.Exec(`INSERT INTO container_receipts
		(container_id, task_ref, generation, image_digest, cleanup_owner, expected_terminal_state, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ContainerID, r.TaskRef, r.Generation, r.ImageDigest, r.CleanupOwner, r.ExpectedTerminalState, StateRegistered, now, now)
	if err != nil {
		return Receipt{}, fmt.Errorf("insert container receipt: %w", err)
	}
	out, err := getFrom(tx, r.ContainerID)
	if err != nil {
		return Receipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Receipt{}, fmt.Errorf("commit: %w", err)
	}
	return *out, nil
}

// queryRower is satisfied by both *sql.DB and *sql.Tx, so reads can run
// either standalone or as part of an atomic check-then-act transaction.
type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// Get returns the receipt for containerID, or nil if none exists.
func (s *Store) Get(containerID string) (*Receipt, error) {
	return getFrom(s.db, containerID)
}

func getFrom(q queryRower, containerID string) (*Receipt, error) {
	row := q.QueryRow(`SELECT id, container_id, task_ref, generation, image_digest, cleanup_owner,
		expected_terminal_state, state, absence_proved, last_error, created_at, updated_at, removed_at
		FROM container_receipts WHERE container_id = ?`, containerID)
	r, err := scanReceipt(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// MarkStarted transitions a registered receipt to started.
func (s *Store) MarkStarted(containerID string) error {
	return s.transition(containerID, []State{StateRegistered}, StateStarted, "", false)
}

// MarkAwaitingCleanup transitions to awaiting_cleanup, recording the
// terminal state the run actually reached (e.g. "success", "test_failed",
// "timeout", "cancelled") in the SAME statement as the state change, so a
// crash between them can never leave a terminal-awaiting receipt with a
// blank expected state. Callers invoke this once, at the point their run
// concludes for any reason, immediately before the single deferred
// EnsureCleanup call — the "one defer/compensation path for every
// outcome" this task requires. EnsureCleanup calls this itself, so most
// callers never need to call it directly.
func (s *Store) MarkAwaitingCleanup(containerID, expectedTerminalState string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, containerID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("%w: id=%s", ErrUnknownContainer, containerID)
	}
	if existing.State == StateAwaitingCleanup {
		return nil // idempotent replay
	}
	if existing.State != StateRegistered && existing.State != StateStarted {
		return fmt.Errorf("%w: %s -> %s (currently %s)", ErrInvalidTransition, existing.State, StateAwaitingCleanup, existing.State)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE container_receipts SET state = ?, expected_terminal_state = ?, updated_at = ? WHERE container_id = ?`,
		StateAwaitingCleanup, expectedTerminalState, now, containerID); err != nil {
		return fmt.Errorf("update container receipt: %w", err)
	}
	return tx.Commit()
}

// MarkRemoved transitions to removed. absenceProved must be true: this
// store refuses to record a container as removed on the strength of a
// remove call alone; the caller must have independently confirmed the
// exact ID is gone (e.g. `docker inspect` reporting "no such container").
func (s *Store) MarkRemoved(containerID string, absenceProved bool) error {
	if !absenceProved {
		return ErrAbsenceNotProved
	}
	return s.transition(containerID, []State{StateRegistered, StateStarted, StateAwaitingCleanup}, StateRemoved, "", true)
}

// MarkQuarantined transitions to quarantined: cleanup was attempted and
// failed (or absence couldn't be confirmed), so this receipt needs
// operator review instead of further automatic retries this sweep.
func (s *Store) MarkQuarantined(containerID, reason string) error {
	return s.transition(containerID, []State{StateRegistered, StateStarted, StateAwaitingCleanup}, StateQuarantined, reason, false)
}

// transition performs one atomic check-then-act state change: the read
// of the current state and the conditional write happen inside a single
// SQLite transaction, so a racing writer can never observe (or act on) a
// torn intermediate state.
func (s *Store) transition(containerID string, from []State, to State, reason string, removed bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, containerID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("%w: id=%s", ErrUnknownContainer, containerID)
	}
	if existing.State == to {
		return nil // idempotent replay
	}
	ok := false
	for _, f := range from {
		if existing.State == f {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("%w: %s -> %s (currently %s)", ErrInvalidTransition, existing.State, to, existing.State)
	}
	now := time.Now().UTC()
	if removed {
		_, err = tx.Exec(`UPDATE container_receipts SET state = ?, absence_proved = 1, updated_at = ?, removed_at = ? WHERE container_id = ?`,
			to, now, now, containerID)
	} else {
		_, err = tx.Exec(`UPDATE container_receipts SET state = ?, last_error = ?, updated_at = ? WHERE container_id = ?`,
			to, reason, now, containerID)
	}
	if err != nil {
		return fmt.Errorf("update container receipt: %w", err)
	}
	return tx.Commit()
}

// ListNonTerminal returns every receipt not yet removed or quarantined,
// ordered by id for deterministic sweeps.
func (s *Store) ListNonTerminal() ([]Receipt, error) {
	return s.list(`state IN (?, ?, ?) ORDER BY id ASC`, StateRegistered, StateStarted, StateAwaitingCleanup)
}

// ListAll returns every receipt, ordered by id.
func (s *Store) ListAll() ([]Receipt, error) {
	return s.list(`1 = 1 ORDER BY id ASC`)
}

func (s *Store) list(where string, args ...any) ([]Receipt, error) {
	rows, err := s.db.Query(`SELECT id, container_id, task_ref, generation, image_digest, cleanup_owner,
		expected_terminal_state, state, absence_proved, last_error, created_at, updated_at, removed_at
		FROM container_receipts WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanReceipt(row rowScanner) (*Receipt, error) {
	var r Receipt
	var absenceProved int
	var removedAt sql.NullTime
	var imageDigest, cleanupOwner, expected, lastError sql.NullString
	err := row.Scan(&r.ID, &r.ContainerID, &r.TaskRef, &r.Generation, &imageDigest, &cleanupOwner,
		&expected, &r.State, &absenceProved, &lastError, &r.CreatedAt, &r.UpdatedAt, &removedAt)
	if err != nil {
		return nil, err
	}
	r.ImageDigest = imageDigest.String
	r.CleanupOwner = cleanupOwner.String
	r.ExpectedTerminalState = expected.String
	r.LastError = lastError.String
	r.AbsenceProved = absenceProved != 0
	if removedAt.Valid {
		t := removedAt.Time
		r.RemovedAt = &t
	}
	return &r, nil
}
