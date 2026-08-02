// Package outbox implements the transactional-outbox pattern for
// Herdforge's lifecycle side effects (provider board mutations, Herdr
// dispatch, Git operations). An Item is enqueued in the SAME database
// transaction as the lifecycle event that caused it, so a crash between
// "decide to act" and "durably record the intent to act" is impossible.
// A Relay later delivers pending items to per-kind Handlers, deduplicating
// on IdempotencyKey so replays never repeat a side effect.
package outbox

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusSent    Status = "sent"
	StatusFailed  Status = "failed"
)

// Item is one durable side-effect intent.
type Item struct {
	ID             int64     `json:"id"`
	IdempotencyKey string    `json:"idempotency_key"`
	TaskRef        string    `json:"task_ref"`
	Kind           string    `json:"kind"`
	Payload        string    `json:"payload,omitempty"`
	Status         Status    `json:"status"`
	Attempts       int       `json:"attempts"`
	LastError      string    `json:"last_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Store is the SQLite-backed outbox persistence.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) a SQLite database at path and applies the
// outbox schema.
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
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
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return fmt.Errorf("migrate outbox schema: %w", err)
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
// an existing transaction. If idempotency_key was already enqueued, the
// original item is returned unchanged (idempotent no-op) — a replayed
// lifecycle transition never enqueues a duplicate side effect.
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
		`SELECT id, idempotency_key, task_ref, kind, payload, status, attempts, last_error, created_at, updated_at
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
		`SELECT id, idempotency_key, task_ref, kind, payload, status, attempts, last_error, created_at, updated_at
		 FROM outbox_items WHERE id = ?`, id,
	)
	return scanItem(row)
}

// Pending returns pending items, optionally filtered by kind (empty kind
// means all kinds), oldest first.
func (s *Store) Pending(kind string, limit int) ([]Item, error) {
	var rows *sql.Rows
	var err error
	if kind == "" {
		rows, err = s.db.Query(
			`SELECT id, idempotency_key, task_ref, kind, payload, status, attempts, last_error, created_at, updated_at
			 FROM outbox_items WHERE status = ? ORDER BY id ASC LIMIT ?`, StatusPending, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, idempotency_key, task_ref, kind, payload, status, attempts, last_error, created_at, updated_at
			 FROM outbox_items WHERE status = ? AND kind = ? ORDER BY id ASC LIMIT ?`, StatusPending, kind, limit,
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

// MarkSent marks an item delivered. Delivery is terminal and idempotent —
// marking an already-sent item sent again is a no-op.
func (s *Store) MarkSent(id int64) error {
	_, err := s.db.Exec(
		`UPDATE outbox_items SET status = ?, updated_at = ? WHERE id = ?`,
		StatusSent, time.Now().UTC(), id,
	)
	return err
}

// MarkFailed records a delivery failure. Below maxAttempts the item stays
// pending for the next relay pass; at or above maxAttempts it moves to the
// terminal Failed status and stops being picked up by Pending.
func (s *Store) MarkFailed(id int64, errMsg string, maxAttempts int) error {
	item, err := s.Get(id)
	if err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("mark failed: outbox item %d not found", id)
	}
	attempts := item.Attempts + 1
	status := StatusPending
	if maxAttempts > 0 && attempts >= maxAttempts {
		status = StatusFailed
	}
	_, err = s.db.Exec(
		`UPDATE outbox_items SET status = ?, attempts = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, attempts, errMsg, time.Now().UTC(), id,
	)
	return err
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanItem(row rowScanner) (*Item, error) {
	var item Item
	var payload, lastError sql.NullString
	err := row.Scan(
		&item.ID, &item.IdempotencyKey, &item.TaskRef, &item.Kind, &payload,
		&item.Status, &item.Attempts, &lastError, &item.CreatedAt, &item.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	item.Payload = payload.String
	item.LastError = lastError.String
	return &item, nil
}
