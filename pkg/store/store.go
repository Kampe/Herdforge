package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type PulseRecord struct {
	ID          int64      `json:"id"`
	TaskRef     string     `json:"task_ref"`
	TaskID      string     `json:"task_id"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	ClaimedAt   time.Time  `json:"claimed_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type ClaimedTask struct {
	ID          int64     `json:"id"`
	TaskRef     string    `json:"task_ref"`
	TaskID      string    `json:"task_id"`
	Role        string    `json:"role"`
	LaneName    string    `json:"lane_name"`
	ClaimedAt   time.Time `json:"claimed_at"`
	Description string    `json:"description"`
}

type LaneRuntime struct {
	ID        int64     `json:"id"`
	LaneName  string    `json:"lane_name"`
	Status    string    `json:"status"`
	TabID     string    `json:"tab_id"`
	StartedAt time.Time `json:"started_at"`
}

// BlockedRecord is durable evidence that dependency selection held back a task.
// Ref+TaskID is the stable board identity; Code+Reason is the fail-closed cause.
type BlockedRecord struct {
	ID               int64     `json:"id"`
	Ref              string    `json:"ref"`
	TaskID           string    `json:"task_id"`
	Entrypoint       string    `json:"entrypoint"`
	Code             string    `json:"code"`
	Reason           string    `json:"reason"`
	GraphRevision    string    `json:"graph_revision,omitempty"`
	ProviderRevision string    `json:"provider_revision,omitempty"`
	RecordedAt       time.Time `json:"recorded_at"`
	RecencySeq       int64     `json:"recency_seq"`
}

// BlockedSelection is an attention item produced by dependency selection.
// Empty Ref/TaskID identify a global hard-selection failure.
type BlockedSelection struct {
	Ref              string
	TaskID           string
	Entrypoint       string
	Code             string
	Reason           string
	GraphRevision    string
	ProviderRevision string
}

type Store struct {
	db *sql.DB
}

func New(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("migration connection: %w", err)
	}
	defer conn.Close()
	if err := configureSQLiteConnection(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS pulse_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_ref TEXT NOT NULL,
			task_id TEXT NOT NULL,
			role TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'claimed',
			claimed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS claimed_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_ref TEXT NOT NULL,
			task_id TEXT NOT NULL,
			role TEXT NOT NULL,
			lane_name TEXT NOT NULL,
			claimed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			description TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS lane_runtime (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			lane_name TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL DEFAULT 'idle',
			tab_id TEXT,
			started_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS blocked_selection_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ref TEXT NOT NULL,
			task_id TEXT NOT NULL,
			entrypoint TEXT NOT NULL,
			code TEXT NOT NULL,
			reason TEXT NOT NULL,
			graph_revision TEXT NOT NULL DEFAULT '',
			provider_revision TEXT NOT NULL DEFAULT '',
			recorded_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			recency_seq INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS blocked_selection_identity ON blocked_selection_history
			(ref, task_id, entrypoint, code, graph_revision, provider_revision)`,
		`CREATE TABLE IF NOT EXISTS blocked_selection_sequence (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			value INTEGER NOT NULL
		)`,
		`INSERT OR IGNORE INTO blocked_selection_sequence (id, value) VALUES (1, 0)`,
	}
	for _, m := range migrations {
		if _, err := conn.ExecContext(context.Background(), m); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	rows, err := conn.QueryContext(context.Background(), `PRAGMA table_info(blocked_selection_history)`)
	if err != nil {
		return fmt.Errorf("inspect blocked selection schema: %w", err)
	}
	hasRecency := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan blocked selection schema: %w", err)
		}
		if name == "recency_seq" {
			hasRecency = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close blocked selection schema: %w", err)
	}
	if !hasRecency {
		if _, err := conn.ExecContext(context.Background(), `ALTER TABLE blocked_selection_history ADD COLUMN recency_seq INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add blocked selection recency: %w", err)
		}
	}
	if _, err := conn.ExecContext(context.Background(), `CREATE UNIQUE INDEX IF NOT EXISTS blocked_selection_identity ON blocked_selection_history
		(ref, task_id, entrypoint, code, graph_revision, provider_revision)`); err != nil {
		return fmt.Errorf("create blocked selection identity index: %w", err)
	}
	if _, err := conn.ExecContext(context.Background(), `UPDATE blocked_selection_sequence
		SET value = MAX(value, COALESCE((SELECT MAX(recency_seq) FROM blocked_selection_history), 0))
		WHERE id = 1`); err != nil {
		return fmt.Errorf("bootstrap blocked selection recency: %w", err)
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	committed = true
	return nil
}

const sqliteBusyTimeout = 5000

func configureSQLiteConnection(conn *sql.Conn) error {
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf(`PRAGMA busy_timeout = %d`, sqliteBusyTimeout)); err != nil {
		return fmt.Errorf("configure sqlite busy timeout: %w", err)
	}
	return nil
}

// RecordBlockedSelection persists one selection attention item transactionally.
func (s *Store) RecordBlockedSelection(ref, taskID, entrypoint, code, reason, graphRevision, providerRevision string) (*BlockedRecord, error) {
	records, err := s.RecordBlockedSelections([]BlockedSelection{{
		Ref: ref, TaskID: taskID, Entrypoint: entrypoint, Code: code, Reason: reason,
		GraphRevision: graphRevision, ProviderRevision: providerRevision,
	}})
	if err != nil {
		return nil, err
	}
	return &records[0], nil
}

// RecordBlockedSelections atomically upserts all attention items. Same-identity
// reason changes update the persisted row, so callers never receive content
// that differs from durable evidence. A failed transaction preserves prior
// rows and writes none of the current batch.
func (s *Store) RecordBlockedSelections(items []BlockedSelection) ([]BlockedRecord, error) {
	if len(items) == 0 {
		return nil, nil
	}
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("blocked selection connection: %w", err)
	}
	defer conn.Close()
	if err := configureSQLiteConnection(conn); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		return nil, fmt.Errorf("begin blocked selection transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()
	for _, item := range items {
		if _, err := conn.ExecContext(context.Background(), `UPDATE blocked_selection_sequence SET value = value + 1 WHERE id = 1`); err != nil {
			return nil, fmt.Errorf("allocate blocked selection recency: %w", err)
		}
		var recencySeq int64
		if err := conn.QueryRowContext(context.Background(), `SELECT value FROM blocked_selection_sequence WHERE id = 1`).Scan(&recencySeq); err != nil {
			return nil, fmt.Errorf("read blocked selection recency: %w", err)
		}
		_, err := conn.ExecContext(context.Background(), `INSERT INTO blocked_selection_history
			(ref, task_id, entrypoint, code, reason, graph_revision, provider_revision, recency_seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(ref, task_id, entrypoint, code, graph_revision, provider_revision)
			DO UPDATE SET reason=excluded.reason, recorded_at=CURRENT_TIMESTAMP, recency_seq=excluded.recency_seq`,
			item.Ref, item.TaskID, item.Entrypoint, item.Code, item.Reason,
			item.GraphRevision, item.ProviderRevision, recencySeq)
		if err != nil {
			return nil, fmt.Errorf("record blocked selection: %w", err)
		}
	}
	if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
		return nil, fmt.Errorf("commit blocked selection transaction: %w", err)
	}
	committed = true

	records := make([]BlockedRecord, 0, len(items))
	for _, item := range items {
		var record BlockedRecord
		err := conn.QueryRowContext(context.Background(), `SELECT id, ref, task_id, entrypoint, code, reason,
			graph_revision, provider_revision, recorded_at, recency_seq
			FROM blocked_selection_history WHERE ref=? AND task_id=? AND entrypoint=? AND code=? AND graph_revision=? AND provider_revision=?`,
			item.Ref, item.TaskID, item.Entrypoint, item.Code, item.GraphRevision, item.ProviderRevision).
			Scan(&record.ID, &record.Ref, &record.TaskID, &record.Entrypoint, &record.Code,
				&record.Reason, &record.GraphRevision, &record.ProviderRevision, &record.RecordedAt, &record.RecencySeq)
		if err != nil {
			return nil, fmt.Errorf("read persisted blocked selection: %w", err)
		}
		records = append(records, record)
	}
	return records, nil
}

// BlockedSelectionHistory returns durable dependency holds newest first.
func (s *Store) BlockedSelectionHistory(limit int) ([]BlockedRecord, error) {
	return s.BlockedSelectionHistorySince(limit, time.Time{})
}

// BlockedSelectionHistorySince returns dependency holds newer than since,
// newest first. A zero since preserves the full-history behavior for callers
// that need audit data; operational status should pass a bounded window so
// resolved outages do not remain as live-looking ghosts indefinitely.
func (s *Store) BlockedSelectionHistorySince(limit int, since time.Time) ([]BlockedRecord, error) {
	query := `SELECT id, ref, task_id, entrypoint, code, reason,
		graph_revision, provider_revision, recorded_at, recency_seq FROM blocked_selection_history`
	args := []any{}
	if !since.IsZero() {
		query += ` WHERE recorded_at >= ?`
		args = append(args, since)
	}
	query += ` ORDER BY recency_seq DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []BlockedRecord
	for rows.Next() {
		var r BlockedRecord
		if err := rows.Scan(&r.ID, &r.Ref, &r.TaskID, &r.Entrypoint, &r.Code,
			&r.Reason, &r.GraphRevision, &r.ProviderRevision, &r.RecordedAt, &r.RecencySeq); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Store) RecordPulse(taskRef, taskID, role string) (*PulseRecord, error) {
	res, err := s.db.Exec(
		`INSERT INTO pulse_history (task_ref, task_id, role) VALUES (?, ?, ?)`,
		taskRef, taskID, role,
	)
	if err != nil {
		return nil, fmt.Errorf("record pulse: %w", err)
	}
	id, _ := res.LastInsertId()
	return &PulseRecord{
		ID:        id,
		TaskRef:   taskRef,
		TaskID:    taskID,
		Role:      role,
		Status:    "claimed",
		ClaimedAt: time.Now(),
	}, nil
}

func (s *Store) CompletePulse(id int64) error {
	now := time.Now()
	_, err := s.db.Exec(
		`UPDATE pulse_history SET status = 'done', completed_at = ? WHERE id = ?`,
		now, id,
	)
	return err
}

func (s *Store) PulseHistory(limit int) ([]PulseRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, task_ref, task_id, role, status, claimed_at, completed_at
		 FROM pulse_history ORDER BY claimed_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []PulseRecord
	for rows.Next() {
		var r PulseRecord
		if err := rows.Scan(&r.ID, &r.TaskRef, &r.TaskID, &r.Role, &r.Status, &r.ClaimedAt, &r.CompletedAt); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

func (s *Store) ClaimTask(taskRef, taskID, role, laneName, description string) (*ClaimedTask, error) {
	res, err := s.db.Exec(
		`INSERT INTO claimed_tasks (task_ref, task_id, role, lane_name, description) VALUES (?, ?, ?, ?, ?)`,
		taskRef, taskID, role, laneName, description,
	)
	if err != nil {
		return nil, fmt.Errorf("claim task: %w", err)
	}
	id, _ := res.LastInsertId()
	return &ClaimedTask{
		ID:          id,
		TaskRef:     taskRef,
		TaskID:      taskID,
		Role:        role,
		LaneName:    laneName,
		ClaimedAt:   time.Now(),
		Description: description,
	}, nil
}

func (s *Store) ActiveClaims() ([]ClaimedTask, error) {
	rows, err := s.db.Query(
		`SELECT id, task_ref, task_id, role, lane_name, claimed_at, description
		 FROM claimed_tasks ORDER BY claimed_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []ClaimedTask
	for rows.Next() {
		var t ClaimedTask
		if err := rows.Scan(&t.ID, &t.TaskRef, &t.TaskID, &t.Role, &t.LaneName, &t.ClaimedAt, &t.Description); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, nil
}

func (s *Store) UpsertLaneRuntime(laneName, status, tabID string) (*LaneRuntime, error) {
	_, err := s.db.Exec(
		`INSERT INTO lane_runtime (lane_name, status, tab_id)
		 VALUES (?, ?, ?)
		 ON CONFLICT(lane_name) DO UPDATE SET status = excluded.status, tab_id = excluded.tab_id`,
		laneName, status, tabID,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert lane runtime: %w", err)
	}
	return &LaneRuntime{
		LaneName:  laneName,
		Status:    status,
		TabID:     tabID,
		StartedAt: time.Now(),
	}, nil
}

func (s *Store) LaneRuntimes() ([]LaneRuntime, error) {
	rows, err := s.db.Query(
		`SELECT id, lane_name, status, tab_id, started_at FROM lane_runtime ORDER BY lane_name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runtimes []LaneRuntime
	for rows.Next() {
		var r LaneRuntime
		if err := rows.Scan(&r.ID, &r.LaneName, &r.Status, &r.TabID, &r.StartedAt); err != nil {
			return nil, err
		}
		runtimes = append(runtimes, r)
	}
	return runtimes, nil
}
