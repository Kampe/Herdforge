package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrHoldMissing       = errors.New("hold authority state is missing")
	ErrHoldCorrupt       = errors.New("hold authority state is corrupt")
	ErrHoldDenied        = errors.New("hold authority denied action")
	ErrHoldStale         = errors.New("hold authority generation is stale")
	ErrHoldConflict      = errors.New("hold authority generation conflicts")
	ErrHoldReleaseFailed = errors.New("hold authority expiry release failed")
	ErrActiveTaskUnknown = errors.New("active task binding is unknown or ambiguous")
)

// HoldIdentity is the complete identity fence used by every side-effecting
// caller. Empty fields are never wildcard matches.
type HoldIdentity struct {
	Repository string
	Owner      string
	Lane       string
	Task       string
	// Scope distinguishes a lane authority from a task authority. Lane
	// identities intentionally carry an empty task; task identities require it.
	Scope string
}

func (i HoldIdentity) valid() bool {
	if strings.TrimSpace(i.Repository) == "" || strings.TrimSpace(i.Owner) == "" || strings.TrimSpace(i.Lane) == "" {
		return false
	}
	if i.Scope == "lane" {
		return strings.TrimSpace(i.Task) == ""
	}
	return i.Scope == "task" && strings.TrimSpace(i.Task) != ""
}

// HoldDecision is the single read decision shared by all action callers.
type HoldDecision struct {
	Held       bool
	Generation int64
	Reason     string
	Code       string
}

// HoldReader is intentionally narrow so production callers cannot interpret
// raw files, environment booleans, or partial identities themselves.
type HoldReader interface {
	Check(context.Context, HoldIdentity, int64) (HoldDecision, error)
}

// ActiveTaskResolver is the sole binding seam for lane-targeted actions. It
// returns exactly one task identity from authoritative provider/board state.
type ActiveTaskResolver func(context.Context, string) ([]HoldIdentity, error)

// CheckLaneAndTaskHold applies the same lane-then-task admission to kick,
// attention, rescue/recovery, and reap callers.
func CheckLaneAndTaskHold(ctx context.Context, reader HoldReader, resolver ActiveTaskResolver, repository, owner, lane string, generation func(context.Context, HoldIdentity) (int64, error)) error {
	if reader == nil || resolver == nil {
		return fmt.Errorf("%w: authority and resolver are required", ErrActiveTaskUnknown)
	}
	check := func(identity HoldIdentity) error {
		gen := int64(1)
		if generation != nil {
			var err error
			gen, err = generation(ctx, identity)
			if err != nil {
				return err
			}
		}
		decision, err := reader.Check(ctx, identity, gen)
		if err != nil {
			return err
		}
		if decision.Held {
			return fmt.Errorf("held target: %s (%s)", decision.Reason, decision.Code)
		}
		return nil
	}
	if err := check(HoldIdentity{Repository: repository, Owner: owner, Lane: lane, Scope: "lane"}); err != nil {
		return err
	}
	tasks, err := resolver(ctx, lane)
	if err != nil || len(tasks) > 1 {
		return fmt.Errorf("%w: lane=%s", ErrActiveTaskUnknown, lane)
	}
	if len(tasks) == 0 {
		return nil
	}
	task := tasks[0]
	if task.Repository != repository || task.Owner != owner || task.Lane != lane || task.Scope != "task" || strings.TrimSpace(task.Task) == "" {
		return fmt.Errorf("%w: lane=%s", ErrActiveTaskUnknown, lane)
	}
	return check(task)
}

// HoldAuthority persists holds and explicit release events in the canonical
// lifecycle SQLite database. It is safe across processes; SQLite serializes
// the write transaction and the unique event key makes retries idempotent.
type HoldAuthority struct {
	db  *sql.DB
	now func() time.Time
}

// CanonicalStatePath is the one runtime state location shared by CLI,
// dispatch, daemon, lifecycle, kick, claim, and reap. The caller supplies
// the git-common repository root, never a process-relative cwd.
func CanonicalStatePath(repoRoot string) string {
	return filepath.Join(repoRoot, ".herd", "herdforge.db")
}

// CanonicalStatePathForLaunchDB resolves linked worktrees through git's
// common directory, while retaining deterministic temp-repository behavior
// for unit tests that are not git repositories.
func CanonicalStatePathForLaunchDB(launchDB string) (string, error) {
	root := filepath.Dir(filepath.Dir(launchDB))
	out, err := exec.Command("git", "-C", root, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("resolve git-common state root from %q: %w", root, err)
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", errors.New("resolve git-common state root: empty git-common-dir")
	}
	return CanonicalStatePath(filepath.Dir(common)), nil
}

type HoldRecord struct {
	HoldIdentity
	Actor      string
	Reason     string
	Code       string
	Generation int64
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	Held       bool
	ReleasedAt *time.Time
}

func NewHoldAuthority(path string) (*HoldAuthority, error) {
	return newHoldAuthority(path, time.Now)
}

func NewHoldAuthorityWithClock(path string, now func() time.Time) (*HoldAuthority, error) {
	if now == nil {
		return nil, errors.New("hold authority clock is required")
	}
	return newHoldAuthority(path, now)
}

func newHoldAuthority(path string, now func() time.Time) (*HoldAuthority, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("hold authority database path is required")
	}
	db, err := openSQLite(path)
	if err != nil {
		return nil, fmt.Errorf("open hold authority: %w", err)
	}
	a := &HoldAuthority{db: db, now: now}
	if err := a.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return a, nil
}

func (a *HoldAuthority) Close() error { return a.db.Close() }

func (a *HoldAuthority) migrate() error {
	_, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS lifecycle_hold_state (
		repository TEXT NOT NULL, owner TEXT NOT NULL, lane TEXT NOT NULL, task TEXT NOT NULL, scope TEXT NOT NULL DEFAULT 'task',
		actor TEXT NOT NULL, reason TEXT NOT NULL, code TEXT NOT NULL,
		generation INTEGER NOT NULL, created_at DATETIME NOT NULL, expires_at DATETIME,
		held INTEGER NOT NULL, released_at DATETIME,
		PRIMARY KEY(repository, owner, lane, task))`)
	if err != nil {
		return fmt.Errorf("migrate hold state: %w", err)
	}
	if err := ensureHoldColumn(a.db, "lifecycle_hold_state", "scope"); err != nil {
		return err
	}
	_, err = a.db.Exec(`CREATE TABLE IF NOT EXISTS lifecycle_hold_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repository TEXT NOT NULL, owner TEXT NOT NULL, lane TEXT NOT NULL, task TEXT NOT NULL, scope TEXT NOT NULL DEFAULT 'task',
		generation INTEGER NOT NULL, intent TEXT NOT NULL, actor TEXT NOT NULL,
		reason TEXT NOT NULL, code TEXT NOT NULL, created_at DATETIME NOT NULL,
		expires_at DATETIME, UNIQUE(repository, owner, lane, task, generation, intent))`)
	if err != nil {
		return fmt.Errorf("migrate hold events: %w", err)
	}
	if err := ensureHoldColumn(a.db, "lifecycle_hold_events", "scope"); err != nil {
		return err
	}
	return nil
}

func ensureHoldColumn(db *sql.DB, table, column string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("inspect %s schema row: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect %s schema rows: %w", table, err)
	}
	if found {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT 'task'`); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func validateHold(identity HoldIdentity, actor, reason, code string, generation int64) error {
	if !identity.valid() {
		return fmt.Errorf("%w: complete repository/owner/lane/task identity is required", ErrHoldCorrupt)
	}
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || strings.TrimSpace(code) == "" {
		return fmt.Errorf("%w: actor, reason, and code are required", ErrHoldCorrupt)
	}
	if generation <= 0 {
		return fmt.Errorf("%w: generation must be positive", ErrHoldCorrupt)
	}
	return nil
}

func (a *HoldAuthority) Hold(ctx context.Context, identity HoldIdentity, actor, reason, code string, generation int64, expires *time.Time) (HoldRecord, error) {
	if err := validateHold(identity, actor, reason, code, generation); err != nil {
		return HoldRecord{}, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return HoldRecord{}, fmt.Errorf("hold begin: %w", err)
	}
	defer tx.Rollback()
	current, exists, err := readHoldTx(ctx, tx, identity)
	if err != nil {
		return HoldRecord{}, err
	}
	if exists {
		if current.Held && generation > current.Generation {
			return HoldRecord{}, fmt.Errorf("%w: active generation=%d cannot advance to=%d", ErrHoldConflict, current.Generation, generation)
		}
		if generation < current.Generation {
			return HoldRecord{}, fmt.Errorf("%w: current=%d got=%d", ErrHoldStale, current.Generation, generation)
		}
		if generation == current.Generation {
			if current.Held && current.Reason == reason && current.Code == code && sameTime(current.ExpiresAt, expires) {
				return current, nil
			}
			return HoldRecord{}, fmt.Errorf("%w: generation=%d", ErrHoldConflict, generation)
		}
		if generation != current.Generation+1 {
			return HoldRecord{}, fmt.Errorf("%w: next generation=%d got=%d", ErrHoldConflict, current.Generation+1, generation)
		}
	} else if generation != 1 {
		return HoldRecord{}, fmt.Errorf("%w: first generation must be 1, got=%d", ErrHoldConflict, generation)
	}
	now := a.now().UTC()
	if !exists {
		_, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_hold_state(repository,owner,lane,task,scope,actor,reason,code,generation,created_at,expires_at,held,released_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,1,NULL)`, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope, actor, reason, code, generation, now, expires)
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE lifecycle_hold_state SET actor=?,reason=?,code=?,generation=?,created_at=?,expires_at=?,held=1,released_at=NULL WHERE repository=? AND owner=? AND lane=? AND task=? AND scope=? AND generation=?`, actor, reason, code, generation, now, expires, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope, current.Generation)
		if updateErr == nil {
			var n int64
			n, updateErr = result.RowsAffected()
			if n != 1 {
				updateErr = fmt.Errorf("expected one hold transition, affected %d rows", n)
			}
		}
		err = updateErr
	}
	if err != nil {
		return HoldRecord{}, fmt.Errorf("write hold state: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_hold_events(repository,owner,lane,task,scope,generation,intent,actor,reason,code,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope, generation, "hold", actor, reason, code, now, expires); err != nil {
		return HoldRecord{}, fmt.Errorf("record hold event: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return HoldRecord{}, fmt.Errorf("commit hold: %w", err)
	}
	return HoldRecord{HoldIdentity: identity, Actor: actor, Reason: reason, Code: code, Generation: generation, CreatedAt: now, ExpiresAt: cloneTime(expires), Held: true}, nil
}

func (a *HoldAuthority) Release(ctx context.Context, identity HoldIdentity, actor, reason, code string, generation int64) (HoldRecord, error) {
	if err := validateHold(identity, actor, reason, code, generation); err != nil {
		return HoldRecord{}, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return HoldRecord{}, fmt.Errorf("release begin: %w", err)
	}
	defer tx.Rollback()
	current, exists, err := readHoldTx(ctx, tx, identity)
	if err != nil {
		return HoldRecord{}, err
	}
	if !exists {
		return HoldRecord{}, ErrHoldMissing
	}
	if generation != current.Generation {
		if generation < current.Generation {
			return HoldRecord{}, fmt.Errorf("%w: current=%d got=%d", ErrHoldStale, current.Generation, generation)
		}
		return HoldRecord{}, fmt.Errorf("%w: current=%d got=%d", ErrHoldConflict, current.Generation, generation)
	}
	if !current.Held {
		if current.Reason == reason && current.Code == code {
			return current, nil
		}
		return HoldRecord{}, fmt.Errorf("%w: release payload differs", ErrHoldConflict)
	}
	now := a.now().UTC()
	result, updateErr := tx.ExecContext(ctx, `UPDATE lifecycle_hold_state SET held=0,released_at=?,actor=?,reason=?,code=? WHERE repository=? AND owner=? AND lane=? AND task=? AND scope=? AND generation=? AND held=1`, now, actor, reason, code, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope, generation)
	if updateErr == nil {
		var n int64
		n, updateErr = result.RowsAffected()
		if n != 1 {
			updateErr = fmt.Errorf("expected one release transition, affected %d rows", n)
		}
	}
	if updateErr != nil {
		return HoldRecord{}, fmt.Errorf("release state: %w", updateErr)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO lifecycle_hold_events(repository,owner,lane,task,scope,generation,intent,actor,reason,code,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope, generation, "release", actor, reason, code, now); err != nil {
		return HoldRecord{}, fmt.Errorf("record release event: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return HoldRecord{}, fmt.Errorf("commit release: %w", err)
	}
	return HoldRecord{HoldIdentity: identity, Actor: actor, Reason: reason, Code: code, Generation: generation, CreatedAt: current.CreatedAt, ExpiresAt: current.ExpiresAt, Held: false, ReleasedAt: &now}, nil
}

func readHoldTx(ctx context.Context, tx *sql.Tx, identity HoldIdentity) (HoldRecord, bool, error) {
	var r HoldRecord
	var exp, rel sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT repository,owner,lane,task,scope,actor,reason,code,generation,created_at,expires_at,held,released_at FROM lifecycle_hold_state WHERE repository=? AND owner=? AND lane=? AND task=? AND scope=?`, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope).Scan(&r.Repository, &r.Owner, &r.Lane, &r.Task, &r.Scope, &r.Actor, &r.Reason, &r.Code, &r.Generation, &r.CreatedAt, &exp, &r.Held, &rel)
	if err == sql.ErrNoRows {
		return HoldRecord{}, false, nil
	}
	if err != nil {
		return HoldRecord{}, false, fmt.Errorf("%w: read row: %v", ErrHoldCorrupt, err)
	}
	if r.Generation <= 0 || r.Actor == "" || r.Reason == "" || r.Code == "" {
		return HoldRecord{}, false, ErrHoldCorrupt
	}
	if exp.Valid {
		r.ExpiresAt = &exp.Time
	}
	if rel.Valid {
		r.ReleasedAt = &rel.Time
	}
	return r, true, nil
}
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// Check is the one admission decision. Missing/corrupt storage denies; a
// valid identity with no active row is explicitly unheld.
func (a *HoldAuthority) Check(ctx context.Context, identity HoldIdentity, generation int64) (HoldDecision, error) {
	if !identity.valid() {
		return HoldDecision{}, fmt.Errorf("%w: ambiguous identity", ErrHoldDenied)
	}
	if generation <= 0 {
		return HoldDecision{}, fmt.Errorf("%w: positive generation is required", ErrHoldStale)
	}
	var actor, reason, code string
	var currentGen int64
	var held bool
	var expires sql.NullTime
	err := a.db.QueryRowContext(ctx, `SELECT actor,reason,code,generation,held,expires_at FROM lifecycle_hold_state WHERE repository=? AND owner=? AND lane=? AND task=? AND scope=?`, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope).Scan(&actor, &reason, &code, &currentGen, &held, &expires)
	if err == sql.ErrNoRows {
		return HoldDecision{Held: false, Generation: generation, Reason: "no active hold", Code: "unheld"}, nil
	}
	if err != nil {
		return HoldDecision{}, fmt.Errorf("%w: read: %v", ErrHoldDenied, err)
	}
	if actor == "" || reason == "" || code == "" || currentGen <= 0 {
		return HoldDecision{}, fmt.Errorf("%w: invalid row", ErrHoldCorrupt)
	}
	if generation != currentGen {
		return HoldDecision{}, fmt.Errorf("%w: current=%d got=%d", ErrHoldStale, currentGen, generation)
	}
	if held && expires.Valid && !a.now().UTC().Before(expires.Time) {
		if _, err := a.Release(ctx, identity, "hold-expiry", "hold expired", "expired", currentGen); err != nil {
			return HoldDecision{}, fmt.Errorf("%w: %v", ErrHoldReleaseFailed, err)
		}
		return HoldDecision{Held: false, Generation: currentGen, Reason: "expired", Code: "expired"}, nil
	}
	return HoldDecision{Held: held, Generation: currentGen, Reason: reason, Code: code}, nil
}

// CurrentGeneration returns the current durable fence. A never-seen identity
// starts at generation one; callers still pass that positive value to Check.
func (a *HoldAuthority) CurrentGeneration(ctx context.Context, identity HoldIdentity) (int64, error) {
	if !identity.valid() {
		return 0, fmt.Errorf("%w: ambiguous identity", ErrHoldDenied)
	}
	var generation int64
	err := a.db.QueryRowContext(ctx, `SELECT generation FROM lifecycle_hold_state WHERE repository=? AND owner=? AND lane=? AND task=? AND scope=?`, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope).Scan(&generation)
	if err == sql.ErrNoRows {
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("%w: generation read: %v", ErrHoldDenied, err)
	}
	if generation <= 0 {
		return 0, ErrHoldCorrupt
	}
	return generation, nil
}

// HasCurrent reports whether durable state exists for identity. It is kept
// separate from CurrentGeneration because an unseen identity starts at one,
// while a released generation-one identity must advance to generation two.
func (a *HoldAuthority) HasCurrent(ctx context.Context, identity HoldIdentity) (bool, error) {
	if !identity.valid() {
		return false, fmt.Errorf("%w: ambiguous identity", ErrHoldDenied)
	}
	var generation int64
	err := a.db.QueryRowContext(ctx, `SELECT generation FROM lifecycle_hold_state WHERE repository=? AND owner=? AND lane=? AND task=? AND scope=?`, identity.Repository, identity.Owner, identity.Lane, identity.Task, identity.Scope).Scan(&generation)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: generation presence read: %v", ErrHoldDenied, err)
	}
	if generation <= 0 {
		return false, ErrHoldCorrupt
	}
	return true, nil
}

var _ HoldReader = (*HoldAuthority)(nil)
