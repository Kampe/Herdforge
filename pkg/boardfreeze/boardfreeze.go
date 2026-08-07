// Package boardfreeze is the durable gate every provider mutation adapter
// must pass before changing task state, comments, labels, assignments, or
// relations (FAC-103, port of bin/herd-board-frozen). Reads, health, and
// reconciliation are unaffected — only the write path is gated.
//
// State is SQLite-backed (same WAL + busy_timeout contract as pkg/outbox)
// rather than a plain file sentinel like pkg/posture's ClaudeOnly/NoClaude:
// this gate carries a monotonic generation plus a blocked-mutation counter
// that must stay correct under concurrent writers from many `herd`
// processes, and a single UPDATE statement is the simplest thing that gives
// that for free.
package boardfreeze

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Kampe/Herdforge/pkg/posture"
)

// State is the persisted freeze gate row.
type State struct {
	On               bool       `json:"on"`
	Generation       int64      `json:"generation"`
	Actor            string     `json:"actor"`
	Reason           string     `json:"reason"`
	Scope            string     `json:"scope"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	ChangedAt        time.Time  `json:"changed_at"`
	BlockedMutations int64      `json:"blocked_mutations"`
}

// Expired reports whether an optional expiry has passed as of now.
func (s State) Expired(now time.Time) bool {
	return s.ExpiresAt != nil && !now.Before(*s.ExpiresAt)
}

// Active reports whether the gate is actually blocking as of now: on and
// not yet expired.
func (s State) Active(now time.Time) bool {
	return s.On && !s.Expired(now)
}

// ErrFrozen is wrapped into refusals so callers can errors.Is(err, ErrFrozen).
var ErrFrozen = errors.New("board is frozen")

const sqliteBusyTimeoutMillis = 10000

// DefaultPath is the durable gate location, mirroring pkg/posture's
// StateDir so a HERD_STATE_DIR override moves both together.
func DefaultPath() string { return filepath.Join(posture.StateDir(), "board-freeze.db") }

// Store is the SQLite-backed gate. Each herd invocation opens its own Store
// against the same path; correctness under concurrent processes comes from
// SQLite's own locking plus atomic single-statement reads/writes, not from
// any in-process cache.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the gate database at path. busy_timeout
// is set via the connection DSN — not a separate PRAGMA exec — so it is
// active before ANY statement runs, including the journal_mode=WAL
// conversion of a brand-new file. Setting it as a follow-up Exec (as an
// earlier version of this file did) leaves a window where two processes
// racing to create the same fresh database both hit "database is locked"
// immediately, because busy_timeout defaults to 0 until that Exec
// completes. Mirrors pkg/claim/sqlite_store.go's NewSQLiteLeaseStore,
// which hit and fixed the identical race.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("boardfreeze: create state dir: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, sqliteBusyTimeoutMillis)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("boardfreeze: open: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenDefault opens the gate at DefaultPath().
func OpenDefault() (*Store, error) { return Open(DefaultPath()) }

func (s *Store) Close() error { return s.db.Close() }

// isBusyErr matches SQLite's lock-contention errors. busy_timeout already
// makes SQLite itself block-and-retry internally for the configured
// window; execWithRetry adds an application-level retry on top so a
// fresh-file open/migrate race across several real OS processes (which can
// briefly exceed even a generous busy_timeout while WAL mode is being
// established) degrades to a short additional wait instead of a hard
// error. Same rationale as pkg/claim/sqlite_store.go's isBusyErr.
func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database table is locked")
}

func execWithRetry(db execer, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		res, err = db.ExecContext(context.Background(), query, args...)
		if err == nil || !isBusyErr(err) {
			return res, err
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
	return res, err
}

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) migrate() error {
	if _, err := execWithRetry(s.db, `CREATE TABLE IF NOT EXISTS board_freeze (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		is_on INTEGER NOT NULL DEFAULT 0,
		generation INTEGER NOT NULL DEFAULT 0,
		actor TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		scope TEXT NOT NULL DEFAULT '',
		expires_at DATETIME,
		changed_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		blocked_mutations INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		return fmt.Errorf("boardfreeze: migrate: %w", err)
	}
	if _, err := execWithRetry(s.db, `INSERT OR IGNORE INTO board_freeze (id, is_on, generation, changed_at) VALUES (1, 0, 0, CURRENT_TIMESTAMP)`); err != nil {
		return fmt.Errorf("boardfreeze: seed row: %w", err)
	}
	return nil
}

func scanState(row *sql.Row) (State, error) {
	var st State
	var isOn int
	var expiresAt sql.NullTime
	if err := row.Scan(&isOn, &st.Generation, &st.Actor, &st.Reason, &st.Scope, &expiresAt, &st.ChangedAt, &st.BlockedMutations); err != nil {
		return State{}, err
	}
	st.On = isOn != 0
	if expiresAt.Valid {
		t := expiresAt.Time
		st.ExpiresAt = &t
	}
	return st, nil
}

// Status returns the persisted row as-is (no expiry evaluation — callers
// that need the effective on/off value use Active).
func (s *Store) Status() (State, error) {
	row := s.db.QueryRow(`SELECT is_on, generation, actor, reason, scope, expires_at, changed_at, blocked_mutations FROM board_freeze WHERE id = 1`)
	st, err := scanState(row)
	if err != nil {
		return State{}, fmt.Errorf("boardfreeze: read state: %w", err)
	}
	return st, nil
}

// Set durably flips the gate, bumping generation. actor is always required
// (audit trail); reason is required when turning the gate on.
func (s *Store) Set(on bool, actor, reason, scope string, expiresAt *time.Time, now time.Time) (State, error) {
	if actor == "" {
		return State{}, fmt.Errorf("boardfreeze: actor is required")
	}
	if on && reason == "" {
		return State{}, fmt.Errorf("boardfreeze: reason is required to freeze the board")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return State{}, fmt.Errorf("boardfreeze: begin: %w", err)
	}
	defer tx.Rollback()
	isOn := 0
	if on {
		isOn = 1
	}
	var expiresArg any
	if expiresAt != nil {
		expiresArg = expiresAt.UTC()
	}
	if _, err := execWithRetry(tx, `UPDATE board_freeze SET is_on = ?, generation = generation + 1, actor = ?, reason = ?, scope = ?, expires_at = ?, changed_at = ? WHERE id = 1`,
		isOn, actor, reason, scope, expiresArg, now.UTC()); err != nil {
		return State{}, fmt.Errorf("boardfreeze: set: %w", err)
	}
	row := tx.QueryRow(`SELECT is_on, generation, actor, reason, scope, expires_at, changed_at, blocked_mutations FROM board_freeze WHERE id = 1`)
	st, err := scanState(row)
	if err != nil {
		return State{}, fmt.Errorf("boardfreeze: read after set: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return State{}, fmt.Errorf("boardfreeze: commit: %w", err)
	}
	return st, nil
}

// RecordBlock durably counts one refused mutation attempt so `status` can
// report pending blocked mutations even though nothing was queued for
// retry.
func (s *Store) RecordBlock() error {
	if _, err := execWithRetry(s.db, `UPDATE board_freeze SET blocked_mutations = blocked_mutations + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("boardfreeze: record block: %w", err)
	}
	return nil
}

// Active opens the default store, reads state fresh (never cached), and
// evaluates it against now. Any read failure fails closed: frozen=true is
// returned alongside the error, so a caller that only checks the bool
// still refuses.
func Active(now time.Time) (State, bool, error) {
	s, err := OpenDefault()
	if err != nil {
		return State{}, true, fmt.Errorf("boardfreeze: state unavailable: %w", err)
	}
	defer s.Close()
	st, err := s.Status()
	if err != nil {
		return State{}, true, err
	}
	return st, st.Active(now), nil
}

// SetState opens the default store and durably flips the gate.
func SetState(on bool, actor, reason, scope string, expiresAt *time.Time, now time.Time) (State, error) {
	s, err := OpenDefault()
	if err != nil {
		return State{}, fmt.Errorf("boardfreeze: state unavailable: %w", err)
	}
	defer s.Close()
	return s.Set(on, actor, reason, scope, expiresAt, now)
}

// RecordBlock opens the default store and increments the blocked-mutation
// counter. Best-effort: callers should not fail an already-refused
// mutation just because the counter write itself failed.
func RecordBlock() error {
	s, err := OpenDefault()
	if err != nil {
		return err
	}
	defer s.Close()
	return s.RecordBlock()
}
