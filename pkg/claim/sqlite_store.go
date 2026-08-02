package claim

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteLeaseStore is the default LeaseStore: a SQLite file shared by every
// Herdforge process on the box. SQLite's own file locking (one writer at a
// time, serialized via the OS) is what makes Acquire/Renew/Release/Hold
// atomic across processes, not anything in this Go code — the partial
// unique index below is the mutual-exclusion primitive.
type SQLiteLeaseStore struct {
	db *sql.DB
}

// NewSQLiteLeaseStore opens (creating if needed) a lease database at path.
// Use a real file path, not ":memory:", when leases must be visible across
// processes.
func NewSQLiteLeaseStore(path string) (*SQLiteLeaseStore, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open lease store: %w", err)
	}
	s := &SQLiteLeaseStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteLeaseStore) Close() error { return s.db.Close() }

func (s *SQLiteLeaseStore) migrate() error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS leases (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			provider TEXT NOT NULL,
			project TEXT NOT NULL,
			task_ref TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			role TEXT NOT NULL,
			worktree_path TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			held INTEGER NOT NULL DEFAULT 0,
			claimed_at DATETIME NOT NULL,
			renewed_at DATETIME NOT NULL,
			expires_at DATETIME NOT NULL,
			released_at DATETIME
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_leases_active_key
			ON leases(repo, provider, project, task_ref)
			WHERE status = 'active'`,
		`CREATE INDEX IF NOT EXISTS idx_leases_key
			ON leases(repo, provider, project, task_ref)`,
	}
	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migrate leases: %w", err)
		}
	}
	return nil
}

const leaseColumns = `id, repo, provider, project, task_ref, owner_id, role, worktree_path,
	generation, status, held, claimed_at, renewed_at, expires_at, released_at`

func scanLease(row interface{ Scan(...any) error }) (*Lease, error) {
	l := &Lease{}
	var status string
	var held int
	var releasedAt sql.NullTime
	err := row.Scan(&l.ID, &l.Repo, &l.Provider, &l.Project, &l.TaskRef, &l.OwnerID, &l.Role,
		&l.WorktreePath, &l.Generation, &status, &held, &l.ClaimedAt, &l.RenewedAt, &l.ExpiresAt, &releasedAt)
	if err != nil {
		return nil, err
	}
	l.Status = LeaseStatus(status)
	l.Held = held != 0
	if releasedAt.Valid {
		t := releasedAt.Time
		l.ReleasedAt = &t
	}
	return l, nil
}

func (s *SQLiteLeaseStore) currentActive(ctx context.Context, key LeaseKey) (*Lease, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ? AND status = 'active'`,
		key.Repo, key.Provider, key.Project, key.TaskRef)
	l, err := scanLease(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

func (s *SQLiteLeaseStore) latestGeneration(ctx context.Context, key LeaseKey) (int64, error) {
	var gen sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(generation) FROM leases
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?`,
		key.Repo, key.Provider, key.Project, key.TaskRef).Scan(&gen)
	if err != nil {
		return 0, err
	}
	return gen.Int64, nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}

// Acquire implements LeaseStore. It first expires any stale active row for
// key (a no-op if the row is still live), then attempts to insert a new
// active row. The partial unique index on (repo,provider,project,task_ref)
// WHERE status='active' is the only thing that needs to be atomic here:
// SQLite serializes the competing INSERTs itself, so exactly one succeeds
// regardless of how many processes race this call.
func (s *SQLiteLeaseStore) Acquire(ctx context.Context, key LeaseKey, ownerID, role, worktreePath string, now time.Time, ttl time.Duration) (*Lease, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE leases SET status = 'expired'
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND status = 'active' AND held = 0 AND expires_at <= ?`,
		key.Repo, key.Provider, key.Project, key.TaskRef, now); err != nil {
		return nil, fmt.Errorf("acquire: expire stale: %w", err)
	}

	gen, err := s.latestGeneration(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("acquire: next generation: %w", err)
	}
	gen++

	claimedAt, expiresAt := now, now.Add(ttl)
	res, err := s.db.ExecContext(ctx, `INSERT INTO leases
		(repo, provider, project, task_ref, owner_id, role, worktree_path, generation, status, held, claimed_at, renewed_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', 0, ?, ?, ?)`,
		key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, role, worktreePath, gen, claimedAt, claimedAt, expiresAt)
	if err != nil {
		if isUniqueViolation(err) {
			existing, lookupErr := s.currentActive(ctx, key)
			if lookupErr != nil {
				return nil, fmt.Errorf("acquire: conflict lookup: %w", lookupErr)
			}
			if existing == nil {
				// Lost a tight race after the UNIQUE failure but before
				// re-reading; the winner has since released. Caller retries.
				return nil, fmt.Errorf("acquire: %w: transient contention, retry", ErrAlreadyClaimed)
			}
			return nil, &ClaimConflictError{Key: key, Lease: existing, Reason: "active and unexpired"}
		}
		return nil, fmt.Errorf("acquire: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("acquire: lease id: %w", err)
	}

	return &Lease{
		ID: id, LeaseKey: key, OwnerID: ownerID, Role: role, WorktreePath: worktreePath,
		Generation: gen, Status: StatusActive, ClaimedAt: claimedAt, RenewedAt: claimedAt, ExpiresAt: expiresAt,
	}, nil
}

func (s *SQLiteLeaseStore) Renew(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time, ttl time.Duration) (*Lease, error) {
	expiresAt := now.Add(ttl)
	res, err := s.db.ExecContext(ctx, `UPDATE leases SET renewed_at = ?, expires_at = ?
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND owner_id = ? AND generation = ? AND status = 'active'`,
		now, expiresAt, key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, generation)
	if err != nil {
		return nil, fmt.Errorf("renew: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return s.currentActive(ctx, key)
	}
	return nil, s.fencingError(ctx, key, ownerID, generation)
}

// Release implements LeaseStore. transitioned=true only for the call that
// actually flips the row from active to released, so capacity accounting
// stays exactly-once under concurrent/duplicate release calls.
func (s *SQLiteLeaseStore) Release(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time) (*Lease, bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE leases SET status = 'released', released_at = ?
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND owner_id = ? AND generation = ? AND status = 'active'`,
		now, key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, generation)
	if err != nil {
		return nil, false, fmt.Errorf("release: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		l, err := s.byGeneration(ctx, key, ownerID, generation)
		return l, true, err
	}

	// Nothing flipped: figure out whether this is an idempotent replay of
	// an already-released call, or a stale/unknown generation.
	existing, err := s.byGeneration(ctx, key, ownerID, generation)
	if err != nil {
		return nil, false, fmt.Errorf("release: lookup: %w", err)
	}
	if existing != nil && existing.Status == StatusReleased {
		return existing, false, nil
	}
	if existing != nil {
		return nil, false, fmt.Errorf("release: %w: generation %d is %s, not active", ErrStaleGeneration, generation, existing.Status)
	}
	return nil, false, s.fencingError(ctx, key, ownerID, generation)
}

func (s *SQLiteLeaseStore) byGeneration(ctx context.Context, key LeaseKey, ownerID string, generation int64) (*Lease, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ? AND owner_id = ? AND generation = ?
		ORDER BY id DESC LIMIT 1`,
		key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, generation)
	l, err := scanLease(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

// fencingError distinguishes "a newer generation now owns this key"
// (stale fencing token) from "no such lease ever existed" (not found).
func (s *SQLiteLeaseStore) fencingError(ctx context.Context, key LeaseKey, ownerID string, generation int64) error {
	active, err := s.currentActive(ctx, key)
	if err != nil {
		return fmt.Errorf("fencing lookup: %w", err)
	}
	if active != nil {
		return fmt.Errorf("%w: active generation is %d, caller had %d", ErrStaleGeneration, active.Generation, generation)
	}
	return fmt.Errorf("%w: no lease for %s owned by %s at generation %d", ErrNotFound, key.TaskRef, ownerID, generation)
}

func (s *SQLiteLeaseStore) Hold(ctx context.Context, key LeaseKey, held bool, now time.Time) (*Lease, error) {
	h := 0
	if held {
		h = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE leases SET held = ?, renewed_at = ?
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ? AND status = 'active'`,
		h, now, key.Repo, key.Provider, key.Project, key.TaskRef)
	if err != nil {
		return nil, fmt.Errorf("hold: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("hold: %w: no active lease for %s", ErrNotFound, key.TaskRef)
	}
	return s.currentActive(ctx, key)
}

// ExpireStale transitions active-but-expired, unheld leases to Expired one
// row at a time, guarded by `WHERE status = 'active'` so a lease that a
// concurrent ExpireStale (in this or another process) already flipped is
// simply skipped rather than double-counted.
func (s *SQLiteLeaseStore) ExpireStale(ctx context.Context, now time.Time) ([]*Lease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE status = 'active' AND held = 0 AND expires_at <= ?`, now)
	if err != nil {
		return nil, fmt.Errorf("expire stale: candidates: %w", err)
	}
	var candidates []*Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("expire stale: scan: %w", err)
		}
		candidates = append(candidates, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var transitioned []*Lease
	for _, l := range candidates {
		res, err := s.db.ExecContext(ctx, `UPDATE leases SET status = 'expired'
			WHERE id = ? AND status = 'active'`, l.ID)
		if err != nil {
			return nil, fmt.Errorf("expire stale: transition %d: %w", l.ID, err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			l.Status = StatusExpired
			transitioned = append(transitioned, l)
		}
	}
	return transitioned, nil
}

func (s *SQLiteLeaseStore) ActiveClaims(ctx context.Context, now time.Time) ([]*Lease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE status = 'active' AND (held = 1 OR expires_at > ?)
		ORDER BY claimed_at ASC`, now)
	if err != nil {
		return nil, fmt.Errorf("active claims: %w", err)
	}
	defer rows.Close()

	var leases []*Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, fmt.Errorf("active claims: scan: %w", err)
		}
		leases = append(leases, l)
	}
	return leases, rows.Err()
}
