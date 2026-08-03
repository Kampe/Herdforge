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
	// StatusPending is a durably persisted delivery that has not yet
	// been claimed for processing, or whose most recent claim's
	// handlers failed. A later Claim under the same DeliveryID while
	// pending re-runs handlers rather than being treated as an
	// already-processed duplicate.
	StatusPending Status = "pending"
	// StatusInFlight is a short-lived claim state: exactly one caller
	// has Claim'd this delivery and is running its handlers. No other
	// caller may claim it while it's here (see Claim's CAS) — this is
	// what makes two concurrent requests for the same delivery id
	// dispatch handlers at most once between them, not twice.
	StatusInFlight Status = "in_flight"
	// StatusProcessed is a delivery whose handlers all completed. A
	// later delivery reusing the same DeliveryID is a legitimate
	// duplicate (provider retry) and is acknowledged without
	// re-dispatching handlers.
	StatusProcessed Status = "processed"
)

// ErrPayloadConflict is returned by Claim when DeliveryID was already
// recorded for a payload with a different hash. A signature check should
// normally prevent this (the attacker would need the secret to forge a
// valid signature over a different payload), but Claim fails closed
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

// Claim atomically persists deliveryID (if new) and grants exclusive
// ownership of running its handlers, or reports that it could not
// (claimed=false). This single method — rather than a separate
// "record" step followed later by a separate "mark in-flight" step —
// is what closes the race between two concurrent requests for the same
// delivery id: the decision "do I get to run handlers" is made and
// committed atomically with the persistence write, inside one
// transaction. Combined with the single-connection pool from
// openSQLite, SQLite serializes concurrent callers, so a second Begin()
// for the same deliveryID cannot observe the row until the first
// transaction has committed.
//
//   - Unknown deliveryID: inserted as StatusInFlight directly and
//     claimed=true — this caller owns it.
//   - Known deliveryID, same payload, StatusPending (a previous claim's
//     handlers failed): CAS to StatusInFlight, claimed=true — this
//     caller retries the handlers.
//   - Known deliveryID, same payload, StatusInFlight (another caller is
//     mid-handler right now): claimed=false, event.Status=InFlight —
//     the caller must NOT run handlers; it lost the race.
//   - Known deliveryID, same payload, StatusProcessed: claimed=false,
//     event.Status=Processed — a legitimate duplicate delivery,
//     already fully handled.
//   - Known deliveryID, DIFFERENT payload (any status): fails closed
//     with ErrPayloadConflict.
func (s *Store) Claim(deliveryID, provider, eventType, taskRef, projectID, payload string) (event Event, claimed bool, err error) {
	if deliveryID == "" {
		return Event{}, false, fmt.Errorf("claim webhook event: delivery_id is required (fail-closed)")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Event{}, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	hash := hashPayload(payload)
	now := time.Now().UTC()

	existing, gerr := getByDeliveryID(tx, deliveryID)
	if gerr != nil {
		return Event{}, false, gerr
	}

	if existing == nil {
		res, err := tx.Exec(
			`INSERT INTO webhook_events (delivery_id, provider, event_type, task_ref, project_id, payload, payload_hash, status, attempts, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)`,
			deliveryID, provider, eventType, taskRef, projectID, payload, hash, StatusInFlight, now, now,
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
			Status: StatusInFlight, CreatedAt: now, UpdatedAt: now,
		}, true, nil
	}

	if existing.PayloadHash != hash {
		return Event{}, false, fmt.Errorf("%w: delivery_id=%s", ErrPayloadConflict, deliveryID)
	}

	if existing.Status != StatusPending {
		// StatusInFlight (lost the race) or StatusProcessed (duplicate
		// of completed work): report the current state, claim nothing.
		if err := tx.Commit(); err != nil {
			return Event{}, false, fmt.Errorf("commit: %w", err)
		}
		return *existing, false, nil
	}

	res, err := tx.Exec(
		`UPDATE webhook_events SET status = ?, updated_at = ? WHERE delivery_id = ? AND status = ?`,
		StatusInFlight, now, deliveryID, StatusPending,
	)
	if err != nil {
		return Event{}, false, fmt.Errorf("claim webhook event: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Event{}, false, fmt.Errorf("claim webhook event: delivery_id=%s not pending", deliveryID)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, fmt.Errorf("commit: %w", err)
	}
	existing.Status = StatusInFlight
	existing.UpdatedAt = now
	return *existing, true, nil
}

// MarkProcessed marks a claimed (in-flight) delivery as having had all
// handlers complete successfully. Conditioned on the row's current
// state being in_flight, so it can only be called by whichever caller
// actually won the Claim.
func (s *Store) MarkProcessed(deliveryID string) error {
	res, err := s.db.Exec(
		`UPDATE webhook_events SET status = ?, updated_at = ? WHERE delivery_id = ? AND status = ?`,
		StatusProcessed, time.Now().UTC(), deliveryID, StatusInFlight,
	)
	if err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("mark processed: delivery_id=%s not in flight", deliveryID)
	}
	return nil
}

// MarkFailed records a handler failure for a claimed (in-flight)
// delivery, returning it to StatusPending so a retried delivery (or a
// future reconciler) can Claim and try handlers again rather than the
// event being silently dropped.
func (s *Store) MarkFailed(deliveryID, errMsg string) error {
	res, err := s.db.Exec(
		`UPDATE webhook_events SET status = ?, attempts = attempts + 1, last_error = ?, updated_at = ? WHERE delivery_id = ? AND status = ?`,
		StatusPending, errMsg, time.Now().UTC(), deliveryID, StatusInFlight,
	)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("mark failed: delivery_id=%s not in flight", deliveryID)
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
