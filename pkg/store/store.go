package store

import (
	"database/sql"
	"fmt"
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
}

type Store struct {
	db *sql.DB
}

func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
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
			recorded_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS blocked_selection_identity ON blocked_selection_history
			(ref, task_id, entrypoint, code, graph_revision, provider_revision)`,
	}
	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// RecordBlockedSelection persists every per-card blocked result from the
// production selection path. A write failure is returned to preserve evidence.
func (s *Store) RecordBlockedSelection(ref, taskID, entrypoint, code, reason, graphRevision, providerRevision string) (*BlockedRecord, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO blocked_selection_history
		(ref, task_id, entrypoint, code, reason, graph_revision, provider_revision)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, ref, taskID, entrypoint, code, reason, graphRevision, providerRevision)
	if err != nil {
		return nil, fmt.Errorf("record blocked selection: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("record blocked selection id: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("record blocked selection rows: %w", err)
	}
	if inserted == 0 {
		err = s.db.QueryRow(`SELECT id FROM blocked_selection_history WHERE ref=? AND task_id=? AND entrypoint=? AND code=? AND graph_revision=? AND provider_revision=?`, ref, taskID, entrypoint, code, graphRevision, providerRevision).Scan(&id)
		if err != nil {
			return nil, fmt.Errorf("read blocked selection identity: %w", err)
		}
	}
	return &BlockedRecord{ID: id, Ref: ref, TaskID: taskID, Entrypoint: entrypoint,
		Code: code, Reason: reason, GraphRevision: graphRevision,
		ProviderRevision: providerRevision, RecordedAt: time.Now()}, nil
}

// BlockedSelectionHistory returns durable dependency holds newest first.
func (s *Store) BlockedSelectionHistory(limit int) ([]BlockedRecord, error) {
	rows, err := s.db.Query(`SELECT id, ref, task_id, entrypoint, code, reason,
		graph_revision, provider_revision, recorded_at FROM blocked_selection_history
		ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []BlockedRecord
	for rows.Next() {
		var r BlockedRecord
		if err := rows.Scan(&r.ID, &r.Ref, &r.TaskID, &r.Entrypoint, &r.Code,
			&r.Reason, &r.GraphRevision, &r.ProviderRevision, &r.RecordedAt); err != nil {
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
