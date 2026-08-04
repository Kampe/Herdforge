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

// queryWithRetry is execWithRetry's counterpart for statements that
// return rows (in particular UPDATE...RETURNING, used by
// ClaimCapacityRelease so the atomic claim and reading back exactly what
// was claimed happen in one statement instead of a claim-then-SELECT
// pair that would reopen a race window).
func queryWithRetry(ctx context.Context, db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	var rows *sql.Rows
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		rows, err = db.QueryContext(ctx, query, args...)
		if err == nil || !isBusyErr(err) {
			return rows, err
		}
		time.Sleep(time.Duration(attempt+1) * 25 * time.Millisecond)
	}
	return rows, err
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
			hold_repository TEXT NOT NULL DEFAULT '',
			hold_owner TEXT NOT NULL DEFAULT '',
			hold_lane TEXT NOT NULL DEFAULT '',
			worktree_path TEXT NOT NULL DEFAULT '',
			generation INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			held INTEGER NOT NULL DEFAULT 0,
			claimed_at DATETIME NOT NULL,
			renewed_at DATETIME NOT NULL,
			expires_at DATETIME NOT NULL,
			released_at DATETIME,
			capacity_released_at DATETIME,
			capacity_release_state TEXT NOT NULL DEFAULT 'pending',
			capacity_release_owner TEXT NOT NULL DEFAULT '',
			capacity_release_claimed_at DATETIME,
			provider_lock_owner TEXT NOT NULL DEFAULT '',
			provider_lock_at DATETIME
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_leases_active_key
			ON leases(repo, provider, project, task_ref)
			WHERE status = 'active'`,
		`CREATE INDEX IF NOT EXISTS idx_leases_key
			ON leases(repo, provider, project, task_ref)`,
		`CREATE INDEX IF NOT EXISTS idx_leases_pending_capacity
			ON leases(capacity_release_state, capacity_release_claimed_at)
			WHERE capacity_released_at IS NULL`,
	}
	for _, m := range migrations {
		if _, err := execWithRetry(context.Background(), s.db, m); err != nil {
			return fmt.Errorf("migrate leases: %w", err)
		}
	}
	if err := ensureHoldIdentityColumns(s.db); err != nil {
		return fmt.Errorf("validate hold identity schema: %w", err)
	}
	return nil
}

func ensureHoldIdentityColumns(db *sql.DB) error {
	for pass := 0; pass < 2; pass++ {
		columns, err := leaseIdentityColumns(db)
		if err != nil {
			return err
		}
		missing := false
		for _, name := range []string{"hold_repository", "hold_owner", "hold_lane"} {
			if _, ok := columns[name]; !ok {
				missing = true
				if _, err := execWithRetry(context.Background(), db, `ALTER TABLE leases ADD COLUMN `+name+` TEXT NOT NULL DEFAULT ''`); err != nil {
					return fmt.Errorf("add %s: %w", name, err)
				}
			}
		}
		if !missing {
			break
		}
	}
	columns, err := leaseIdentityColumns(db)
	if err != nil {
		return err
	}
	for _, name := range []string{"hold_repository", "hold_owner", "hold_lane"} {
		c, ok := columns[name]
		if !ok || !strings.EqualFold(strings.TrimSpace(c.typ), "TEXT") || c.notNull != 1 || !c.def.Valid || strings.Trim(strings.TrimSpace(c.def.String), "'\"") != "" {
			return fmt.Errorf("incompatible %s column", name)
		}
	}
	return nil
}

type leaseIdentityColumn struct {
	typ     string
	notNull int
	def     sql.NullString
}

func leaseIdentityColumns(db *sql.DB) (map[string]leaseIdentityColumn, error) {
	rows, err := db.Query(`PRAGMA table_info(leases)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]leaseIdentityColumn{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
			return nil, err
		}
		columns[name] = leaseIdentityColumn{typ: typ, notNull: notNull, def: def}
	}
	return columns, rows.Err()
}

const leaseColumns = `id, repo, provider, project, task_ref, owner_id, role, hold_repository, hold_owner, hold_lane, worktree_path,
	generation, status, held, claimed_at, renewed_at, expires_at, released_at, capacity_released_at`

func scanLease(row interface{ Scan(...any) error }) (*Lease, error) {
	l := &Lease{}
	var status string
	var held int
	var releasedAt, capacityReleasedAt sql.NullTime
	err := row.Scan(&l.ID, &l.Repo, &l.Provider, &l.Project, &l.TaskRef, &l.OwnerID, &l.Role, &l.HoldRepository, &l.HoldOwner, &l.HoldLane,
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

// providerLockStaleAfter bounds how long Release and Acquire's reclaim
// path honor a provider-transition lock (see AcquireProviderLock) before
// treating it as abandoned (its holder crashed) and proceeding anyway.
// Not configurable via the LeaseStore interface -- Release/Acquire's
// signatures are unchanged and heavily depended on elsewhere -- but kept
// generous enough that a normal (fast) provider call never trips it, and
// short enough that a crash doesn't block a lease indefinitely.
// ClaimManager's own WithProviderLockTimeout (which controls how long
// AcquireProviderLock itself honors a *different* stale lock before
// preempting it) defaults to the same value for consistency.
const providerLockStaleAfter = 5 * time.Minute

// expireStaleTestHook, when non-nil, runs once per candidate row right
// before ExpireStale's per-row UPDATE, letting a test deterministically
// land a concurrent Hold/Renew inside the SELECT-to-UPDATE window instead
// of relying on goroutine timing. Always nil outside tests.
var expireStaleTestHook func(candidate *Lease)

// Acquire implements LeaseStore. It first expires any stale active row for
// key (a no-op if the row is still live) — which is also what makes that
// row's capacity token show up as claimable via ClaimCapacityRelease,
// since it now has status Expired and a still-nil CapacityReleasedAt —
// then attempts to insert a new active row. The partial unique index on
// (repo,provider,project,task_ref) WHERE status='active' is the only
// thing that needs to be atomic here: SQLite serializes the competing
// INSERTs itself, so exactly one succeeds regardless of how many
// processes (real OS processes, not just goroutines) race this call.
func (s *SQLiteLeaseStore) Acquire(ctx context.Context, key LeaseKey, ownerID, role, worktreePath string, now time.Time, ttl time.Duration) (*Lease, error) {
	return s.acquire(ctx, key, ownerID, role, worktreePath, "", "", "", now, ttl)
}

func (s *SQLiteLeaseStore) AcquireWithIdentity(ctx context.Context, key LeaseKey, ownerID, role, worktreePath, holdRepository, holdOwner, holdLane string, now time.Time, ttl time.Duration) (*Lease, error) {
	return s.acquire(ctx, key, ownerID, role, worktreePath, holdRepository, holdOwner, holdLane, now, ttl)
}

func (s *SQLiteLeaseStore) acquire(ctx context.Context, key LeaseKey, ownerID, role, worktreePath, holdRepository, holdOwner, holdLane string, now time.Time, ttl time.Duration) (*Lease, error) {
	// Deliberately NOT a staleness carve-out on provider_lock_owner here
	// (see ForceReleaseProviderLock's doc comment): whether it's safe to
	// preempt a stale provider lock by time alone depends on whether a
	// ProviderCAS/fencing scheme is even in play, which only
	// ClaimManager knows. The store unconditionally refuses to evict a
	// locked row; ClaimManager.Claim durably advances the provider fence
	// (or, with no provider configured, force-clears the lock directly --
	// safe, since there's nothing external to protect) BEFORE calling
	// Acquire, so by the time this runs, a preemptable lock has already
	// been cleared and this condition just sees provider_lock_owner = ''.
	if _, err := execWithRetry(ctx, s.db, `UPDATE leases SET status = 'expired'
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND status = 'active' AND held = 0 AND expires_at <= ?
		AND provider_lock_owner = ''`,
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
		(repo, provider, project, task_ref, owner_id, role, hold_repository, hold_owner, hold_lane, worktree_path, generation, status, held, claimed_at, renewed_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', 0, ?, ?, ?)`,
		key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, role, holdRepository, holdOwner, holdLane, worktreePath, gen, claimedAt, claimedAt, expiresAt)
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
			reason := "active and unexpired"
			if existing.Expired(now) {
				reason = "expired but blocked by an in-progress provider transition"
			}
			return nil, &ClaimConflictError{Key: key, Lease: existing, Reason: reason}
		}
		return nil, fmt.Errorf("acquire: insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("acquire: lease id: %w", err)
	}

	return &Lease{
		ID: id, LeaseKey: key, OwnerID: ownerID, Role: role, HoldRepository: holdRepository, HoldOwner: holdOwner, HoldLane: holdLane, WorktreePath: worktreePath,
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
// driven separately by the ClaimCapacityRelease/AckCapacityRelease
// claim/ack protocol (see
// ClaimManager.settlePendingCapacity), not by this boolean, so a retry
// after a capacity-coordinator failure still finds the row pending.
func (s *SQLiteLeaseStore) Release(ctx context.Context, key LeaseKey, ownerID string, generation int64, now time.Time) (*Lease, bool, error) {
	// See Acquire's comment: no staleness carve-out here either. Release
	// is unconditionally blocked while ANY provider lock is held, live or
	// stale; ClaimManager.Release durably preempts a stale one (fence
	// advance, then ForceReleaseProviderLock) before calling this.
	res, err := execWithRetry(ctx, s.db, `UPDATE leases SET status = 'released', released_at = ?
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND owner_id = ? AND generation = ? AND status = 'active'
		AND provider_lock_owner = ''`,
		now, key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, generation)
	if err != nil {
		return nil, false, fmt.Errorf("release: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		l, err := s.byGeneration(ctx, key, ownerID, generation)
		return l, true, err
	}

	// Nothing flipped: figure out whether this is an idempotent replay of
	// an already-released call, a stale/unknown generation, or blocked by
	// a live provider-transition lock (everything else in the WHERE
	// clause matched, so if the row is still active, the lock is why).
	existing, err := s.byGeneration(ctx, key, ownerID, generation)
	if err != nil {
		return nil, false, fmt.Errorf("release: lookup: %w", err)
	}
	if existing != nil && existing.Status == StatusReleased {
		return existing, false, nil
	}
	if existing != nil && existing.Status == StatusActive {
		return nil, false, fmt.Errorf("%w: %s generation %d", ErrProviderTransitionInProgress, key.TaskRef, generation)
	}
	if existing != nil {
		return nil, false, fmt.Errorf("release: %w: generation %d is %s, not active", ErrStaleGeneration, generation, existing.Status)
	}
	return nil, false, s.fencingError(ctx, key, ownerID, generation)
}

// AcquireProviderLock implements LeaseStore. The UPDATE...RETURNING
// statement combines the fencing check (owner_id/generation/status match)
// and the lock acquisition into one atomic operation, so there is no
// window between "verify current" and "lock it" for a concurrent
// Release/reclaim to land in -- unlike a plain read-only check followed
// by a separate write.
func (s *SQLiteLeaseStore) AcquireProviderLock(ctx context.Context, key LeaseKey, ownerID string, generation int64, lockOwner string, staleAfter time.Duration, now time.Time) (*Lease, error) {
	staleBefore := now.Add(-staleAfter)
	rows, err := queryWithRetry(ctx, s.db, `UPDATE leases SET provider_lock_owner = ?, provider_lock_at = ?
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND owner_id = ? AND generation = ? AND status = 'active'
		AND (provider_lock_owner = '' OR provider_lock_owner = ? OR provider_lock_at < ?)
		RETURNING `+leaseColumns,
		lockOwner, now, key.Repo, key.Provider, key.Project, key.TaskRef, ownerID, generation, lockOwner, staleBefore)
	if err != nil {
		return nil, fmt.Errorf("acquire provider lock: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return scanLease(rows)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Nothing matched: distinguish "still current but locked by a live,
	// different transition" from "genuinely not the current lease
	// anymore" (the race this whole mechanism exists to close).
	current, cerr := s.byGeneration(ctx, key, ownerID, generation)
	if cerr != nil {
		return nil, fmt.Errorf("acquire provider lock: lookup: %w", cerr)
	}
	if current != nil && current.Status == StatusActive {
		return nil, fmt.Errorf("%w: %s generation %d", ErrProviderTransitionInProgress, key.TaskRef, generation)
	}
	return nil, s.fencingError(ctx, key, ownerID, generation)
}

// ReleaseProviderLock implements LeaseStore.
func (s *SQLiteLeaseStore) ReleaseProviderLock(ctx context.Context, key LeaseKey, generation int64, lockOwner string) error {
	_, err := execWithRetry(ctx, s.db, `UPDATE leases SET provider_lock_owner = '', provider_lock_at = NULL
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ? AND generation = ? AND provider_lock_owner = ?`,
		key.Repo, key.Provider, key.Project, key.TaskRef, generation, lockOwner)
	if err != nil {
		return fmt.Errorf("release provider lock: %w", err)
	}
	return nil
}

// PeekStaleProviderLock implements LeaseStore.
func (s *SQLiteLeaseStore) PeekStaleProviderLock(ctx context.Context, key LeaseKey, now time.Time) (*Lease, error) {
	staleBefore := now.Add(-providerLockStaleAfter)
	row := s.db.QueryRowContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ?
		AND status = 'active' AND provider_lock_owner != '' AND provider_lock_at <= ?`,
		key.Repo, key.Provider, key.Project, key.TaskRef, staleBefore)
	l, err := scanLease(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return l, err
}

// PeekAllStaleProviderLocks implements LeaseStore: the ExpireStale-scale
// (all keys) counterpart to PeekStaleProviderLock, for
// ClaimManager.ExpireStale's global sweep to durably preempt every
// stale-locked lease before delegating to the store's own (now
// lock-oblivious-to-staleness) ExpireStale.
func (s *SQLiteLeaseStore) PeekAllStaleProviderLocks(ctx context.Context, now time.Time) ([]*Lease, error) {
	staleBefore := now.Add(-providerLockStaleAfter)
	rows, err := s.db.QueryContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE status = 'active' AND provider_lock_owner != '' AND provider_lock_at <= ?
		ORDER BY id ASC`, staleBefore)
	if err != nil {
		return nil, fmt.Errorf("peek all stale provider locks: %w", err)
	}
	defer rows.Close()

	var leases []*Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, fmt.Errorf("peek all stale provider locks: scan: %w", err)
		}
		leases = append(leases, l)
	}
	return leases, rows.Err()
}

// ForceReleaseProviderLock implements LeaseStore: unlike
// ReleaseProviderLock, this clears the lock unconditionally, without
// requiring the caller to be the current lock owner. Reserved for
// ClaimManager's orchestration layer, and only ever called immediately
// after a durably-confirmed provider fence advance for the lease's next
// generation -- never as a substitute for that confirmation.
func (s *SQLiteLeaseStore) ForceReleaseProviderLock(ctx context.Context, key LeaseKey, generation int64) error {
	_, err := execWithRetry(ctx, s.db, `UPDATE leases SET provider_lock_owner = '', provider_lock_at = NULL
		WHERE repo = ? AND provider = ? AND project = ? AND task_ref = ? AND generation = ?`,
		key.Repo, key.Provider, key.Project, key.TaskRef, generation)
	if err != nil {
		return fmt.Errorf("force release provider lock: %w", err)
	}
	return nil
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
// skipped rather than double-counted. Also respects an active,
// non-stale provider-transition lock (see AcquireProviderLock) exactly
// like Release and Acquire's reclaim path do: a lease genuinely past its
// TTL but mid an in-flight ProviderCAS call must not be expired (and
// thus become reclaimable at a new generation) out from under that call
// -- expiring a locked lease would let a concurrent Acquire hand the key
// to a new owner while the old CompleteProviderTransition is still
// running, which is exactly as unsafe as Release racing it. A stale
// (crashed settler) lock is not honored, so this self-heals the same
// way Release/Acquire's reclaim path does.
// ExpireStale never preempts a provider lock by time alone -- see
// Acquire's comment. ClaimManager.ExpireStale durably preempts every
// stale-locked lease (fence advance, then ForceReleaseProviderLock)
// before calling this, so a row this method ever sees with
// provider_lock_owner != ” is genuinely still protected and correctly
// left alone.
func (s *SQLiteLeaseStore) ExpireStale(ctx context.Context, now time.Time) ([]*Lease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE status = 'active' AND held = 0 AND expires_at <= ?
		AND provider_lock_owner = ''`, now)
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
			WHERE id = ? AND status = 'active' AND held = 0 AND expires_at <= ?
			AND provider_lock_owner = ''`, l.ID, now)
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

// ClaimCapacityRelease implements LeaseStore. The UPDATE...RETURNING
// statement is both the atomic claim and the read of exactly what got
// claimed in one operation: SQLite executes a write statement (including
// one that returns rows) as a single atomic unit under its single-writer
// lock, so no other connection's write can interleave between rows of
// this statement or between "claim" and "read back". That is the mutual
// exclusion primitive that fixes concurrent double-delivery: two
// settlers racing this call can never both see the same lease in their
// claimed batch, because only one of their UPDATEs can be the one that
// actually flips a given row's owner (the WHERE clause only matches rows
// still pending, or in_progress with a stale claimed_at).
func (s *SQLiteLeaseStore) ClaimCapacityRelease(ctx context.Context, settlerID string, staleAfter time.Duration, now time.Time, key *LeaseKey) ([]*Lease, error) {
	staleBefore := now.Add(-staleAfter)
	query := `UPDATE leases SET capacity_release_state = 'in_progress', capacity_release_owner = ?, capacity_release_claimed_at = ?
		WHERE status IN ('released', 'expired') AND capacity_released_at IS NULL
		AND (capacity_release_state = 'pending' OR (capacity_release_state = 'in_progress' AND capacity_release_claimed_at < ?))`
	args := []any{settlerID, now, staleBefore}
	if key != nil {
		query += ` AND repo = ? AND provider = ? AND project = ? AND task_ref = ?`
		args = append(args, key.Repo, key.Provider, key.Project, key.TaskRef)
	}
	query += ` RETURNING ` + leaseColumns

	rows, err := queryWithRetry(ctx, s.db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("claim capacity release: %w", err)
	}
	defer rows.Close()

	var claimed []*Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, fmt.Errorf("claim capacity release: scan: %w", err)
		}
		claimed = append(claimed, l)
	}
	return claimed, rows.Err()
}

// AckCapacityRelease implements LeaseStore. Guarded on
// (capacity_release_owner = settlerID AND capacity_release_state =
// 'in_progress') so acking after a stale claim was reclaimed by a
// different settler is a silent no-op rather than clobbering that
// settler's in-flight claim.
func (s *SQLiteLeaseStore) AckCapacityRelease(ctx context.Context, leaseID int64, settlerID string, now time.Time) error {
	_, err := execWithRetry(ctx, s.db, `UPDATE leases SET capacity_release_state = 'done', capacity_released_at = ?
		WHERE id = ? AND capacity_release_owner = ? AND capacity_release_state = 'in_progress'`,
		now, leaseID, settlerID)
	if err != nil {
		return fmt.Errorf("ack capacity release: %w", err)
	}
	return nil
}

// FailCapacityRelease implements LeaseStore, guarded the same way as
// AckCapacityRelease.
func (s *SQLiteLeaseStore) FailCapacityRelease(ctx context.Context, leaseID int64, settlerID string) error {
	_, err := execWithRetry(ctx, s.db, `UPDATE leases SET capacity_release_state = 'pending', capacity_release_owner = '', capacity_release_claimed_at = NULL
		WHERE id = ? AND capacity_release_owner = ? AND capacity_release_state = 'in_progress'`,
		leaseID, settlerID)
	if err != nil {
		return fmt.Errorf("fail capacity release: %w", err)
	}
	return nil
}
