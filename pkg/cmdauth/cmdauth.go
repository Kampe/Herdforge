// Package cmdauth is the compiled command-authorization boundary for
// root/coordinator-issued commands (FAC-195, incident FAC-151).
//
// THE INCIDENT
//
// FAC-151's worker was given an exact one-command authorization with a
// stop-on-first-failure disposition. It ran the guarded command four times
// in one turn, editing code between failures. Nothing in the system could
// have stopped it: the authorization existed only as prompt wording, and
// prompt wording is not an execution control. An agent that decides to
// retry simply retries.
//
// WHAT THIS ENFORCES
//
// An attempt is consumed DURABLY AND ATOMICALLY BEFORE a process is
// created. The budget lives in SQLite, not in a turn, a process, or a
// prompt, so it survives edits, reconnects, resumes, duplicate prompt
// delivery, coordinator restarts, and races between competing workers. Once
// the budget is spent — or a stop-on-first-failure command has returned
// nonzero — every later attempt is refused before exec, and only a NEW
// authorization carrying a DISTINCT command ID can permit another one.
//
// The command hash is not trusted from the caller: Run recomputes it from
// the exact argv and directory it is about to spawn (see exec.go), so an
// authorization for `go test ./pkg/x` cannot be spent on anything else.
//
// FAC-193 SEAM — READ BEFORE TRUSTING LANE/SESSION IDENTITY
//
// Lane and session identity are caller-asserted here. FAC-193's
// pkg/cmdsession IS now merged and is the right authority, but nothing
// calls its Store.Register outside tests and nothing conveys a
// (CoordinatorSession, ToolCallID) to an executing process — so a lookup
// would miss every time, and binding to it today would either refuse all
// execution or prove nothing while looking rigorous.
//
// So this package exposes OwnerProver: a single-method seam Store.Prover
// accepts, consulted inside the same transaction that consumes the attempt
// and failing closed when it errors. Nothing here pretends the proof
// already exists. See docs/fac-195-integration-status.md for the exact
// evidence and the two prerequisites that unblock the binding.
package cmdauth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Disposition is what an authorization says must happen after a nonzero
// exit. It is part of the immutable command packet, not a runtime choice.
type Disposition string

const (
	// StopOnFirstFailure burns the whole authorization the moment the
	// command exits nonzero, whatever budget remained. This is the FAC-151
	// disposition: "run it once; if it fails, stop and report".
	StopOnFirstFailure Disposition = "stop_on_first_failure"
	// ContinueOnFailure lets a failure consume only its own attempt, so
	// the remaining budget stays spendable.
	ContinueOnFailure Disposition = "continue_on_failure"
)

func (d Disposition) valid() bool {
	return d == StopOnFirstFailure || d == ContinueOnFailure
}

// Terminal states. An authorization with a non-empty terminal state can
// never be consumed again, independent of its attempt counter — two
// separate records must both be defeated to replay a spent command.
const (
	TerminalNone       = ""
	TerminalExhausted  = "exhausted"
	TerminalFailedStop = "failed_stop"
	TerminalSuperseded = "superseded"
)

// Receipt events. The ledger is append-only (enforced by SQLite triggers,
// not convention) and is the durable readback for every acceptance claim.
const (
	EventAuthorized = "authorized"
	EventConsumed   = "consumed"
	EventRejected   = "rejected"
	EventSucceeded  = "succeeded"
	EventFailed     = "failed"
	EventSuperseded = "superseded"
)

var (
	// ErrNoAuthorization means no root/coordinator ever authorized this
	// command ID. Absence is a refusal, never a permissive default.
	ErrNoAuthorization = errors.New("cmdauth: no authorization for this command id")
	// ErrHashMismatch means the argv/dir about to be spawned is not the
	// argv/dir that was authorized.
	ErrHashMismatch = errors.New("cmdauth: command does not match the authorized command hash")
	// ErrIdentityMismatch means the lane/session presenting the token is
	// not the lane/session it was issued to.
	ErrIdentityMismatch = errors.New("cmdauth: lane/session identity does not match the authorization")
	// ErrIdentityConflict means a command ID was re-authorized on
	// different terms. An ID cannot silently change what it permits.
	ErrIdentityConflict = errors.New("cmdauth: command id already authorized on different terms")
	// ErrBudgetExhausted means the attempt budget is spent.
	ErrBudgetExhausted = errors.New("cmdauth: attempt budget exhausted")
	// ErrStopOnFailure means a stop-on-first-failure command already
	// returned nonzero. This is the FAC-151 refusal.
	ErrStopOnFailure = errors.New("cmdauth: stop-on-first-failure command already failed")
	// ErrSuperseded means a newer authorization took this lane/session.
	ErrSuperseded = errors.New("cmdauth: authorization superseded by a newer command")
	// ErrLedgerTampered means the authorization row's attempt counter
	// disagrees with the append-only receipt ledger — someone reset or
	// removed attempt consumption.
	ErrLedgerTampered = errors.New("cmdauth: attempt counter disagrees with the append-only receipt ledger")
	// ErrOwnerUnproven means the injected OwnerProver refused the
	// lane/session identity presenting this token.
	ErrOwnerUnproven = errors.New("cmdauth: lane/session ownership could not be proven")
)

// OwnerProver is the FAC-193 seam (see the package comment). A nil Prover
// means identity is caller-asserted and only checked for equality against
// the authorization.
type OwnerProver interface {
	ProveOwner(ctx context.Context, lane, sessionID string) error
}

// Authorization is the immutable command packet a root/coordinator issues.
// Every field is required; there are no wildcards and no defaults.
type Authorization struct {
	CommandID   string      `json:"command_id"`
	CommandHash string      `json:"command_hash"`
	MaxAttempts int         `json:"max_attempts"`
	Authority   string      `json:"authority"`
	Lane        string      `json:"lane"`
	SessionID   string      `json:"session_id"`
	Disposition Disposition `json:"disposition"`
}

func (a Authorization) validate() error {
	switch {
	case strings.TrimSpace(a.CommandID) == "":
		return fmt.Errorf("cmdauth: command id is required")
	case strings.TrimSpace(a.CommandHash) == "":
		return fmt.Errorf("cmdauth: canonical command hash is required")
	case a.MaxAttempts < 1:
		return fmt.Errorf("cmdauth: max attempts must be at least 1")
	case strings.TrimSpace(a.Authority) == "":
		return fmt.Errorf("cmdauth: issuing authority is required")
	case strings.TrimSpace(a.Lane) == "":
		return fmt.Errorf("cmdauth: lane is required")
	case strings.TrimSpace(a.SessionID) == "":
		return fmt.Errorf("cmdauth: session id is required")
	case !a.Disposition.valid():
		return fmt.Errorf("cmdauth: failure disposition must be %q or %q", StopOnFirstFailure, ContinueOnFailure)
	}
	return nil
}

// State is an authorization plus its live consumption.
type State struct {
	Authorization
	AttemptsUsed int       `json:"attempts_used"`
	Terminal     string    `json:"terminal"`
	IssuedAt     time.Time `json:"issued_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Receipt is one append-only ledger row.
type Receipt struct {
	Seq         int64     `json:"seq"`
	CommandID   string    `json:"command_id"`
	CommandHash string    `json:"command_hash"`
	Lane        string    `json:"lane"`
	SessionID   string    `json:"session_id"`
	Authority   string    `json:"authority"`
	Event       string    `json:"event"`
	Attempt     int       `json:"attempt"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	At          time.Time `json:"at"`
}

// Request is what an executor presents at the boundary.
type Request struct {
	CommandID   string
	CommandHash string
	Lane        string
	SessionID   string
}

// Grant is proof that exactly one attempt was durably consumed. It is
// returned only after the consumption is committed.
type Grant struct {
	CommandID   string      `json:"command_id"`
	CommandHash string      `json:"command_hash"`
	Attempt     int         `json:"attempt"`
	MaxAttempts int         `json:"max_attempts"`
	Disposition Disposition `json:"disposition"`
	Lane        string      `json:"lane"`
	SessionID   string      `json:"session_id"`
}

// CanonicalHash is the exact identity of a command: its working directory
// and full argv, length-prefixed so no regrouping of the same bytes
// ("ab","c" vs "a","bc") can collide with a different command.
func CanonicalHash(dir string, argv []string) string {
	h := sha256.New()
	writeField(h, dir)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(argv)))
	_, _ = h.Write(n[:])
	for _, a := range argv {
		writeField(h, a)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(s))
}

const sqliteBusyTimeoutMillis = 10000

// Store is the durable boundary. Each process opens its own Store against
// the same file; correctness under competing workers comes from SQLite's
// own write lock (every consumption runs inside BEGIN IMMEDIATE), never
// from an in-process cache.
type Store struct {
	db *sql.DB
	// Prover is the optional FAC-193 ownership check. Nil means
	// caller-asserted identity (see the package comment).
	Prover OwnerProver
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
}

// DefaultPath is the per-repository ledger location.
func DefaultPath(root string) string {
	return filepath.Join(root, ".herd", "command-authorizations.db")
}

// Open opens (creating if needed) the ledger at path. busy_timeout is set
// in the DSN so it is live before the first statement, including the WAL
// conversion of a brand-new file — the fresh-file race pkg/boardfreeze and
// pkg/claim both had to fix.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("cmdauth: create ledger dir: %w", err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, sqliteBusyTimeoutMillis)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cmdauth: open: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the connection for readback tooling and tamper tests.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS command_authorizations (
			command_id TEXT PRIMARY KEY,
			command_hash TEXT NOT NULL,
			max_attempts INTEGER NOT NULL,
			authority TEXT NOT NULL,
			lane TEXT NOT NULL,
			session_id TEXT NOT NULL,
			disposition TEXT NOT NULL,
			attempts_used INTEGER NOT NULL DEFAULT 0,
			terminal TEXT NOT NULL DEFAULT '',
			issued_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS command_receipts (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			command_id TEXT NOT NULL,
			command_hash TEXT NOT NULL,
			lane TEXT NOT NULL,
			session_id TEXT NOT NULL,
			authority TEXT NOT NULL,
			event TEXT NOT NULL,
			attempt INTEGER NOT NULL DEFAULT 0,
			exit_code INTEGER,
			reason TEXT NOT NULL DEFAULT '',
			at DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_command_receipts_command ON command_receipts(command_id, seq)`,
		// Append-only is enforced by the database, not by discipline: a
		// consumed attempt cannot be edited or deleted away, which is what
		// makes the ledger a usable cross-check on the attempt counter.
		`CREATE TRIGGER IF NOT EXISTS command_receipts_append_only_update
			BEFORE UPDATE ON command_receipts
			BEGIN SELECT RAISE(ABORT, 'cmdauth: command_receipts is append-only'); END`,
		`CREATE TRIGGER IF NOT EXISTS command_receipts_append_only_delete
			BEFORE DELETE ON command_receipts
			BEGIN SELECT RAISE(ABORT, 'cmdauth: command_receipts is append-only'); END`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("cmdauth: migrate: %w", err)
		}
	}
	return nil
}

// tx runs fn inside BEGIN IMMEDIATE on a single pinned connection. IMMEDIATE
// (rather than database/sql's deferred Begin) takes the write lock up front,
// so two workers racing to consume the same last attempt serialize instead of
// both reading attempts_used=0 and one failing to upgrade.
func (s *Store) tx(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("cmdauth: conn: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("cmdauth: begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("cmdauth: commit: %w", err)
	}
	committed = true
	return nil
}

const stateCols = `command_id, command_hash, max_attempts, authority, lane, session_id,
	disposition, attempts_used, terminal, issued_at, updated_at`

func scanState(row *sql.Row) (*State, error) {
	var st State
	err := row.Scan(&st.CommandID, &st.CommandHash, &st.MaxAttempts, &st.Authority, &st.Lane,
		&st.SessionID, &st.Disposition, &st.AttemptsUsed, &st.Terminal, &st.IssuedAt, &st.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cmdauth: read authorization: %w", err)
	}
	return &st, nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadState(ctx context.Context, q queryRower, commandID string) (*State, error) {
	return scanState(q.QueryRowContext(ctx,
		`SELECT `+stateCols+` FROM command_authorizations WHERE command_id = ?`, commandID))
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func appendReceipt(ctx context.Context, e execer, r Receipt) error {
	var exit any
	if r.ExitCode != nil {
		exit = *r.ExitCode
	}
	_, err := e.ExecContext(ctx, `INSERT INTO command_receipts
		(command_id, command_hash, lane, session_id, authority, event, attempt, exit_code, reason, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.CommandID, r.CommandHash, r.Lane, r.SessionID, r.Authority, r.Event, r.Attempt, exit, r.Reason, r.At)
	if err != nil {
		return fmt.Errorf("cmdauth: append receipt: %w", err)
	}
	return nil
}

// Authorize durably records a root/coordinator command packet.
//
// Duplicate delivery of the SAME packet is an idempotent no-op — it must
// never hand back a fresh budget, which is precisely how a retried prompt
// would otherwise launder a spent authorization. The same command ID
// carrying different terms is ErrIdentityConflict. A new packet for a
// lane/session supersedes whatever open authorization that lane still holds,
// so a lane never has two live tokens to choose between.
func (s *Store) Authorize(ctx context.Context, a Authorization) (State, error) {
	if err := a.validate(); err != nil {
		return State{}, err
	}
	var out State
	err := s.tx(ctx, func(conn *sql.Conn) error {
		existing, err := loadState(ctx, conn, a.CommandID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Authorization != a {
				return fmt.Errorf("%w: id=%s", ErrIdentityConflict, a.CommandID)
			}
			out = *existing // replay: budget untouched
			return nil
		}
		now := s.now()
		if err := s.supersedeOpen(ctx, conn, a, now); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO command_authorizations
			(command_id, command_hash, max_attempts, authority, lane, session_id, disposition,
			 attempts_used, terminal, issued_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0, '', ?, ?)`,
			a.CommandID, a.CommandHash, a.MaxAttempts, a.Authority, a.Lane, a.SessionID,
			string(a.Disposition), now, now); err != nil {
			return fmt.Errorf("cmdauth: insert authorization: %w", err)
		}
		if err := appendReceipt(ctx, conn, Receipt{
			CommandID: a.CommandID, CommandHash: a.CommandHash, Lane: a.Lane, SessionID: a.SessionID,
			Authority: a.Authority, Event: EventAuthorized,
			Reason: fmt.Sprintf("max_attempts=%d disposition=%s", a.MaxAttempts, a.Disposition), At: now,
		}); err != nil {
			return err
		}
		st, err := loadState(ctx, conn, a.CommandID)
		if err != nil {
			return err
		}
		out = *st
		return nil
	})
	if err != nil {
		return State{}, err
	}
	return out, nil
}

// supersedeOpen retires every other non-terminal authorization held by the
// same lane/session. Runs inside the caller's transaction.
func (s *Store) supersedeOpen(ctx context.Context, conn *sql.Conn, a Authorization, now time.Time) error {
	rows, err := conn.QueryContext(ctx,
		`SELECT command_id, command_hash, authority FROM command_authorizations
		 WHERE lane = ? AND session_id = ? AND terminal = '' AND command_id <> ?`,
		a.Lane, a.SessionID, a.CommandID)
	if err != nil {
		return fmt.Errorf("cmdauth: scan open authorizations: %w", err)
	}
	type open struct{ id, hash, authority string }
	var stale []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.hash, &o.authority); err != nil {
			rows.Close()
			return fmt.Errorf("cmdauth: scan open authorizations: %w", err)
		}
		stale = append(stale, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("cmdauth: scan open authorizations: %w", err)
	}
	rows.Close()
	for _, o := range stale {
		if _, err := conn.ExecContext(ctx,
			`UPDATE command_authorizations SET terminal = ?, updated_at = ? WHERE command_id = ?`,
			TerminalSuperseded, now, o.id); err != nil {
			return fmt.Errorf("cmdauth: supersede: %w", err)
		}
		if err := appendReceipt(ctx, conn, Receipt{
			CommandID: o.id, CommandHash: o.hash, Lane: a.Lane, SessionID: a.SessionID,
			Authority: o.authority, Event: EventSuperseded,
			Reason: "superseded by command id " + a.CommandID, At: now,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Consume atomically spends exactly one attempt, or refuses. It is the only
// gate: callers MUST NOT create a process unless this returns a Grant.
//
// Every refusal writes a rejection receipt naming the reason, and every
// refusal happens before the caller reaches exec — the rejection path in
// this package touches nothing but the ledger.
func (s *Store) Consume(ctx context.Context, req Request) (Grant, error) {
	if strings.TrimSpace(req.CommandID) == "" || strings.TrimSpace(req.CommandHash) == "" {
		return Grant{}, fmt.Errorf("cmdauth: command id and hash are required to consume an attempt")
	}
	var grant Grant
	// refusal is carried out of the transaction rather than returned from
	// it: a rejection's receipt has to COMMIT, so the closure must succeed
	// even when the attempt is refused. Returning the refusal as a
	// transaction error would roll back the very evidence of it.
	var refusal error
	err := s.tx(ctx, func(conn *sql.Conn) error {
		st, err := loadState(ctx, conn, req.CommandID)
		if err != nil {
			return err
		}
		if st == nil {
			// An unknown ID still earns a durable rejection receipt: an
			// unauthorized execution attempt is exactly the evidence this
			// ledger exists to keep.
			refusal, err = s.reject(ctx, conn, req, Authorization{}, 0, ErrNoAuthorization, "no authorization on record")
			return err
		}
		if st.CommandHash != req.CommandHash {
			refusal, err = s.reject(ctx, conn, req, st.Authorization, st.AttemptsUsed, ErrHashMismatch,
				"presented hash "+short(req.CommandHash)+" but authorization binds "+short(st.CommandHash))
			return err
		}
		if st.Lane != req.Lane || st.SessionID != req.SessionID {
			refusal, err = s.reject(ctx, conn, req, st.Authorization, st.AttemptsUsed, ErrIdentityMismatch,
				fmt.Sprintf("presented lane=%s session=%s but authorization binds lane=%s session=%s",
					req.Lane, req.SessionID, st.Lane, st.SessionID))
			return err
		}
		if s.Prover != nil {
			if proveErr := s.Prover.ProveOwner(ctx, req.Lane, req.SessionID); proveErr != nil {
				refusal, err = s.reject(ctx, conn, req, st.Authorization, st.AttemptsUsed, ErrOwnerUnproven, proveErr.Error())
				return err
			}
		}
		// The append-only ledger is an independent record of how many
		// attempts were actually spent. If the counter no longer agrees with
		// it, consumption has been reset or removed — refuse rather than
		// hand out an attempt the ledger says is already gone.
		var consumed int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM command_receipts WHERE command_id = ? AND event = ?`,
			req.CommandID, EventConsumed).Scan(&consumed); err != nil {
			return fmt.Errorf("cmdauth: count consumed receipts: %w", err)
		}
		if consumed != st.AttemptsUsed {
			refusal, err = s.reject(ctx, conn, req, st.Authorization, st.AttemptsUsed, ErrLedgerTampered,
				fmt.Sprintf("counter says %d attempts used, ledger records %d", st.AttemptsUsed, consumed))
			return err
		}
		switch st.Terminal {
		case TerminalFailedStop:
			refusal, err = s.reject(ctx, conn, req, st.Authorization, st.AttemptsUsed, ErrStopOnFailure,
				"stop-on-first-failure command already returned nonzero; a distinct newly authorized command id is required")
			return err
		case TerminalSuperseded:
			refusal, err = s.reject(ctx, conn, req, st.Authorization, st.AttemptsUsed, ErrSuperseded,
				"a newer authorization holds this lane/session")
			return err
		case TerminalExhausted:
			refusal, err = s.reject(ctx, conn, req, st.Authorization, st.AttemptsUsed, ErrBudgetExhausted,
				fmt.Sprintf("all %d authorized attempts already consumed", st.MaxAttempts))
			return err
		}
		if st.AttemptsUsed >= st.MaxAttempts {
			refusal, err = s.reject(ctx, conn, req, st.Authorization, st.AttemptsUsed, ErrBudgetExhausted,
				fmt.Sprintf("all %d authorized attempts already consumed", st.MaxAttempts))
			return err
		}

		attempt := st.AttemptsUsed + 1
		terminal := TerminalNone
		if attempt >= st.MaxAttempts {
			terminal = TerminalExhausted
		}
		now := s.now()
		if _, err := conn.ExecContext(ctx,
			`UPDATE command_authorizations SET attempts_used = ?, terminal = ?, updated_at = ? WHERE command_id = ?`,
			attempt, terminal, now, req.CommandID); err != nil {
			return fmt.Errorf("cmdauth: consume attempt: %w", err)
		}
		if err := appendReceipt(ctx, conn, Receipt{
			CommandID: st.CommandID, CommandHash: st.CommandHash, Lane: st.Lane, SessionID: st.SessionID,
			Authority: st.Authority, Event: EventConsumed, Attempt: attempt,
			Reason: fmt.Sprintf("attempt %d of %d", attempt, st.MaxAttempts), At: now,
		}); err != nil {
			return err
		}
		grant = Grant{
			CommandID: st.CommandID, CommandHash: st.CommandHash, Attempt: attempt,
			MaxAttempts: st.MaxAttempts, Disposition: st.Disposition,
			Lane: st.Lane, SessionID: st.SessionID,
		}
		return nil
	})
	if err != nil {
		return Grant{}, err
	}
	if refusal != nil {
		return Grant{}, refusal
	}
	return grant, nil
}

// reject appends the rejection receipt and returns (refusal, dbErr). The
// refusal is NOT a transaction error — it is handed back to Consume so the
// receipt can commit; only dbErr aborts. No attempt is consumed on this path.
func (s *Store) reject(ctx context.Context, conn *sql.Conn, req Request, a Authorization, attempt int, sentinel error, reason string) (error, error) {
	// Not named `hash`: that would shadow the stdlib hash package imported
	// for writeField.
	lane, session, authority, cmdHash := req.Lane, req.SessionID, a.Authority, req.CommandHash
	if a.Lane != "" {
		lane = a.Lane
	}
	if a.SessionID != "" {
		session = a.SessionID
	}
	if err := appendReceipt(ctx, conn, Receipt{
		CommandID: req.CommandID, CommandHash: cmdHash, Lane: lane, SessionID: session,
		Authority: authority, Event: EventRejected, Attempt: attempt, Reason: reason, At: s.now(),
	}); err != nil {
		return nil, err
	}
	return fmt.Errorf("%w: %s", sentinel, reason), nil
}

// RecordOutcome durably records how a granted attempt ended. A nonzero exit
// under StopOnFirstFailure burns the remaining budget immediately — that is
// the control FAC-151 needed and did not have.
func (s *Store) RecordOutcome(ctx context.Context, g Grant, exitCode int) error {
	if strings.TrimSpace(g.CommandID) == "" || g.Attempt < 1 {
		return fmt.Errorf("cmdauth: outcome requires a granted command id and attempt")
	}
	return s.tx(ctx, func(conn *sql.Conn) error {
		st, err := loadState(ctx, conn, g.CommandID)
		if err != nil {
			return err
		}
		if st == nil {
			return fmt.Errorf("%w: id=%s", ErrNoAuthorization, g.CommandID)
		}
		now := s.now()
		event := EventSucceeded
		reason := "exited 0"
		if exitCode != 0 {
			event = EventFailed
			reason = fmt.Sprintf("exited %d", exitCode)
		}
		code := exitCode
		if err := appendReceipt(ctx, conn, Receipt{
			CommandID: st.CommandID, CommandHash: st.CommandHash, Lane: st.Lane, SessionID: st.SessionID,
			Authority: st.Authority, Event: event, Attempt: g.Attempt, ExitCode: &code, Reason: reason, At: now,
		}); err != nil {
			return err
		}
		if exitCode != 0 && st.Disposition == StopOnFirstFailure && st.Terminal != TerminalSuperseded {
			if _, err := conn.ExecContext(ctx,
				`UPDATE command_authorizations SET terminal = ?, updated_at = ? WHERE command_id = ?`,
				TerminalFailedStop, now, st.CommandID); err != nil {
				return fmt.Errorf("cmdauth: burn budget on failure: %w", err)
			}
		}
		return nil
	})
}

// Supersede retires an open authorization by root/coordinator decision.
func (s *Store) Supersede(ctx context.Context, commandID, reason string) error {
	return s.tx(ctx, func(conn *sql.Conn) error {
		st, err := loadState(ctx, conn, commandID)
		if err != nil {
			return err
		}
		if st == nil {
			return fmt.Errorf("%w: id=%s", ErrNoAuthorization, commandID)
		}
		if st.Terminal != TerminalNone {
			return nil // already terminal; superseding again changes nothing
		}
		now := s.now()
		if _, err := conn.ExecContext(ctx,
			`UPDATE command_authorizations SET terminal = ?, updated_at = ? WHERE command_id = ?`,
			TerminalSuperseded, now, commandID); err != nil {
			return fmt.Errorf("cmdauth: supersede: %w", err)
		}
		return appendReceipt(ctx, conn, Receipt{
			CommandID: st.CommandID, CommandHash: st.CommandHash, Lane: st.Lane, SessionID: st.SessionID,
			Authority: st.Authority, Event: EventSuperseded, Reason: reason, At: now,
		})
	})
}

// Get returns the durable state for a command ID, or nil if unknown.
func (s *Store) Get(ctx context.Context, commandID string) (*State, error) {
	return loadState(ctx, s.db, commandID)
}

// Receipts returns the append-only ledger in issue order. An empty
// commandID returns every receipt.
func (s *Store) Receipts(ctx context.Context, commandID string) ([]Receipt, error) {
	query := `SELECT seq, command_id, command_hash, lane, session_id, authority, event,
		attempt, exit_code, reason, at FROM command_receipts`
	var args []any
	if commandID != "" {
		query += ` WHERE command_id = ?`
		args = append(args, commandID)
	}
	query += ` ORDER BY seq ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("cmdauth: read receipts: %w", err)
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		var r Receipt
		var exit sql.NullInt64
		if err := rows.Scan(&r.Seq, &r.CommandID, &r.CommandHash, &r.Lane, &r.SessionID,
			&r.Authority, &r.Event, &r.Attempt, &exit, &r.Reason, &r.At); err != nil {
			return nil, fmt.Errorf("cmdauth: read receipts: %w", err)
		}
		if exit.Valid {
			c := int(exit.Int64)
			r.ExitCode = &c
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cmdauth: read receipts: %w", err)
	}
	return out, nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
