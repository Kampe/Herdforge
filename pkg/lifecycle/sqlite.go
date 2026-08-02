package lifecycle

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// sqliteBusyTimeoutMillis bounds how long a writer blocked behind another
// writer's lock retries internally before SQLite returns SQLITE_BUSY to
// the caller.
const sqliteBusyTimeoutMillis = 5000

// SQLite concurrency contract for every *sql.DB this package opens
// (openSQLite is the only call site of sql.Open):
//
//   - journal_mode=WAL: readers never block writers and writers never
//     block readers. This does NOT mean concurrent writers — SQLite still
//     allows only one writer transaction in flight at a time across ALL
//     connections and ALL processes sharing this database file. WAL only
//     changes how readers are treated while that writer runs.
//   - busy_timeout=5000ms (sqliteBusyTimeoutMillis): a writer that can't
//     immediately acquire the write lock retries internally for up to 5s
//     before the call returns SQLITE_BUSY. This package does not add its
//     own retry loop on top — a caller that gets SQLITE_BUSY, or the
//     application-level ErrConcurrentModification from AppendTx once
//     inside a transaction, is expected to retry the whole Machine.
//     Transition call with a fresh read, not resume the failed one.
//   - SetMaxOpenConns(1): a single *sql.DB never fans this process's own
//     concurrent callers across more than one pooled connection. Without
//     this, two goroutines in the SAME process could each grab a
//     different pooled connection and contend for SQLite's single write
//     lock exactly like two separate processes would, burning
//     busy_timeout for no reason — Go's connection pool queues them
//     instead.
//
// Correctness under concurrency does not depend on any of the above by
// itself: AppendTx derives FromState/seq from a read taken inside its own
// transaction and CAS-updates lifecycle_task_state against that read's
// seq, so even if two processes race with these settings pushed to their
// limits, only one transaction can ever commit a given seq.
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
