package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type PulseRecord struct {
	ID         int64     `json:"id"`
	TaskRef    string    `json:"task_ref"`
	TaskID     string    `json:"task_id"`
	Role       string    `json:"role"`
	Status     string    `json:"status"`
	ClaimedAt  time.Time `json:"claimed_at"`
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
	}
	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
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
		ID:        id,
		TaskRef:   taskRef,
		TaskID:    taskID,
		Role:      role,
		LaneName:  laneName,
		ClaimedAt: time.Now(),
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
