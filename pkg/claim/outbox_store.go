package claim

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// OutboxStatus is the lifecycle state of a durable outbox record.
type OutboxStatus string

const (
	OutboxPending    OutboxStatus = "pending"
	OutboxInProgress OutboxStatus = "in_progress"
	OutboxApplied    OutboxStatus = "applied"
	OutboxFailed     OutboxStatus = "failed"
)

// OutboxRecord is one durably-tracked side effect and its delivery state.
type OutboxRecord struct {
	ID             int64
	IdempotencyKey string
	Kind           string
	Payload        []byte
	Status         OutboxStatus
	Owner          string
	Attempts       int
	LastError      string
	CreatedAt      time.Time
	ClaimedAt      *time.Time
	AppliedAt      *time.Time
}

// DurableOutbox is the narrow persistence port provider-transition
// intents use, so "we intend to mutate the provider" survives a crash
// between recording the intent and either attempting or confirming it.
// Uses the same atomic-claim shape as LeaseStore's capacity-release
// protocol (Claim/MarkApplied/MarkFailed), for the same reason: two
// concurrent settlers must never both be mid-attempt on the same
// intent, and a crashed settler's claim must be recoverable, not lost.
type DurableOutbox interface {
	// Enqueue durably records intent as Pending if its IdempotencyKey is
	// new; enqueueing an existing key is idempotent and returns the
	// existing record UNCHANGED (a Failed record stays Failed -- use
	// Claim to pick it back up for retry, not a re-Enqueue, so repeatedly
	// calling Enqueue on every attempt can never accidentally erase
	// attempt/failure history).
	Enqueue(ctx context.Context, intent OutboxIntent) (*OutboxRecord, error)

	// Claim atomically claims one Pending/Failed/stale-in_progress record
	// by idempotency key for ownerID, transitioning it to InProgress.
	// Returns (nil, nil) -- not an error -- if the record is not
	// currently claimable (already Applied, or in_progress and owned by
	// someone else within staleAfter).
	Claim(ctx context.Context, idempotencyKey, ownerID string, staleAfter time.Duration, now time.Time) (*OutboxRecord, error)

	// MarkApplied completes ownerID's claim. A no-op if ownerID no
	// longer holds it (a stale claim was reclaimed by someone else).
	MarkApplied(ctx context.Context, idempotencyKey, ownerID string, now time.Time) error

	// MarkFailed releases ownerID's claim back to Failed (immediately
	// retryable, not waiting out staleAfter), recording errMsg. A no-op
	// if ownerID no longer holds the claim.
	MarkFailed(ctx context.Context, idempotencyKey, ownerID, errMsg string, now time.Time) error

	// ForceMarkApplied unconditionally marks idempotencyKey Applied
	// regardless of current owner/state (a no-op if already Applied).
	// For reconciliation only: used after independently verifying
	// ground truth (the provider already reflects the mutation), where
	// ownership fencing doesn't apply because no external call is being
	// made here -- this is a pure local bookkeeping correction.
	ForceMarkApplied(ctx context.Context, idempotencyKey string, now time.Time) error

	// Get returns the current record for idempotencyKey, or nil if none.
	Get(ctx context.Context, idempotencyKey string) (*OutboxRecord, error)

	// Pending returns every record in Pending, Failed, or InProgress
	// state, oldest first, for a reconciliation sweep to inspect.
	Pending(ctx context.Context) ([]*OutboxRecord, error)

	Close() error
}

// SQLiteOutbox is the default DurableOutbox: a SQLite file, same
// busy-safe/atomic-claim approach as SQLiteLeaseStore.
type SQLiteOutbox struct {
	db *sql.DB
}

// NewSQLiteOutbox opens (creating if needed) an outbox database at path.
func NewSQLiteOutbox(path string) (*SQLiteOutbox, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open outbox: %w", err)
	}
	o := &SQLiteOutbox{db: db}
	if err := o.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return o, nil
}

func (o *SQLiteOutbox) Close() error { return o.db.Close() }

func (o *SQLiteOutbox) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			idempotency_key TEXT NOT NULL UNIQUE,
			kind TEXT NOT NULL,
			payload BLOB,
			status TEXT NOT NULL DEFAULT 'pending',
			owner TEXT NOT NULL DEFAULT '',
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			claimed_at DATETIME,
			applied_at DATETIME
		)`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_status ON outbox(status)`,
	}
	for _, m := range migrations {
		if _, err := execWithRetry(context.Background(), o.db, m); err != nil {
			return fmt.Errorf("migrate outbox: %w", err)
		}
	}
	return nil
}

const outboxColumns = `id, idempotency_key, kind, payload, status, owner, attempts, last_error, created_at, claimed_at, applied_at`

func scanOutbox(row interface{ Scan(...any) error }) (*OutboxRecord, error) {
	r := &OutboxRecord{}
	var status string
	var claimedAt, appliedAt sql.NullTime
	err := row.Scan(&r.ID, &r.IdempotencyKey, &r.Kind, &r.Payload, &status, &r.Owner, &r.Attempts, &r.LastError,
		&r.CreatedAt, &claimedAt, &appliedAt)
	if err != nil {
		return nil, err
	}
	r.Status = OutboxStatus(status)
	if claimedAt.Valid {
		t := claimedAt.Time
		r.ClaimedAt = &t
	}
	if appliedAt.Valid {
		t := appliedAt.Time
		r.AppliedAt = &t
	}
	return r, nil
}

func (o *SQLiteOutbox) Get(ctx context.Context, idempotencyKey string) (*OutboxRecord, error) {
	row := o.db.QueryRowContext(ctx, `SELECT `+outboxColumns+` FROM outbox WHERE idempotency_key = ?`, idempotencyKey)
	r, err := scanOutbox(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

func (o *SQLiteOutbox) Enqueue(ctx context.Context, intent OutboxIntent) (*OutboxRecord, error) {
	_, err := execWithRetry(ctx, o.db, `INSERT INTO outbox (idempotency_key, kind, payload, status, created_at)
		VALUES (?, ?, ?, 'pending', ?)
		ON CONFLICT(idempotency_key) DO NOTHING`,
		intent.IdempotencyKey, intent.Kind, intent.Payload, time.Now())
	if err != nil {
		return nil, fmt.Errorf("enqueue outbox intent: %w", err)
	}
	rec, err := o.Get(ctx, intent.IdempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("enqueue outbox intent: reload: %w", err)
	}
	return rec, nil
}

// Claim implements DurableOutbox using the identical UPDATE...RETURNING
// atomic-claim pattern as SQLiteLeaseStore.ClaimCapacityRelease.
func (o *SQLiteOutbox) Claim(ctx context.Context, idempotencyKey, ownerID string, staleAfter time.Duration, now time.Time) (*OutboxRecord, error) {
	staleBefore := now.Add(-staleAfter)
	rows, err := queryWithRetry(ctx, o.db, `UPDATE outbox SET status = 'in_progress', owner = ?, claimed_at = ?, attempts = attempts + 1
		WHERE idempotency_key = ? AND status != 'applied'
		AND (status != 'in_progress' OR claimed_at < ?)
		RETURNING `+outboxColumns,
		ownerID, now, idempotencyKey, staleBefore)
	if err != nil {
		return nil, fmt.Errorf("claim outbox record: %w", err)
	}
	defer rows.Close()

	if rows.Next() {
		return scanOutbox(rows)
	}
	return nil, rows.Err()
}

func (o *SQLiteOutbox) MarkApplied(ctx context.Context, idempotencyKey, ownerID string, now time.Time) error {
	_, err := execWithRetry(ctx, o.db, `UPDATE outbox SET status = 'applied', applied_at = ?
		WHERE idempotency_key = ? AND owner = ? AND status = 'in_progress'`,
		now, idempotencyKey, ownerID)
	if err != nil {
		return fmt.Errorf("mark outbox applied: %w", err)
	}
	return nil
}

func (o *SQLiteOutbox) MarkFailed(ctx context.Context, idempotencyKey, ownerID, errMsg string, now time.Time) error {
	_, err := execWithRetry(ctx, o.db, `UPDATE outbox SET status = 'failed', last_error = ?
		WHERE idempotency_key = ? AND owner = ? AND status = 'in_progress'`,
		errMsg, idempotencyKey, ownerID)
	if err != nil {
		return fmt.Errorf("mark outbox failed: %w", err)
	}
	return nil
}

func (o *SQLiteOutbox) ForceMarkApplied(ctx context.Context, idempotencyKey string, now time.Time) error {
	_, err := execWithRetry(ctx, o.db, `UPDATE outbox SET status = 'applied', applied_at = ?
		WHERE idempotency_key = ? AND status != 'applied'`,
		now, idempotencyKey)
	if err != nil {
		return fmt.Errorf("force mark outbox applied: %w", err)
	}
	return nil
}

func (o *SQLiteOutbox) Pending(ctx context.Context) ([]*OutboxRecord, error) {
	rows, err := o.db.QueryContext(ctx, `SELECT `+outboxColumns+` FROM outbox
		WHERE status IN ('pending', 'failed', 'in_progress')
		ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("pending outbox records: %w", err)
	}
	defer rows.Close()

	var recs []*OutboxRecord
	for rows.Next() {
		r, err := scanOutbox(rows)
		if err != nil {
			return nil, fmt.Errorf("pending outbox records: scan: %w", err)
		}
		recs = append(recs, r)
	}
	return recs, rows.Err()
}

var _ DurableOutbox = (*SQLiteOutbox)(nil)
