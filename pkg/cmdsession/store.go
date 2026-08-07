// Package cmdsession is the durable authority for every command execution a
// coordinator or worker command runner starts: one receipt per exec session,
// PTY, shell, or bounded CLI child, keyed by coordinator session and tool-call
// ID and bound to an exact PID + start token.
//
// It exists because a finished tool call must not be able to leave a live
// background terminal behind. On 2026-08-04 a targeted ancestry inspection of
// the coordinator found six child shell command sessions still alive at zero
// CPU 1 to 6.5 hours after completed-looking commands, while the harness UI
// reported six background terminals (FAC-193). That is a different class from
// FAC-188's inherited graph MCP servers: these are general exec/PTY command
// sessions, so this package tracks them itself rather than widening
// pkg/toolchild's lane-owned tool-child boundary.
//
// Two rules shape the whole package:
//
//   - Nothing is ever reaped on the strength of a PID alone. Identity is
//     PID + start token + parentage; any mismatch, probe failure, or
//     ambiguity fails closed as durable BLOCKED evidence and the session is
//     left exactly as it was.
//   - Live and ambiguous sessions are never signalled or closed. This package
//     contains no signalling at all; teardown is close-descriptors-then-wait
//     on handles the owning process already holds.
package cmdsession

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

// State is a command session's lifecycle stage.
type State string

const (
	// StateRunning is recorded the moment a command session is spawned,
	// before any output or wait, so a coordinator crash one instruction
	// later still leaves a receipt reconciliation can find.
	StateRunning State = "running"
	// StateCompleted means the runner recorded a terminal outcome for the
	// tool call (any outcome, including failure) but the exec session has
	// not yet been closed and waited. This is the state the six retained
	// shells were in.
	StateCompleted State = "completed"
	// StateReaped means every descriptor was closed and the exact session
	// was waited exactly once by the process that owns it. Terminal.
	StateReaped State = "reaped"
	// StateSettledAbsent means the exact PID+start token was proven absent,
	// so there is nothing left to close or wait — but this process did not
	// perform the wait and does not claim to have. Terminal.
	StateSettledAbsent State = "settled_absent"
	// StateBlocked means cleanup was refused because identity or completion
	// was ambiguous. It needs an operator, not another automatic sweep.
	StateBlocked State = "blocked"
)

// Outcome classifies how a tool call concluded. Every path a command runner
// can take has one, so "completed" never silently means "exited zero".
const (
	OutcomeNormal           = "normal"
	OutcomeNonZeroExit      = "nonzero_exit"
	OutcomeCanceled         = "canceled"
	OutcomeTimedOut         = "timed_out"
	OutcomeCoordinatorCrash = "coordinator_crash"
	OutcomeLostReadback     = "lost_readback"
)

var validOutcomes = map[string]bool{
	OutcomeNormal:           true,
	OutcomeNonZeroExit:      true,
	OutcomeCanceled:         true,
	OutcomeTimedOut:         true,
	OutcomeCoordinatorCrash: true,
	OutcomeLostReadback:     true,
}

var (
	// ErrUnknownSession is returned for any operation on a session this
	// store has no receipt for. Cleanup targets come from receipts only.
	ErrUnknownSession = errors.New("cmdsession: no receipt for this command session")
	// ErrIdentityCollision is returned when a key is registered again under
	// a different exact identity — a session key cannot change owners.
	ErrIdentityCollision = errors.New("cmdsession: command session key already registered under a different identity")
	// ErrInvalidTransition is returned when a state change is not legal.
	ErrInvalidTransition = errors.New("cmdsession: invalid state transition")
	// ErrNotCompleted is returned when a reap is attempted on a session
	// that has not recorded a terminal outcome. Live work is never torn down.
	ErrNotCompleted = errors.New("cmdsession: refusing to reap a session with no terminal outcome")
	// ErrIncompleteIdentity is returned when a registration omits any part
	// of the exact identity a later reap has to re-prove.
	ErrIncompleteIdentity = errors.New("cmdsession: command session identity is incomplete")
)

// Key identifies one tool call's command session.
type Key struct {
	CoordinatorSession string `json:"coordinator_session"`
	ToolCallID         string `json:"tool_call_id"`
}

func (k Key) String() string { return k.CoordinatorSession + "/" + k.ToolCallID }

// Identity is the exact, re-provable binding for one command session. Every
// field is evidence a later sweep re-checks before touching anything;
// PID on its own is never sufficient.
type Identity struct {
	Key
	PID           int    `json:"pid"`
	ParentPID     int    `json:"parent_pid"`
	StartToken    string `json:"start_token"`
	ProcessGroup  int    `json:"process_group,omitempty"`
	PTY           string `json:"pty,omitempty"`
	CommandDigest string `json:"command_digest"`
	WorkingDir    string `json:"working_dir"`
	TaskRef       string `json:"task_ref,omitempty"`
	// LaneOwned marks a child that pkg/toolchild (FAC-188) already owns as a
	// lane-bound tool child. This package tracks it for visibility but never
	// tears it down, so neither boundary is duplicated or weakened.
	LaneOwned bool      `json:"lane_owned,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// Receipt is the durable record of exactly one command session.
type Receipt struct {
	ID int64 `json:"id"`
	Identity
	State    State  `json:"state"`
	Outcome  string `json:"outcome,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	// LastOutputAt is the last time the runner observed output on this
	// session. Output after CompletedAt means the terminal receipt is not
	// trustworthy, which fails closed rather than reaping a talking session.
	LastOutputAt  *time.Time `json:"last_output_at,omitempty"`
	DetachedOpen  int        `json:"detached_open"`
	BlockedReason string     `json:"blocked_reason,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	ReapedAt      *time.Time `json:"reaped_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Retained reports whether this receipt still describes a command session
// that has not been accounted for — the count fleet status must surface.
func (r Receipt) Retained() bool {
	return r.State == StateRunning || r.State == StateCompleted
}

const sqliteBusyTimeoutMillis = 5000

// Store is the SQLite-backed command session receipt persistence.
type Store struct{ db *sql.DB }

// NewStore opens (or creates) a SQLite database at path and applies the
// schema, matching pkg/outbox's concurrency contract: WAL journal mode, a
// bounded busy_timeout, and a single-connection pool.
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open command session store: %w", err)
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
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS command_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			coordinator_session TEXT NOT NULL,
			tool_call_id TEXT NOT NULL,
			pid INTEGER NOT NULL,
			parent_pid INTEGER NOT NULL,
			start_token TEXT NOT NULL,
			process_group INTEGER NOT NULL DEFAULT 0,
			pty TEXT,
			command_digest TEXT NOT NULL,
			working_dir TEXT NOT NULL,
			task_ref TEXT,
			lane_owned INTEGER NOT NULL DEFAULT 0,
			started_at DATETIME NOT NULL,
			state TEXT NOT NULL,
			outcome TEXT,
			exit_code INTEGER,
			last_output_at DATETIME,
			blocked_reason TEXT,
			completed_at DATETIME,
			reaped_at DATETIME,
			updated_at DATETIME NOT NULL,
			UNIQUE(coordinator_session, tool_call_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_command_sessions_state ON command_sessions(state)`,
		`CREATE TABLE IF NOT EXISTS command_session_detached (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			coordinator_session TEXT NOT NULL,
			tool_call_id TEXT NOT NULL,
			pid INTEGER NOT NULL,
			start_token TEXT NOT NULL,
			settled INTEGER NOT NULL DEFAULT 0,
			UNIQUE(coordinator_session, tool_call_id, pid, start_token)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate command_sessions schema: %w", err)
		}
	}
	return nil
}

// Register durably records a command session's exact identity. Runners must
// call this immediately after spawn, before the first read.
//
// Re-registering the same key with the same identity is an idempotent replay.
// Re-registering it with a different PID, start token, or command digest fails
// closed with ErrIdentityCollision rather than silently rebinding a receipt a
// live session still believes describes it.
//
// Registering a PID that a *different* non-terminal receipt is still bound to
// under a different start token is PID reuse: the older receipt can never be
// re-proved again, so it is marked BLOCKED with durable evidence here rather
// than being reaped later against whatever now holds that PID.
func (s *Store) Register(id Identity) (Receipt, error) {
	if id.CoordinatorSession == "" || id.ToolCallID == "" || id.PID <= 0 ||
		id.ParentPID <= 0 || id.StartToken == "" || id.CommandDigest == "" || id.WorkingDir == "" {
		return Receipt{}, ErrIncompleteIdentity
	}
	if id.StartedAt.IsZero() {
		id.StartedAt = time.Now().UTC()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Receipt{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, id.Key)
	if err != nil {
		return Receipt{}, err
	}
	if existing != nil {
		if existing.PID != id.PID || existing.StartToken != id.StartToken || existing.CommandDigest != id.CommandDigest {
			return Receipt{}, fmt.Errorf("%w: key=%s", ErrIdentityCollision, id.Key)
		}
		if err := tx.Commit(); err != nil {
			return Receipt{}, fmt.Errorf("commit: %w", err)
		}
		return *existing, nil
	}
	now := time.Now().UTC()
	if err := blockReusedPID(tx, id, now); err != nil {
		return Receipt{}, err
	}
	_, err = tx.Exec(`INSERT INTO command_sessions
		(coordinator_session, tool_call_id, pid, parent_pid, start_token, process_group, pty,
		 command_digest, working_dir, task_ref, lane_owned, started_at, state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.CoordinatorSession, id.ToolCallID, id.PID, id.ParentPID, id.StartToken, id.ProcessGroup, id.PTY,
		id.CommandDigest, id.WorkingDir, id.TaskRef, boolToInt(id.LaneOwned), id.StartedAt, StateRunning, now)
	if err != nil {
		return Receipt{}, fmt.Errorf("insert command session receipt: %w", err)
	}
	out, err := getFrom(tx, id.Key)
	if err != nil {
		return Receipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Receipt{}, fmt.Errorf("commit: %w", err)
	}
	return *out, nil
}

// blockReusedPID marks every non-terminal receipt bound to id's PID under a
// different start token as BLOCKED, inside the caller's transaction.
func blockReusedPID(tx *sql.Tx, id Identity, now time.Time) error {
	rows, err := tx.Query(`SELECT coordinator_session, tool_call_id, start_token FROM command_sessions
		WHERE pid = ? AND state IN (?, ?)`, id.PID, StateRunning, StateCompleted)
	if err != nil {
		return fmt.Errorf("scan for pid reuse: %w", err)
	}
	type stale struct {
		key   Key
		token string
	}
	var found []stale
	for rows.Next() {
		var st stale
		if err := rows.Scan(&st.key.CoordinatorSession, &st.key.ToolCallID, &st.token); err != nil {
			rows.Close()
			return err
		}
		if st.token != id.StartToken {
			found = append(found, st)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, st := range found {
		reason := fmt.Sprintf("pid %d reused by %s (start token %s != %s); identity can no longer be re-proved",
			id.PID, id.Key, id.StartToken, st.token)
		if _, err := tx.Exec(`UPDATE command_sessions SET state = ?, blocked_reason = ?, updated_at = ?
			WHERE coordinator_session = ? AND tool_call_id = ?`,
			StateBlocked, reason, now, st.key.CoordinatorSession, st.key.ToolCallID); err != nil {
			return fmt.Errorf("record pid reuse: %w", err)
		}
	}
	return nil
}

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// Get returns the receipt for key, or nil if none exists.
func (s *Store) Get(key Key) (*Receipt, error) { return getFrom(s.db, key) }

const receiptColumns = `id, coordinator_session, tool_call_id, pid, parent_pid, start_token, process_group,
	pty, command_digest, working_dir, task_ref, lane_owned, started_at, state, outcome, exit_code,
	last_output_at, blocked_reason, completed_at, reaped_at, updated_at`

func getFrom(q queryRower, key Key) (*Receipt, error) {
	row := q.QueryRow(`SELECT `+receiptColumns+` FROM command_sessions
		WHERE coordinator_session = ? AND tool_call_id = ?`, key.CoordinatorSession, key.ToolCallID)
	r, err := scanReceipt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.DetachedOpen, err = countDetached(q, key)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func countDetached(q queryRower, key Key) (int, error) {
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM command_session_detached
		WHERE coordinator_session = ? AND tool_call_id = ? AND settled = 0`,
		key.CoordinatorSession, key.ToolCallID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count detached descendants: %w", err)
	}
	return n, nil
}

// NoteOutput records that output was observed on this session at ts. A runner
// calls it as it reads; a timestamp later than the terminal receipt is what
// makes "the parent said it finished but the session is still talking"
// detectable instead of invisible.
func (s *Store) NoteOutput(key Key, ts time.Time) error {
	res, err := s.db.Exec(`UPDATE command_sessions SET last_output_at = ?, updated_at = ?
		WHERE coordinator_session = ? AND tool_call_id = ?`,
		ts.UTC(), time.Now().UTC(), key.CoordinatorSession, key.ToolCallID)
	if err != nil {
		return fmt.Errorf("record session output: %w", err)
	}
	return requireAffected(res, key)
}

// RegisterDetached records a detached descendant this session spawned that
// outlives the parent shell (a background writer). While one is unsettled the
// session cannot be reaped, so a parent shell's exit alone can never mint a
// terminal receipt for work that is still running.
func (s *Store) RegisterDetached(key Key, pid int, startToken string) error {
	if pid <= 0 || startToken == "" {
		return ErrIncompleteIdentity
	}
	r, err := s.Get(key)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("%w: key=%s", ErrUnknownSession, key)
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO command_session_detached
		(coordinator_session, tool_call_id, pid, start_token) VALUES (?, ?, ?, ?)`,
		key.CoordinatorSession, key.ToolCallID, pid, startToken)
	if err != nil {
		return fmt.Errorf("register detached descendant: %w", err)
	}
	return nil
}

// SettleDetached marks one detached descendant accounted for. The exact
// start token must match: settling by PID alone would let a reused PID
// silently release a descendant that is still running.
func (s *Store) SettleDetached(key Key, pid int, startToken string) error {
	res, err := s.db.Exec(`UPDATE command_session_detached SET settled = 1
		WHERE coordinator_session = ? AND tool_call_id = ? AND pid = ? AND start_token = ?`,
		key.CoordinatorSession, key.ToolCallID, pid, startToken)
	if err != nil {
		return fmt.Errorf("settle detached descendant: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: no detached descendant pid=%d token=%s for %s", ErrUnknownSession, pid, startToken, key)
	}
	return nil
}

// MarkCompleted records the tool call's terminal outcome. Runners call this
// once, from the single deferred path every outcome flows through — normal
// exit, non-zero exit, cancellation, timeout, crash recovery, or a lost
// readback. It records the outcome and the state in one statement, so a crash
// between them cannot leave a completed receipt with no outcome.
func (s *Store) MarkCompleted(key Key, outcome string, exitCode *int) error {
	if !validOutcomes[outcome] {
		return fmt.Errorf("cmdsession: unknown completion outcome %q", outcome)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, key)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("%w: key=%s", ErrUnknownSession, key)
	}
	if existing.State == StateCompleted {
		return nil // idempotent replay; the first, more specific outcome wins
	}
	if existing.State != StateRunning {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, existing.State, StateCompleted)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE command_sessions SET state = ?, outcome = ?, exit_code = ?, completed_at = ?, updated_at = ?
		WHERE coordinator_session = ? AND tool_call_id = ?`,
		StateCompleted, outcome, exitCode, now, now, key.CoordinatorSession, key.ToolCallID); err != nil {
		return fmt.Errorf("record terminal outcome: %w", err)
	}
	return tx.Commit()
}

// markReaped records that every descriptor was closed and the exact session
// was waited exactly once. Only reap() calls this, and only after both.
func (s *Store) markReaped(key Key, exitCode *int) error {
	return s.settle(key, StateReaped, exitCode, "")
}

// markSettledAbsent records that the exact PID+start token was proven gone.
func (s *Store) markSettledAbsent(key Key, reason string) error {
	return s.settle(key, StateSettledAbsent, nil, reason)
}

// MarkBlocked records durable BLOCKED evidence and refuses further automatic
// cleanup of this session. Exported because a runner that discovers ambiguity
// outside a sweep must be able to fail closed the same way.
func (s *Store) MarkBlocked(key Key, reason string) error {
	if reason == "" {
		return fmt.Errorf("cmdsession: BLOCKED evidence requires a reason")
	}
	return s.settle(key, StateBlocked, nil, reason)
}

func (s *Store) settle(key Key, to State, exitCode *int, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	existing, err := getFrom(tx, key)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("%w: key=%s", ErrUnknownSession, key)
	}
	if existing.State == to {
		return nil // idempotent replay
	}
	if !existing.Retained() {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, existing.State, to)
	}
	if to == StateReaped && existing.State != StateCompleted {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, existing.State, to)
	}
	now := time.Now().UTC()
	if to == StateReaped {
		_, err = tx.Exec(`UPDATE command_sessions SET state = ?, exit_code = COALESCE(?, exit_code), reaped_at = ?, updated_at = ?
			WHERE coordinator_session = ? AND tool_call_id = ?`,
			to, exitCode, now, now, key.CoordinatorSession, key.ToolCallID)
	} else {
		_, err = tx.Exec(`UPDATE command_sessions SET state = ?, blocked_reason = ?, updated_at = ?
			WHERE coordinator_session = ? AND tool_call_id = ?`,
			to, reason, now, key.CoordinatorSession, key.ToolCallID)
	}
	if err != nil {
		return fmt.Errorf("settle command session: %w", err)
	}
	return tx.Commit()
}

func requireAffected(res sql.Result, key Key) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: key=%s", ErrUnknownSession, key)
	}
	return nil
}

// ListRetained returns every receipt still describing an unaccounted-for
// command session (running or completed-but-unreaped), ordered by id.
func (s *Store) ListRetained() ([]Receipt, error) {
	return s.list(`state IN (?, ?) ORDER BY id ASC`, StateRunning, StateCompleted)
}

// ListBlocked returns every receipt holding durable BLOCKED evidence.
func (s *Store) ListBlocked() ([]Receipt, error) {
	return s.list(`state = ? ORDER BY id ASC`, StateBlocked)
}

// ListAll returns every receipt, ordered by id.
func (s *Store) ListAll() ([]Receipt, error) { return s.list(`1 = 1 ORDER BY id ASC`) }

func (s *Store) list(where string, args ...any) ([]Receipt, error) {
	rows, err := s.db.Query(`SELECT `+receiptColumns+` FROM command_sessions WHERE `+where, args...)
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
	for i := range out {
		n, err := countDetached(s.db, out[i].Key)
		if err != nil {
			return nil, err
		}
		out[i].DetachedOpen = n
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SummaryRow is one command session as fleet status reports it.
type SummaryRow struct {
	Key
	TaskRef       string `json:"task_ref,omitempty"`
	PID           int    `json:"pid"`
	State         State  `json:"state"`
	Outcome       string `json:"outcome,omitempty"`
	CommandDigest string `json:"command_digest"`
	AgeSeconds    int64  `json:"age_seconds"`
	DetachedOpen  int    `json:"detached_open,omitempty"`
	BlockedReason string `json:"blocked_reason,omitempty"`
}

// Summary is the retained-command-session projection fleet status shows, so a
// background terminal cannot hide behind an agent-level working state.
type Summary struct {
	Retained         int          `json:"retained"`
	Blocked          int          `json:"blocked"`
	OldestAgeSeconds int64        `json:"oldest_age_seconds"`
	Rows             []SummaryRow `json:"rows"`
}

// Summarize projects every retained and BLOCKED receipt, oldest first, with
// each one's age measured from spawn by now().
func (s *Store) Summarize(now func() time.Time) (Summary, error) {
	retained, err := s.ListRetained()
	if err != nil {
		return Summary{}, err
	}
	blocked, err := s.ListBlocked()
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{Retained: len(retained), Blocked: len(blocked)}
	for _, r := range append(append([]Receipt{}, retained...), blocked...) {
		age := int64(now().Sub(r.StartedAt).Seconds())
		if age < 0 {
			age = 0
		}
		if age > sum.OldestAgeSeconds {
			sum.OldestAgeSeconds = age
		}
		sum.Rows = append(sum.Rows, SummaryRow{
			Key: r.Key, TaskRef: r.TaskRef, PID: r.PID, State: r.State, Outcome: r.Outcome,
			CommandDigest: r.CommandDigest, AgeSeconds: age, DetachedOpen: r.DetachedOpen,
			BlockedReason: r.BlockedReason,
		})
	}
	sort.SliceStable(sum.Rows, func(i, j int) bool { return sum.Rows[i].AgeSeconds > sum.Rows[j].AgeSeconds })
	return sum, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanReceipt(row rowScanner) (*Receipt, error) {
	var r Receipt
	var laneOwned int
	var pty, taskRef, outcome, blockedReason sql.NullString
	var exitCode sql.NullInt64
	var lastOutputAt, completedAt, reapedAt sql.NullTime
	err := row.Scan(&r.ID, &r.CoordinatorSession, &r.ToolCallID, &r.PID, &r.ParentPID, &r.StartToken,
		&r.ProcessGroup, &pty, &r.CommandDigest, &r.WorkingDir, &taskRef, &laneOwned, &r.StartedAt,
		&r.State, &outcome, &exitCode, &lastOutputAt, &blockedReason, &completedAt, &reapedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.PTY = pty.String
	r.TaskRef = taskRef.String
	r.Outcome = outcome.String
	r.BlockedReason = blockedReason.String
	r.LaneOwned = laneOwned != 0
	if exitCode.Valid {
		code := int(exitCode.Int64)
		r.ExitCode = &code
	}
	r.LastOutputAt = nullTime(lastOutputAt)
	r.CompletedAt = nullTime(completedAt)
	r.ReapedAt = nullTime(reapedAt)
	return &r, nil
}

func nullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
