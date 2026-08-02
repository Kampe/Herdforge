package outbox

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// sqliteBusyTimeoutMillis bounds how long a writer blocked behind another
// writer's lock retries internally before SQLite returns SQLITE_BUSY to
// the caller.
const sqliteBusyTimeoutMillis = 5000

// openSQLite applies the same SQLite concurrency contract as
// pkg/lifecycle's openSQLite (see that package for the full rationale):
// WAL journal mode, a bounded busy_timeout, and a single-connection pool
// so this process's own goroutines queue on Go's pool instead of
// contending for SQLite's single write lock. Correctness under
// concurrency does not depend on this by itself — Claim/MarkSent/
// MarkFailed are all conditioned (CAS) on the expected prior status, so
// even a busy_timeout expiry surfaces as a clear error rather than a
// silently duplicated side effect.
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set journal_mode: %w", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", sqliteBusyTimeoutMillis)); err != nil {
		db.Close()
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	return db, nil
}
