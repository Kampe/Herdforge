// Package mutationprobe owns the bounded, transaction-like lifecycle of
// temporary git worktrees used by reviewer/verifier mutation probes
// (FAC-157).
//
// Create an isolated checkout at a candidate SHA, run the mutation, and
// restore/reap the probe on success, failure, timeout, cancellation, or
// crash recovery. Clean disposable probes are unregistered and removed;
// dirty or uncertain probes are preserved byte-for-byte and reported
// BLOCKED — never force-removed.
//
// Registry checks are always scoped to the Manager's configured origin
// repository and temp root so hermetic tests cannot mutate a developer
// checkout.
package mutationprobe

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// State is a probe receipt's lifecycle stage.
type State string

const (
	// StateRegistered is recorded immediately after git worktree add
	// succeeds, before any mutation or later failure — so a crash right
	// after create still leaves a receipt reconciliation can find.
	StateRegistered State = "registered"
	// StateActive means the probe is in use by a mutation run.
	StateActive State = "active"
	// StateAwaitingCleanup means the run reached a terminal outcome but
	// removal has not yet been confirmed.
	StateAwaitingCleanup State = "awaiting_cleanup"
	// StateRemoved means the exact probe was removed and its absence
	// was independently confirmed against both the temp directory and
	// the origin repository's git worktree list.
	StateRemoved State = "removed"
	// StatePreserved means cleanup refused the probe (dirty, unique,
	// or unknown). The worktree remains on disk as recovery evidence.
	StatePreserved State = "preserved"
)

// Class is the fail-closed classification used before any destructive
// cleanup action.
type Class string

const (
	// ClassDisposable is clean and still pinned at the registered
	// candidate SHA — safe to remove without --force.
	ClassDisposable Class = "disposable"
	// ClassDirty has uncommitted changes; refuse and preserve.
	ClassDirty Class = "dirty"
	// ClassUnique has commits not present at the registered candidate.
	ClassUnique Class = "unique-committed"
	// ClassUnknown means a Git (or probe) error prevented safe
	// classification. Unknown is a hard refusal — never permission to remove.
	ClassUnknown Class = "unknown"
)

var (
	// ErrUnknownProbe is returned for operations on a probe ID this store
	// has no receipt for.
	ErrUnknownProbe = errors.New("mutationprobe: no receipt for this probe id")
	// ErrIdentityConflict is returned when a probe ID is registered again
	// under a different task_ref/generation/candidate than its existing
	// receipt.
	ErrIdentityConflict = errors.New("mutationprobe: probe id already registered under a different identity")
	// ErrInvalidTransition is returned when a state change is not legal
	// from the receipt's current state.
	ErrInvalidTransition = errors.New("mutationprobe: invalid state transition")
	// ErrAbsenceNotProved is returned by MarkRemoved when the caller
	// has not independently confirmed the probe is gone from both temp
	// storage and the git worktree registry.
	ErrAbsenceNotProved = errors.New("mutationprobe: cannot mark removed without absence proof")
	// ErrProbePreserved is returned when cleanup refuses a dirty,
	// unique, or unknown probe. The receipt is left in StatePreserved.
	ErrProbePreserved = errors.New("mutationprobe: probe preserved as recovery evidence")
	// ErrNotDisposable is returned by Classify when the probe is not
	// safe to remove.
	ErrNotDisposable = errors.New("mutationprobe: probe is not disposable")
)

// Receipt is the durable record of exactly one mutation-probe launch.
// Paths are never absolute host paths: ProbeName is the portable leaf
// under the manager's TempRoot (e.g. "herd-mutprobe.<id>").
type Receipt struct {
	ID                    int64
	ProbeID               string
	TaskRef               string
	Generation            string
	CandidateSHA          string
	ProbeName             string // portable leaf name; never an absolute path
	ExpectedTerminalState string
	State                 State
	Class                 Class
	AbsenceProved         bool
	LastError             string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	RemovedAt             *time.Time
}

const sqliteBusyTimeoutMillis = 5000

// Store is the SQLite-backed probe receipt persistence.
type Store struct{ db *sql.DB }

// NewStore opens (or creates) a SQLite database at path and applies the
// probe_receipts schema, matching pkg/containerlifecycle's concurrency
// contract: WAL journal mode, a bounded busy_timeout, and a single-
// connection pool.
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open mutationprobe store: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set journal_mode: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", sqliteBusyTimeoutMillis)); err != nil {
		_ = db.Close()
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

// Close closes the underlying connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS probe_receipts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		probe_id TEXT NOT NULL UNIQUE,
		task_ref TEXT NOT NULL,
		generation TEXT NOT NULL,
		candidate_sha TEXT NOT NULL,
		probe_name TEXT NOT NULL,
		expected_terminal_state TEXT,
		state TEXT NOT NULL DEFAULT 'registered',
		class TEXT NOT NULL DEFAULT '',
		absence_proved INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		removed_at DATETIME
	)`)
	if err != nil {
		return fmt.Errorf("migrate probe_receipts schema: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_probe_receipts_state ON probe_receipts(state)`); err != nil {
		return fmt.Errorf("migrate probe_receipts schema: %w", err)
	}
	return nil
}

// Register durably records a probe immediately after git worktree add
// succeeds. Registering the same probe_id again with the same identity
// is an idempotent no-op; a different identity fails closed.
func (s *Store) Register(r Receipt) (Receipt, error) {
	if r.ProbeID == "" || r.TaskRef == "" || r.Generation == "" || r.CandidateSHA == "" || r.ProbeName == "" {
		return Receipt{}, fmt.Errorf("mutationprobe: probe id, task ref, generation, candidate sha, and probe name are required")
	}
	if filepathIsAbs(r.ProbeName) || strings.ContainsAny(r.ProbeName, `/\`) {
		return Receipt{}, fmt.Errorf("mutationprobe: probe name must be a portable leaf, not a path: %q", r.ProbeName)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Receipt{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, r.ProbeID)
	if err != nil {
		return Receipt{}, err
	}
	if existing != nil {
		if existing.TaskRef != r.TaskRef || existing.Generation != r.Generation ||
			existing.CandidateSHA != r.CandidateSHA || existing.ProbeName != r.ProbeName {
			return Receipt{}, fmt.Errorf("%w: id=%s", ErrIdentityConflict, r.ProbeID)
		}
		if err := tx.Commit(); err != nil {
			return Receipt{}, fmt.Errorf("commit: %w", err)
		}
		return *existing, nil
	}
	now := time.Now().UTC()
	_, err = tx.Exec(`INSERT INTO probe_receipts
		(probe_id, task_ref, generation, candidate_sha, probe_name, expected_terminal_state, state, class, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ProbeID, r.TaskRef, r.Generation, r.CandidateSHA, r.ProbeName, r.ExpectedTerminalState, StateRegistered, "", now, now)
	if err != nil {
		return Receipt{}, fmt.Errorf("insert probe receipt: %w", err)
	}
	out, err := getFrom(tx, r.ProbeID)
	if err != nil {
		return Receipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Receipt{}, fmt.Errorf("commit: %w", err)
	}
	return *out, nil
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// Get returns the receipt for probeID, or nil if none exists.
func (s *Store) Get(probeID string) (*Receipt, error) {
	return getFrom(s.db, probeID)
}

func getFrom(q queryRower, probeID string) (*Receipt, error) {
	row := q.QueryRow(`SELECT id, probe_id, task_ref, generation, candidate_sha, probe_name,
		expected_terminal_state, state, class, absence_proved, last_error, created_at, updated_at, removed_at
		FROM probe_receipts WHERE probe_id = ?`, probeID)
	r, err := scanReceipt(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// MarkActive transitions a registered receipt to active.
func (s *Store) MarkActive(probeID string) error {
	return s.transition(probeID, []State{StateRegistered}, StateActive, "", "", false)
}

// MarkAwaitingCleanup transitions to awaiting_cleanup, recording the
// terminal state the run actually reached in the same statement.
func (s *Store) MarkAwaitingCleanup(probeID, expectedTerminalState string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, probeID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("%w: id=%s", ErrUnknownProbe, probeID)
	}
	if existing.State == StateAwaitingCleanup {
		return nil
	}
	if existing.State != StateRegistered && existing.State != StateActive {
		return fmt.Errorf("%w: %s -> %s (currently %s)", ErrInvalidTransition, existing.State, StateAwaitingCleanup, existing.State)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE probe_receipts SET state = ?, expected_terminal_state = ?, updated_at = ? WHERE probe_id = ?`,
		StateAwaitingCleanup, expectedTerminalState, now, probeID); err != nil {
		return fmt.Errorf("update probe receipt: %w", err)
	}
	return tx.Commit()
}

// MarkRemoved transitions to removed. absenceProved must be true.
func (s *Store) MarkRemoved(probeID string, absenceProved bool) error {
	if !absenceProved {
		return ErrAbsenceNotProved
	}
	return s.transition(probeID, []State{StateRegistered, StateActive, StateAwaitingCleanup}, StateRemoved, "", ClassDisposable, true)
}

// MarkPreserved transitions to preserved with the refuse classification
// and recovery reason. The worktree remains on disk.
func (s *Store) MarkPreserved(probeID string, class Class, reason string) error {
	return s.transition(probeID, []State{StateRegistered, StateActive, StateAwaitingCleanup}, StatePreserved, reason, class, false)
}

func (s *Store) transition(probeID string, from []State, to State, reason string, class Class, removed bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, probeID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("%w: id=%s", ErrUnknownProbe, probeID)
	}
	if existing.State == to {
		return nil
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
		_, err = tx.Exec(`UPDATE probe_receipts SET state = ?, class = ?, absence_proved = 1, updated_at = ?, removed_at = ? WHERE probe_id = ?`,
			to, class, now, now, probeID)
	} else {
		_, err = tx.Exec(`UPDATE probe_receipts SET state = ?, class = ?, last_error = ?, updated_at = ? WHERE probe_id = ?`,
			to, class, reason, now, probeID)
	}
	if err != nil {
		return fmt.Errorf("update probe receipt: %w", err)
	}
	return tx.Commit()
}

// ListNonTerminal returns every receipt not yet removed or preserved,
// ordered by id for deterministic sweeps.
func (s *Store) ListNonTerminal() ([]Receipt, error) {
	return s.list(`state IN (?, ?, ?) ORDER BY id ASC`, StateRegistered, StateActive, StateAwaitingCleanup)
}

// ListPreserved returns every receipt left as recovery evidence.
func (s *Store) ListPreserved() ([]Receipt, error) {
	return s.list(`state = ? ORDER BY id ASC`, StatePreserved)
}

// ListAll returns every receipt, ordered by id.
func (s *Store) ListAll() ([]Receipt, error) {
	return s.list(`1 = 1 ORDER BY id ASC`)
}

func (s *Store) list(where string, args ...any) ([]Receipt, error) {
	rows, err := s.db.Query(`SELECT id, probe_id, task_ref, generation, candidate_sha, probe_name,
		expected_terminal_state, state, class, absence_proved, last_error, created_at, updated_at, removed_at
		FROM probe_receipts WHERE `+where, args...)
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
	var expected, class, lastErr sql.NullString
	if err := row.Scan(
		&r.ID, &r.ProbeID, &r.TaskRef, &r.Generation, &r.CandidateSHA, &r.ProbeName,
		&expected, &r.State, &class, &absenceProved, &lastErr, &r.CreatedAt, &r.UpdatedAt, &removedAt,
	); err != nil {
		return nil, err
	}
	if expected.Valid {
		r.ExpectedTerminalState = expected.String
	}
	if class.Valid {
		r.Class = Class(class.String)
	}
	if lastErr.Valid {
		r.LastError = lastErr.String
	}
	r.AbsenceProved = absenceProved != 0
	if removedAt.Valid {
		t := removedAt.Time
		r.RemovedAt = &t
	}
	return &r, nil
}

// filepathIsAbs is a tiny helper so store.go does not import path/filepath
// only for one check (and so drive-letter absolute paths are refused too).
func filepathIsAbs(p string) bool {
	if p == "" {
		return false
	}
	if p[0] == '/' {
		return true
	}
	// Drive-letter absolute form (letter + colon), refused for portable names.
	if len(p) >= 2 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' {
		return true
	}
	return false
}
