package webhook

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type Status string

const (
	// StatusPending is a durably persisted delivery whose handlers have
	// not yet all completed successfully. A retried delivery under the
	// same DeliveryID while pending re-runs handlers rather than being
	// treated as an already-processed duplicate.
	StatusPending Status = "pending"
	// StatusProcessed is a delivery whose handlers all completed. A
	// later delivery reusing the same DeliveryID is a legitimate
	// duplicate (provider retry) and is acknowledged without
	// re-dispatching handlers.
	StatusProcessed Status = "processed"
)

// ErrPayloadConflict is returned by Record when DeliveryID was already
// recorded for a payload with a different hash. A signature check should
// normally prevent this (the attacker would need the secret to forge a
// valid signature over a different payload), but Record fails closed
// instead of silently keeping whichever payload happened to land first.
var ErrPayloadConflict = errors.New("webhook: delivery id reused for a different payload")

// Event is one durably persisted webhook delivery.
type Event struct {
	ID          int64
	DeliveryID  string
	Provider    string
	Type        string
	TaskRef     string
	ProjectID   string
	Payload     string
	PayloadHash string
	Status      Status
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store is the SQLite-backed durable record of inbound webhook
// deliveries. A delivery is persisted BEFORE handlers run (see
// Receiver.ServeHTTP), so a crash between "verified" and "acknowledged"
// leaves durable evidence instead of a silently dropped event.
type Store struct {
	db *sql.DB
}

// NewStore opens (or creates) a SQLite database at path and applies the
// webhook_events schema.
func NewStore(path string) (*Store, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open webhook store: %w", err)
	}
	return NewStoreWithDB(db)
}

// NewStoreWithDB wraps an already-open *sql.DB, applying the
// webhook_events schema.
func NewStoreWithDB(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS webhook_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		delivery_id TEXT NOT NULL UNIQUE,
		provider TEXT,
		event_type TEXT,
		task_ref TEXT,
		project_id TEXT,
		payload TEXT NOT NULL,
		payload_hash TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return fmt.Errorf("migrate webhook schema: %w", err)
	}
	return nil
}

func hashPayload(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// Record durably persists the intent to process deliveryID, or returns
// the already-recorded Event if this exact deliveryID/payload pair was
// seen before (existed=true). Reuse of deliveryID for a DIFFERENT
// payload fails closed with ErrPayloadConflict. The whole check-then-
// insert runs inside one transaction; combined with the single-
// connection pool from openSQLite, SQLite serializes concurrent callers
// so two goroutines racing on the same new deliveryID cannot both
// insert.
func (s *Store) Record(deliveryID, provider, eventType, taskRef, projectID, payload string) (event Event, existed bool, err error) {
	if deliveryID == "" {
		return Event{}, false, fmt.Errorf("record webhook event: delivery_id is required (fail-closed)")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Event{}, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	hash := hashPayload(payload)

	if existing, gerr := getByDeliveryID(tx, deliveryID); gerr != nil {
		return Event{}, false, gerr
	} else if existing != nil {
		if existing.PayloadHash != hash {
			return Event{}, false, fmt.Errorf("%w: delivery_id=%s", ErrPayloadConflict, deliveryID)
		}
		if err := tx.Commit(); err != nil {
			return Event{}, false, fmt.Errorf("commit: %w", err)
		}
		return *existing, true, nil
	}

	now := time.Now().UTC()
	res, err := tx.Exec(
		`INSERT INTO webhook_events (delivery_id, provider, event_type, task_ref, project_id, payload, payload_hash, status, attempts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
		deliveryID, provider, eventType, taskRef, projectID, payload, hash, StatusPending, now, now,
	)
	if err != nil {
		return Event{}, false, fmt.Errorf("insert webhook event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, fmt.Errorf("commit: %w", err)
	}

	id, _ := res.LastInsertId()
	return Event{
		ID: id, DeliveryID: deliveryID, Provider: provider, Type: eventType,
		TaskRef: taskRef, ProjectID: projectID, Payload: payload, PayloadHash: hash,
		Status: StatusPending, CreatedAt: now, UpdatedAt: now,
	}, false, nil
}

// MarkProcessed marks a pending delivery as having had all handlers
// complete successfully. Conditioned on the row's current state being
// pending, so a delivery that was somehow already marked processed
// cannot be re-marked and lose its original attempts/last_error.
func (s *Store) MarkProcessed(deliveryID string) error {
	res, err := s.db.Exec(
		`UPDATE webhook_events SET status = ?, updated_at = ? WHERE delivery_id = ? AND status = ?`,
		StatusProcessed, time.Now().UTC(), deliveryID, StatusPending,
	)
	if err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("mark processed: delivery_id=%s not pending", deliveryID)
	}
	return nil
}

// MarkFailed records a handler failure for a pending delivery, leaving
// it pending so a retried delivery (or a future reconciler) tries
// handlers again rather than being silently dropped.
func (s *Store) MarkFailed(deliveryID, errMsg string) error {
	res, err := s.db.Exec(
		`UPDATE webhook_events SET attempts = attempts + 1, last_error = ?, updated_at = ? WHERE delivery_id = ? AND status = ?`,
		errMsg, time.Now().UTC(), deliveryID, StatusPending,
	)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("mark failed: delivery_id=%s not pending", deliveryID)
	}
	return nil
}

// Get returns the event recorded under deliveryID, or nil if none.
func (s *Store) Get(deliveryID string) (*Event, error) {
	return getByDeliveryID(s.db, deliveryID)
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func getByDeliveryID(q querier, deliveryID string) (*Event, error) {
	row := q.QueryRow(
		`SELECT id, delivery_id, provider, event_type, task_ref, project_id, payload, payload_hash, status, attempts, last_error, created_at, updated_at
		 FROM webhook_events WHERE delivery_id = ?`, deliveryID,
	)
	var ev Event
	var provider, eventType, taskRef, projectID, lastError sql.NullString
	err := row.Scan(
		&ev.ID, &ev.DeliveryID, &provider, &eventType, &taskRef, &projectID,
		&ev.Payload, &ev.PayloadHash, &ev.Status, &ev.Attempts, &lastError,
		&ev.CreatedAt, &ev.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup webhook event: %w", err)
	}
	ev.Provider = provider.String
	ev.Type = eventType.String
	ev.TaskRef = taskRef.String
	ev.ProjectID = projectID.String
	ev.LastError = lastError.String
	return &ev, nil
}
