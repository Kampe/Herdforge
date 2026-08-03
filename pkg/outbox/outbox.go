// Package outbox implements the transactional-outbox pattern for
// Herdforge's lifecycle side effects (provider board mutations, Herdr
// dispatch, Git operations). An Item is enqueued in the SAME database
// transaction as the lifecycle event that caused it, so a crash between
// "decide to act" and "durably record the intent to act" is impossible.
// A Relay later claims and delivers pending items to per-kind Handlers,
// deduplicating on IdempotencyKey so replays never repeat a side effect.
package outbox

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Status string

const (
	StatusPending Status = "pending"
	// StatusInFlight is a short-lived claim state: a Relay has taken this
	// item and is calling its Handler. No other Relay may claim it while
	// it's here (see Claim's CAS).
	StatusInFlight     Status = "in_flight"
	StatusSent         Status = "sent"
	StatusFailed       Status = "failed"
	StatusAcknowledged Status = "acknowledged"
	StatusSuperseded   Status = "superseded"
)

var (
	// ErrIdempotencyConflict is returned when an idempotency key is reused
	// for an item whose task, kind, or payload differs from the one
	// originally enqueued under that key. Reuse for the SAME task/kind/
	// payload is a legitimate replay and returns the original item with no
	// error; anything else fails closed instead of silently keeping
	// whichever payload happened to land first.
	ErrIdempotencyConflict = errors.New("outbox: idempotency key reused for a different item")
	// ErrNotClaimable is returned by Claim when the item is no longer
	// pending (already claimed by another Relay, already sent, or already
	// failed terminally).
	ErrNotClaimable = errors.New("outbox: item is not claimable")
	// ErrNotInFlight is returned by MarkSent/MarkFailed when the item
	// wasn't in the in_flight state they expect — e.g. two Relays raced
	// and the other one already resolved it.
	ErrNotInFlight = errors.New("outbox: item is not in_flight")
	ErrNotSent     = errors.New("outbox: item is not sent")
)

// Item is one durable side-effect intent.
type Item struct {
	ID             int64  `json:"id"`
	IdempotencyKey string `json:"idempotency_key"`
	TaskRef        string `json:"task_ref"`
	Kind           string `json:"kind"`
	Payload        string `json:"payload,omitempty"`
	Status         Status `json:"status"`
	Attempts       int    `json:"attempts"`
	LastError      string `json:"last_error,omitempty"`
	// NextAttemptAt gates retry: Pending never returns a pending item
	// whose NextAttemptAt is in the future. Nil means immediately
	// eligible (a fresh item's first attempt).
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Owner         string     `json:"owner,omitempty"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	MessageID     string     `json:"message_id,omitempty"`
	Sequence      int64      `json:"sequence,omitempty"`
}

// Store is the SQLite-backed outbox persistence.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) a SQLite database at path, applies the
// SQLite concurrency contract (see openSQLite), and applies the outbox
// schema.
func NewStore(path string) (*Store, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open outbox store: %w", err)
	}
	return NewStoreWithDB(db)
}

// NewStoreWithDB wraps an already-open *sql.DB, applying the outbox
// schema. Used by lifecycle.Machine so events and outbox items share one
// connection and can be written in a single transaction.
func NewStoreWithDB(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// DB returns the underlying connection so callers (Machine) can compose a
// single transaction across the outbox store and the lifecycle event store.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS outbox_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		idempotency_key TEXT NOT NULL UNIQUE,
		task_ref TEXT NOT NULL,
		kind TEXT NOT NULL,
		payload TEXT,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		next_attempt_at DATETIME,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		owner TEXT,
		claimed_at DATETIME,
		message_id TEXT,
		sequence INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return fmt.Errorf("migrate outbox schema: %w", err)
	}
	for _, alter := range []string{`ALTER TABLE outbox_items ADD COLUMN owner TEXT`, `ALTER TABLE outbox_items ADD COLUMN claimed_at DATETIME`, `ALTER TABLE outbox_items ADD COLUMN message_id TEXT`, `ALTER TABLE outbox_items ADD COLUMN sequence INTEGER NOT NULL DEFAULT 0`} {
		if _, alterErr := s.db.Exec(alter); alterErr != nil && !strings.Contains(strings.ToLower(alterErr.Error()), "duplicate column") {
			return fmt.Errorf("migrate outbox delivery columns: %w", alterErr)
		}
	}
	if _, err := s.db.Exec(`UPDATE outbox_items SET owner = 'legacy-unowned', claimed_at = CURRENT_TIMESTAMP WHERE status = ? AND claimed_at IS NULL`, StatusInFlight); err != nil {
		return fmt.Errorf("migrate legacy claims: %w", err)
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_outbox_items_status_kind ON outbox_items(status, kind)`)
	if err != nil {
		return fmt.Errorf("migrate outbox schema: %w", err)
	}
	return nil
}

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// EnqueueTx durably records the intent to perform one side effect, within
// an existing transaction. If idempotency_key was already enqueued for
// the exact same task/kind/payload, the original item is returned
// unchanged (idempotent no-op) — a replayed lifecycle transition never
// enqueues a duplicate side effect. Reuse of the key for a DIFFERENT
// task/kind/payload fails closed with ErrIdempotencyConflict instead of
// silently keeping whichever one was enqueued first.
func (s *Store) EnqueueTx(tx *sql.Tx, item Item) (Item, error) {
	return enqueue(tx, item)
}

// Enqueue is a convenience wrapper for callers that don't need to combine
// the enqueue with another write in the same transaction.
func (s *Store) Enqueue(item Item) (Item, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, fmt.Errorf("begin: %w", err)
	}
	out, err := enqueue(tx, item)
	if err != nil {
		tx.Rollback()
		return Item{}, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// GetByKey returns the existing durable item without creating one. Terminal
// acknowledgement paths use this lookup so a forged receipt cannot create a
// phantom order that was never durably delivered.
func (s *Store) GetByKey(key string) (*Item, error) { return getByKey(s.db, key) }

func enqueue(e execer, item Item) (Item, error) {
	if item.IdempotencyKey == "" {
		return Item{}, fmt.Errorf("enqueue outbox item: idempotency_key is required (fail-closed)")
	}
	if item.Kind == "" {
		return Item{}, fmt.Errorf("enqueue outbox item: kind is required")
	}

	if existing, err := getByKey(e, item.IdempotencyKey); err != nil {
		return Item{}, err
	} else if existing != nil {
		if existing.TaskRef != item.TaskRef || existing.Kind != item.Kind || existing.Payload != item.Payload {
			return Item{}, fmt.Errorf("%w: key=%s", ErrIdempotencyConflict, item.IdempotencyKey)
		}
		return *existing, nil
	}

	now := time.Now().UTC()
	item.Status = StatusPending
	item.CreatedAt = now
	item.UpdatedAt = now

	res, err := e.Exec(
		`INSERT INTO outbox_items (idempotency_key, task_ref, kind, payload, status, attempts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		item.IdempotencyKey, item.TaskRef, item.Kind, item.Payload, item.Status, item.CreatedAt, item.UpdatedAt,
	)
	if err != nil {
		return Item{}, fmt.Errorf("insert outbox item: %w", err)
	}
	item.ID, _ = res.LastInsertId()
	return item, nil
}

func getByKey(e execer, key string) (*Item, error) {
	row := e.QueryRow(
		`SELECT id, idempotency_key, task_ref, kind, payload, status, attempts, last_error, next_attempt_at, created_at, updated_at, owner, claimed_at, message_id, sequence
		 FROM outbox_items WHERE idempotency_key = ?`, key,
	)
	item, err := scanItem(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup outbox item: %w", err)
	}
	return item, nil
}

// Get returns one item by ID.
func (s *Store) Get(id int64) (*Item, error) {
	row := s.db.QueryRow(
		`SELECT id, idempotency_key, task_ref, kind, payload, status, attempts, last_error, next_attempt_at, created_at, updated_at, owner, claimed_at, message_id, sequence
		 FROM outbox_items WHERE id = ?`, id,
	)
	return scanItem(row)
}

// Pending returns items that are pending AND due (NextAttemptAt is nil or
// <= now), optionally filtered by kind (empty kind means all kinds),
// oldest first. It does not claim them — a Relay must call Claim before
// invoking a Handler, so two Relays racing on the same batch don't both
// dispatch the same side effect.
func (s *Store) Pending(kind string, limit int, now time.Time) ([]Item, error) {
	// next_attempt_at is always stored in UTC (see MarkFailed); comparing
	// against a non-UTC "now" would compare two differently-offset RFC3339
	// strings lexically, which is not a valid ordering. Normalize here so
	// callers don't have to remember.
	now = now.UTC()

	var rows *sql.Rows
	var err error
	if kind == "" {
		rows, err = s.db.Query(
			`SELECT id, idempotency_key, task_ref, kind, payload, status, attempts, last_error, next_attempt_at, created_at, updated_at, owner, claimed_at, message_id, sequence
			 FROM outbox_items
			 WHERE status = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
			 ORDER BY id ASC LIMIT ?`, StatusPending, now, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, idempotency_key, task_ref, kind, payload, status, attempts, last_error, next_attempt_at, created_at, updated_at, owner, claimed_at, message_id, sequence
			 FROM outbox_items
			 WHERE status = ? AND kind = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
			 ORDER BY id ASC LIMIT ?`, StatusPending, kind, now, limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("pending: %w", err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, fmt.Errorf("scan outbox item: %w", err)
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// Claim atomically moves one item from pending to in_flight. Exactly one
// caller wins a given item: the UPDATE is conditioned on status='pending',
// so a second Relay racing for the same row gets ErrNotClaimable and must
// move on rather than also invoking the Handler.
func (s *Store) Claim(id int64) (Item, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(`UPDATE outbox_items SET status = ?, updated_at = ? WHERE id = ? AND status = ?`, StatusInFlight, now, id, StatusPending)
	if err != nil {
		return Item{}, fmt.Errorf("claim outbox item: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Item{}, fmt.Errorf("%w: id=%d", ErrNotClaimable, id)
	}
	item, err := s.Get(id)
	if err != nil {
		return Item{}, err
	}
	if item == nil {
		return Item{}, fmt.Errorf("claim outbox item: id=%d vanished after claim", id)
	}
	return *item, nil
}

// ClaimOwned takes a pending item, or atomically takes over an in-flight item
// whose claim has expired. The owner and timestamp are changed in the same
// CAS, so a live claimant cannot be reset by reconciliation.
func (s *Store) ClaimOwned(id int64, owner string, staleAfter time.Duration, now time.Time) (Item, error) {
	if owner == "" {
		return Item{}, fmt.Errorf("claim outbox item: owner is required")
	}
	if staleAfter <= 0 {
		return Item{}, fmt.Errorf("claim outbox item: positive staleAfter is required")
	}
	cutoff := now.UTC().Add(-staleAfter)
	res, err := s.db.Exec(
		`UPDATE outbox_items SET status = ?, owner = ?, claimed_at = ?, updated_at = ?
		 WHERE id = ? AND (status = ? OR (status = ? AND claimed_at IS NOT NULL AND claimed_at <= ?))`,
		StatusInFlight, owner, now.UTC(), now.UTC(), id, StatusPending, StatusInFlight, cutoff,
	)
	if err != nil {
		return Item{}, fmt.Errorf("claim outbox item: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Item{}, fmt.Errorf("%w: id=%d", ErrNotClaimable, id)
	}
	item, err := s.Get(id)
	if err != nil {
		return Item{}, err
	}
	if item == nil {
		return Item{}, fmt.Errorf("claim outbox item: id=%d vanished after claim", id)
	}
	return *item, nil
}

// RecordDelivery persists the authoritative stable mailbox identity while
// the item is still owned and in flight. A restart can therefore resume with
// the original sequence instead of inventing a zero or a new message.
func (s *Store) RecordDelivery(id int64, owner, messageID string, sequence int64) error {
	if owner == "" || messageID == "" || sequence <= 0 {
		return fmt.Errorf("outbox: complete delivery identity is required")
	}
	res, err := s.db.Exec(`UPDATE outbox_items SET message_id = ?, sequence = ?, updated_at = ? WHERE id = ? AND status = ? AND owner = ?`, messageID, sequence, time.Now().UTC(), id, StatusInFlight, owner)
	if err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: id=%d owner=%s", ErrNotInFlight, id, owner)
	}
	return nil
}

// MarkSent marks a claimed (in_flight) item delivered. The UPDATE is
// conditioned on status='in_flight': if another Relay already resolved
// this item (shouldn't happen since Claim is exclusive, but this is the
// backstop), MarkSent fails closed with ErrNotInFlight instead of
// silently overwriting a terminal state.
func (s *Store) MarkSent(id int64) error {
	res, err := s.db.Exec(
		`UPDATE outbox_items SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		StatusSent, time.Now().UTC(), id, StatusInFlight,
	)
	if err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: id=%d", ErrNotInFlight, id)
	}
	return nil
}

func (s *Store) MarkSentOwned(id int64, owner string) error {
	res, err := s.db.Exec(`UPDATE outbox_items SET status = ?, updated_at = ? WHERE id = ? AND status = ? AND owner = ?`, StatusSent, time.Now().UTC(), id, StatusInFlight, owner)
	if err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: id=%d owner=%s", ErrNotInFlight, id, owner)
	}
	return nil
}

// MarkAcknowledged and MarkSuperseded are terminal protocol evidence.  They
// are deliberately separate from StatusSent: a Herdr prompt is only a wake
// hint, while the recipient's acknowledgement (or an explicit supersession)
// is what permits a coordinator to finalize an order.
func (s *Store) MarkAcknowledged(id int64) error {
	return s.markTerminal(id, StatusAcknowledged)
}

func (s *Store) MarkSuperseded(id int64) error {
	return s.markTerminal(id, StatusSuperseded)
}

func (s *Store) markTerminal(id int64, status Status) error {
	res, err := s.db.Exec(
		`UPDATE outbox_items SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		status, time.Now().UTC(), id, StatusSent,
	)
	if err != nil {
		return fmt.Errorf("mark %s: %w", status, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: id=%d", ErrNotSent, id)
	}
	return nil
}

// MarkFailed records a delivery failure for a claimed (in_flight) item.
// Below maxAttempts it returns to pending with a bounded exponential
// backoff (see nextAttemptDelay) before Pending will offer it again; at
// or above maxAttempts it moves to the terminal Failed status. Like
// MarkSent, the UPDATE is conditioned on status='in_flight' and fails
// closed with ErrNotInFlight otherwise.
func (s *Store) MarkFailed(id int64, errMsg string, maxAttempts int) error {
	item, err := s.Get(id)
	if err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("mark failed: outbox item %d not found", id)
	}
	if item.Status != StatusInFlight {
		return fmt.Errorf("%w: id=%d", ErrNotInFlight, id)
	}

	attempts := item.Attempts + 1
	status := StatusPending
	now := time.Now().UTC()
	var nextAttemptAt any
	if maxAttempts > 0 && attempts >= maxAttempts {
		status = StatusFailed
		nextAttemptAt = nil
	} else {
		next := now.Add(nextAttemptDelay(attempts))
		nextAttemptAt = next
	}

	res, err := s.db.Exec(
		`UPDATE outbox_items SET status = ?, attempts = ?, last_error = ?, next_attempt_at = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		status, attempts, errMsg, nextAttemptAt, now, id, StatusInFlight,
	)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: id=%d", ErrNotInFlight, id)
	}
	return nil
}

// nextAttemptDelay is a bounded exponential backoff: 1s, 2s, 4s, 8s, ...
// capped at maxBackoff, keyed on the attempt count AFTER this failure.
const (
	baseBackoff = time.Second
	maxBackoff  = 5 * time.Minute
)

func nextAttemptDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return baseBackoff
	}
	d := baseBackoff
	for i := 0; i < attempts-1; i++ {
		d *= 2
		if d >= maxBackoff {
			return maxBackoff
		}
	}
	return d
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (*Item, error) {
	var item Item
	var payload, lastError, owner, messageID sql.NullString
	var nextAttemptAt sql.NullTime
	var claimedAt sql.NullTime
	err := row.Scan(
		&item.ID, &item.IdempotencyKey, &item.TaskRef, &item.Kind, &payload,
		&item.Status, &item.Attempts, &lastError, &nextAttemptAt, &item.CreatedAt, &item.UpdatedAt, &owner, &claimedAt, &messageID, &item.Sequence,
	)
	if err != nil {
		return nil, err
	}
	item.Payload = payload.String
	item.LastError = lastError.String
	item.Owner = owner.String
	item.MessageID = messageID.String
	if claimedAt.Valid {
		t := claimedAt.Time
		item.ClaimedAt = &t
	}
	if nextAttemptAt.Valid {
		t := nextAttemptAt.Time
		item.NextAttemptAt = &t
	}
	return &item, nil
}
