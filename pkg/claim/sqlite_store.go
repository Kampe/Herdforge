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
// processes. Safe to call concurrently from multiple OS processes racing
// to create the same brand-new database file: busy_timeout is set before
// any other statement runs, and migrate() additionally retries on
// SQLITE_BUSY/"database is locked" so a first-writer-wins WAL-mode
// conversion race on a fresh file doesn't surface as an open error.
func NewSQLiteLeaseStore(path string) (*SQLiteLeaseStore, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)", path)
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

// isBusyErr matches SQLite's lock-contention errors. busy_timeout already
// makes SQLite itself block-and-retry internally for the configured
// window, but execWithRetry adds an application-level retry on top so a
// fresh-file open/migrate race across several real OS processes (which
// can briefly exceed even a generous busy_timeout while WAL mode is being
// established) degrades to a short additional wait instead of a hard
// error.
func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "database table is locked")
}

func execWithRetry(ctx context.Context, db *sql.DB, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		res, err = db.ExecContext(ctx, query, args...)
		if err == nil || !isBusyErr(err) {
			return res, err
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
	return res, err
}

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
			released_at DATETIME,
			capacity_released_at DATETIME
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_leases_active_key
			ON leases(repo, provider, project, task_ref)
			WHERE status = 'active'`,
		`CREATE INDEX IF NOT EXISTS idx_leases_key
			ON leases(repo, provider, project, task_ref)`,
		`CREATE INDEX IF NOT EXISTS idx_leases_pending_capacity
			ON leases(status)
			WHERE capacity_released_at IS NULL`,
	}
	for _, m := range migrations {
		if _, err := execWithRetry(context.Background(), s.db, m); err != nil {
			return fmt.Errorf("migrate leases: %w", err)
		}
	}
	return nil
}

const leaseColumns = `id, repo, provider, project, task_ref, owner_id, role, worktree_path,
	generation, status, held, claimed_at, renewed_at, expires_at, released_at, capacity_released_at`

func scanLease(row interface{ Scan(...any) error }) (*Lease, error) {
	l := &Lease{}
	var status string
	var held int
	var releasedAt, capacityReleasedAt sql.NullTime
	err := row.Scan(&l.ID, &l.Repo, &l.Provider, &l.Project, &l.TaskRef, &l.OwnerID, &l.Role,
		&l.WorktreePath, &l.Generation, &status, &held, &l.ClaimedAt, &l.RenewedAt, &l.ExpiresAt,
		&releasedAt, &capacityReleasedAt)
	if err != nil {
		return nil, err
	}
	l.Status = LeaseStatus(status)
	l.Held = held != 0
	if releasedAt.Valid {
		t := releasedAt.Time
		l.ReleasedAt = &t
	}
	if capacityReleasedAt.Valid {
		t := capacityReleasedAt.Time
		l.CapacityReleasedAt = &t
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

// expireStaleTestHook, when non-nil, runs once per candidate row right
// before ExpireStale's per-row UPDATE, letting a test deterministically
// land a concurrent Hold/Renew inside the SELECT-to-UPDATE window instead
// of relying on goroutine timing. Always nil outside tests.
var expireStaleTestHook func(candidate *Lease)

// Acquire implements LeaseStore. It first expires any stale active row for
// key (a no-op if the row is still live) — which is also what makes that
// row's capacity token show up in PendingCapacityRelease, since it now
// has status Expired and a still-nil CapacityReleasedAt — then attempts
// to insert a new active row. The partial unique index on
// (repo,provider,project,task_ref) WHERE status='active' is the only
// thing that needs to be atomic here: SQLite serializes the competing
// INSERTs itself, so exactly one succeeds regardless of how many
// processes (real OS processes, not just goroutines) race this call.
func (s *SQLiteLeaseStore) Acquire(ctx context.Context, key LeaseKey, ownerID, role, worktreePath string, now time.Time, ttl time.Duration) (*Lease, error) {
	if _, err := execWithRetry(ctx, s.db, `UPDATE leases SET status = 'expired'
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
	res, err := execWithRetry(ctx, s.db, `INSERT INTO leases
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

// Renew requires the lease to still be unexpired (or held) at the moment
// of renewal, not merely still status='active' in the database — an
// active-but-past-TTL row that no Acquire/ExpireStale has evicted yet
// must not be silently extended by its old owner.
func (s *SQLiteLeaseStore) Renew(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time, ttl time.Duration) (*Lease, error) {
	expiresAt := now.Add(ttl)
	res, err := execWithRetry(ctx, s.db, `UPDATE leases SET renewed_at = ?, expires_at = ?
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND owner_id = ? AND generation = ? AND status = 'active'
		AND (held = 1 OR expires_at > ?)`,
		now, expiresAt, key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, generation, now)
	if err != nil {
		return nil, fmt.Errorf("renew: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return s.currentActive(ctx, key)
	}
	return nil, s.renewFailureError(ctx, key, ownerID, generation, now)
}

// renewFailureError distinguishes stale generation, already-expired, and
// not-found so Renew's caller can react accordingly (stop working vs.
// re-claim).
func (s *SQLiteLeaseStore) renewFailureError(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time) error {
	l, err := s.byGeneration(ctx, key, ownerID, generation)
	if err != nil {
		return fmt.Errorf("renew lookup: %w", err)
	}
	if l == nil {
		return s.fencingError(ctx, key, ownerID, generation)
	}
	if l.Status != StatusActive {
		return fmt.Errorf("%w: generation %d is %s, not active", ErrStaleGeneration, generation, l.Status)
	}
	if !l.Held && !now.Before(l.ExpiresAt) {
		return fmt.Errorf("%w: lease expired at %s", ErrLeaseExpired, l.ExpiresAt.Format(time.RFC3339))
	}
	return s.fencingError(ctx, key, ownerID, generation)
}

// Release implements LeaseStore. transitioned=true only for the call that
// actually flips the row from active to released; capacity settlement is
// driven separately by PendingCapacityRelease/MarkCapacityReleased (see
// ClaimManager.settlePendingCapacity), not by this boolean, so a retry
// after a capacity-coordinator failure still finds the row pending.
func (s *SQLiteLeaseStore) Release(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time) (*Lease, bool, error) {
	res, err := execWithRetry(ctx, s.db, `UPDATE leases SET status = 'released', released_at = ?
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

// Hold is fenced exactly like Renew/Release: only the current owner at
// the current generation may set or clear operator hold. A caller with a
// stale generation (or no lease at all) is rejected rather than being
// able to hold/unhold a lease it does not currently own.
func (s *SQLiteLeaseStore) Hold(ctx context.Context, key LeaseKey, ownerID string, generation int64, held bool, now time.Time) (*Lease, error) {
	h := 0
	if held {
		h = 1
	}
	res, err := execWithRetry(ctx, s.db, `UPDATE leases SET held = ?, renewed_at = ?
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND owner_id = ? AND generation = ? AND status = 'active'`,
		h, now, key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, generation)
	if err != nil {
		return nil, fmt.Errorf("hold: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, s.fencingError(ctx, key, ownerID, generation)
	}
	return s.currentActive(ctx, key)
}

// ExpireStale transitions active-but-expired, unheld leases to Expired one
// row at a time. The per-row UPDATE re-checks held/expiry in its own
// predicate (not just relying on the earlier candidate SELECT), so a
// Renew or Hold that lands between candidate selection and the row
// transition wins the race: the UPDATE simply matches zero rows for that
// id instead of expiring a lease that was, in the same instant, renewed
// or held. Guarded by `WHERE status = 'active'` too, so a lease already
// flipped by a concurrent ExpireStale (in this or another process) is
// skipped rather than double-counted.
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
		if expireStaleTestHook != nil {
			expireStaleTestHook(l)
		}
		res, err := execWithRetry(ctx, s.db, `UPDATE leases SET status = 'expired'
			WHERE id = ? AND status = 'active' AND held = 0 AND expires_at <= ?`, l.ID, now)
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

// PendingCapacityRelease returns every Released/Expired lease whose
// capacity token has not yet been durably marked returned, across all
// keys, oldest first so a reconciliation sweep drains in FIFO order.
func (s *SQLiteLeaseStore) PendingCapacityRelease(ctx context.Context) ([]*Lease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE status IN ('released', 'expired') AND capacity_released_at IS NULL
		ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("pending capacity release: %w", err)
	}
	defer rows.Close()

	var leases []*Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, fmt.Errorf("pending capacity release: scan: %w", err)
		}
		leases = append(leases, l)
	}
	return leases, rows.Err()
}

// MarkCapacityReleased durably records that leaseID's capacity token has
// been returned. Guarded by `capacity_released_at IS NULL` so calling it
// twice (e.g. two concurrent settlers both succeeded in calling the
// coordinator in a narrow race) is a harmless no-op the second time
// rather than clobbering the first timestamp.
func (s *SQLiteLeaseStore) MarkCapacityReleased(ctx context.Context, leaseID int64, now time.Time) error {
	_, err := execWithRetry(ctx, s.db, `UPDATE leases SET capacity_released_at = ?
		WHERE id = ? AND capacity_released_at IS NULL`, now, leaseID)
	if err != nil {
		return fmt.Errorf("mark capacity released: %w", err)
	}
	return nil
}
