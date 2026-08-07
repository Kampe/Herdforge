package provider

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	_ "modernc.org/sqlite"
)

// OpReceipt is durable evidence that a logical mutation was accepted.
type OpReceipt struct {
	OpID            string
	TaskID          string
	FenceToken      int64
	Revision        string
	// BaseRevision is the live provider revision captured BEFORE remote mutate.
	// Empty-rev Present recovery requires live != BaseRevision to attribute
	// the effect to THIS op; otherwise refuse (pt5t7 #3).
	BaseRevision    string
	ExpectedStatus  string
	ExpectedComment string
	Ambiguous       bool
}

// FenceStore: fence high-water + applied op receipts + per-task exclusive lock.
type FenceStore interface {
	Highest(ctx context.Context, taskID string) (int64, error)
	Advance(ctx context.Context, taskID string, fenceToken int64) (int64, error)
	WithExclusive(ctx context.Context, taskID string, fn func(ctx context.Context) error) error
	// LookupApplied returns the receipt for opID if present.
	LookupApplied(ctx context.Context, opID string) (*OpReceipt, error)
	// ListOpsForTask returns all receipts for taskID (applied + ambiguous).
	// Used for empty-rev recovery competing-same-status attribution.
	ListOpsForTask(ctx context.Context, taskID string) ([]OpReceipt, error)
	// MarkApplied persists a successful application. Errors must propagate.
	MarkApplied(ctx context.Context, rec OpReceipt) error
	// MarkAmbiguous records provider-success/local-failure ambiguity.
	MarkAmbiguous(ctx context.Context, rec OpReceipt) error
	BusyTimeout() time.Duration
	Close() error
}

const DefaultFenceBusyTimeout = 3 * time.Second

// MemoryFenceStore for tests.
type MemoryFenceStore struct {
	mu      sync.Mutex
	high    map[string]int64
	applied map[string]OpReceipt
	taskMu  sync.Mutex
	tasks   map[string]*sync.Mutex
}

func NewMemoryFenceStore() *MemoryFenceStore {
	return &MemoryFenceStore{
		high:    make(map[string]int64),
		applied: make(map[string]OpReceipt),
		tasks:   make(map[string]*sync.Mutex),
	}
}

func (m *MemoryFenceStore) Highest(_ context.Context, taskID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.high[taskID], nil
}

func (m *MemoryFenceStore) Advance(_ context.Context, taskID string, fenceToken int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fenceToken > m.high[taskID] {
		m.high[taskID] = fenceToken
	}
	return m.high[taskID], nil
}

func (m *MemoryFenceStore) WithExclusive(ctx context.Context, taskID string, fn func(ctx context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("fence: WithExclusive requires fn")
	}
	m.taskMu.Lock()
	if m.tasks == nil {
		m.tasks = make(map[string]*sync.Mutex)
	}
	l, ok := m.tasks[taskID]
	if !ok {
		l = &sync.Mutex{}
		m.tasks[taskID] = l
	}
	m.taskMu.Unlock()
	l.Lock()
	defer l.Unlock()
	return fn(ctx)
}

func (m *MemoryFenceStore) LookupApplied(_ context.Context, opID string) (*OpReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.applied[opID]
	if !ok {
		return nil, nil
	}
	cp := r
	return &cp, nil
}

func (m *MemoryFenceStore) ListOpsForTask(_ context.Context, taskID string) ([]OpReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []OpReceipt
	for _, rec := range m.applied {
		if rec.TaskID == taskID {
			out = append(out, rec)
		}
	}
	return out, nil
}

func (m *MemoryFenceStore) MarkApplied(_ context.Context, rec OpReceipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec.OpID == "" {
		return fmt.Errorf("fence: MarkApplied requires opID")
	}
	if prev, ok := m.applied[rec.OpID]; ok {
		if prev.TaskID != rec.TaskID || prev.FenceToken != rec.FenceToken {
			return fmt.Errorf("fence: op %s bound to task=%s fence=%d, cannot rebind to task=%s fence=%d",
				rec.OpID, prev.TaskID, prev.FenceToken, rec.TaskID, rec.FenceToken)
		}
		// Monotonic merge: applied is terminal; never lose evidence.
		rec = mergeReceiptMonotonic(prev, rec, false)
	} else {
		rec.Ambiguous = false
	}
	m.applied[rec.OpID] = rec
	return nil
}

func (m *MemoryFenceStore) MarkAmbiguous(_ context.Context, rec OpReceipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec.OpID == "" {
		return fmt.Errorf("fence: MarkAmbiguous requires opID")
	}
	if prev, ok := m.applied[rec.OpID]; ok {
		if prev.TaskID != rec.TaskID || prev.FenceToken != rec.FenceToken {
			return fmt.Errorf("fence: op %s bound to task=%s fence=%d, cannot rebind to task=%s fence=%d",
				rec.OpID, prev.TaskID, prev.FenceToken, rec.TaskID, rec.FenceToken)
		}
		// Applied is monotonic: never downgrade applied → ambiguous.
		if !prev.Ambiguous {
			return nil
		}
		rec = mergeReceiptMonotonic(prev, rec, true)
	} else {
		rec.Ambiguous = true
	}
	m.applied[rec.OpID] = rec
	return nil
}

// mergeReceiptMonotonic keeps terminal applied state and nonempty evidence.
func mergeReceiptMonotonic(prev, next OpReceipt, wantAmbiguous bool) OpReceipt {
	out := next
	out.TaskID = prev.TaskID
	out.FenceToken = prev.FenceToken
	out.OpID = prev.OpID
	if !prev.Ambiguous {
		// Terminal applied: stay applied.
		out.Ambiguous = false
	} else {
		out.Ambiguous = wantAmbiguous
	}
	if out.Revision == "" {
		out.Revision = prev.Revision
	}
	if out.BaseRevision == "" {
		out.BaseRevision = prev.BaseRevision
	}
	if out.ExpectedStatus == "" {
		out.ExpectedStatus = prev.ExpectedStatus
	}
	if out.ExpectedComment == "" {
		out.ExpectedComment = prev.ExpectedComment
	}
	return out
}

// ListExpectedComments returns durable applied comment bodies for taskID
// (ambiguous/in_progress rows are excluded so pre-backend journals cannot
// false-prove effectMet).
func (m *MemoryFenceStore) ListExpectedComments(_ context.Context, taskID string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	seen := map[string]struct{}{}
	for _, r := range m.applied {
		if r.TaskID != taskID || r.ExpectedComment == "" || r.Ambiguous {
			continue
		}
		if _, ok := seen[r.ExpectedComment]; ok {
			continue
		}
		seen[r.ExpectedComment] = struct{}{}
		out = append(out, r.ExpectedComment)
	}
	return out, nil
}

func (m *MemoryFenceStore) BusyTimeout() time.Duration { return 0 }
func (m *MemoryFenceStore) Close() error               { return nil }

// SQLiteFenceStore: durable fences + receipts; per-task flock for exclusion.
type SQLiteFenceStore struct {
	db          *sql.DB
	lockDir     string
	busyTimeout time.Duration
	localMu     sync.Mutex
	local       map[string]*sync.Mutex
}

func NewSQLiteFenceStore(path string) (*SQLiteFenceStore, error) {
	return NewSQLiteFenceStoreWithBusy(path, DefaultFenceBusyTimeout)
}

func NewSQLiteFenceStoreWithBusy(path string, busy time.Duration) (*SQLiteFenceStore, error) {
	if busy <= 0 {
		busy = DefaultFenceBusyTimeout
	}
	ms := int(busy / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)", path, ms)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open fence store: %w", err)
	}
	db.SetMaxOpenConns(8)
	lockDir := path + ".locks"
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &SQLiteFenceStore{db: db, lockDir: lockDir, busyTimeout: busy, local: make(map[string]*sync.Mutex)}
	if err := s.migrate(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteFenceStore) Close() error               { return s.db.Close() }
func (s *SQLiteFenceStore) BusyTimeout() time.Duration { return s.busyTimeout }

func (s *SQLiteFenceStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS fences (
			task_id TEXT PRIMARY KEY NOT NULL,
			fence_high INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS applied_ops (
			op_id TEXT PRIMARY KEY NOT NULL,
			task_id TEXT NOT NULL,
			fence_token INTEGER NOT NULL,
			revision TEXT NOT NULL DEFAULT '',
			expected_status TEXT NOT NULL DEFAULT '',
			expected_comment TEXT NOT NULL DEFAULT '',
			ambiguous INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate fence store: %w", err)
		}
	}
	// Best-effort column add for BaseRevision (existing DBs).
	_, _ = s.db.Exec(`ALTER TABLE applied_ops ADD COLUMN base_revision TEXT NOT NULL DEFAULT ''`)
	// Preserve/create store_authority for SHARED v5 seal (provision may have seeded it).
	_, _ = s.db.Exec(`CREATE TABLE IF NOT EXISTS store_authority (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		volume_seal TEXT NOT NULL UNIQUE,
		updated_at TEXT NOT NULL
	)`)
	return nil
}

func (s *SQLiteFenceStore) Highest(ctx context.Context, taskID string) (int64, error) {
	var high int64
	err := s.db.QueryRowContext(ctx, `SELECT fence_high FROM fences WHERE task_id = ?`, taskID).Scan(&high)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return high, nil
}

func (s *SQLiteFenceStore) Advance(ctx context.Context, taskID string, fenceToken int64) (int64, error) {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO fences (task_id, fence_high, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			fence_high = CASE WHEN excluded.fence_high > fences.fence_high THEN excluded.fence_high ELSE fences.fence_high END,
			updated_at = CASE WHEN excluded.fence_high > fences.fence_high THEN excluded.updated_at ELSE fences.updated_at END
	`, taskID, fenceToken, now)
	if err != nil {
		return 0, err
	}
	return s.Highest(ctx, taskID)
}

func (s *SQLiteFenceStore) LookupApplied(ctx context.Context, opID string) (*OpReceipt, error) {
	var r OpReceipt
	var amb int
	err := s.db.QueryRowContext(ctx, `
		SELECT op_id, task_id, fence_token, revision, COALESCE(base_revision,''), expected_status, expected_comment, ambiguous
		FROM applied_ops WHERE op_id = ?`, opID).Scan(
		&r.OpID, &r.TaskID, &r.FenceToken, &r.Revision, &r.BaseRevision, &r.ExpectedStatus, &r.ExpectedComment, &amb)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Ambiguous = amb != 0
	return &r, nil
}

func (s *SQLiteFenceStore) ListOpsForTask(ctx context.Context, taskID string) ([]OpReceipt, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT op_id, task_id, fence_token, revision, COALESCE(base_revision,''), expected_status, expected_comment, ambiguous
		FROM applied_ops WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpReceipt
	for rows.Next() {
		var r OpReceipt
		var amb int
		if err := rows.Scan(&r.OpID, &r.TaskID, &r.FenceToken, &r.Revision, &r.BaseRevision, &r.ExpectedStatus, &r.ExpectedComment, &amb); err != nil {
			return nil, err
		}
		r.Ambiguous = amb != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// markOpBound atomically binds opID→(task,fence) under BEGIN IMMEDIATE so
// two task locks cannot both accept the same opID (cross-task race).
// RowsAffected is checked: zero-row conflict = binding rejected.
func (s *SQLiteFenceStore) markOpBound(ctx context.Context, rec OpReceipt, ambiguous bool) error {
	if rec.OpID == "" {
		return fmt.Errorf("fence: markOpBound requires opID")
	}
	amb := 0
	if ambiguous {
		amb = 1
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("fence: BEGIN IMMEDIATE: %w", err)
	}
	commit := false
	defer func() {
		if !commit {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var prevTask, prevRev, prevBase, prevStat, prevCmt string
	var prevFence int64
	var prevAmb int
	err = conn.QueryRowContext(ctx, `
		SELECT task_id, fence_token, ambiguous, revision, COALESCE(base_revision,''), expected_status, expected_comment
		FROM applied_ops WHERE op_id = ?`, rec.OpID).
		Scan(&prevTask, &prevFence, &prevAmb, &prevRev, &prevBase, &prevStat, &prevCmt)
	switch {
	case err == sql.ErrNoRows:
		// insert below
	case err != nil:
		return err
	default:
		if prevTask != rec.TaskID || prevFence != rec.FenceToken {
			return fmt.Errorf("fence: op %s bound to task=%s fence=%d, cannot rebind to task=%s fence=%d",
				rec.OpID, prevTask, prevFence, rec.TaskID, rec.FenceToken)
		}
		// Applied is monotonic: refuse applied → ambiguous downgrade.
		if prevAmb == 0 && ambiguous {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return err
			}
			commit = true
			return nil
		}
		// Preserve nonempty evidence.
		if rec.Revision == "" {
			rec.Revision = prevRev
		}
		if rec.BaseRevision == "" {
			rec.BaseRevision = prevBase
		}
		if rec.ExpectedStatus == "" {
			rec.ExpectedStatus = prevStat
		}
		if rec.ExpectedComment == "" {
			rec.ExpectedComment = prevCmt
		}
		// Cannot un-apply: once applied stay applied.
		if prevAmb == 0 {
			amb = 0
		}
	}

	now := time.Now().UTC()
	res, err := conn.ExecContext(ctx, `
		INSERT INTO applied_ops (op_id, task_id, fence_token, revision, base_revision, expected_status, expected_comment, ambiguous, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(op_id) DO UPDATE SET
			revision = CASE WHEN excluded.revision != '' THEN excluded.revision ELSE applied_ops.revision END,
			base_revision = CASE WHEN excluded.base_revision != '' THEN excluded.base_revision ELSE applied_ops.base_revision END,
			expected_status = CASE WHEN excluded.expected_status != '' THEN excluded.expected_status ELSE applied_ops.expected_status END,
			expected_comment = CASE WHEN excluded.expected_comment != '' THEN excluded.expected_comment ELSE applied_ops.expected_comment END,
			ambiguous = CASE WHEN applied_ops.ambiguous = 0 THEN 0 ELSE excluded.ambiguous END,
			updated_at = excluded.updated_at
		WHERE applied_ops.task_id = excluded.task_id AND applied_ops.fence_token = excluded.fence_token
	`, rec.OpID, rec.TaskID, rec.FenceToken, rec.Revision, rec.BaseRevision, rec.ExpectedStatus, rec.ExpectedComment, amb, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("fence: op %s binding rejected (rows=0); task=%s fence=%d",
			rec.OpID, rec.TaskID, rec.FenceToken)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	commit = true
	return nil
}

func (s *SQLiteFenceStore) MarkApplied(ctx context.Context, rec OpReceipt) error {
	return s.markOpBound(ctx, rec, false)
}

func (s *SQLiteFenceStore) MarkAmbiguous(ctx context.Context, rec OpReceipt) error {
	return s.markOpBound(ctx, rec, true)
}

// ListExpectedComments returns durable applied comment bodies for taskID.
func (s *SQLiteFenceStore) ListExpectedComments(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT expected_comment FROM applied_ops
		WHERE task_id = ? AND expected_comment != '' AND ambiguous = 0
		ORDER BY updated_at ASC`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	seen := map[string]struct{}{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		if _, ok := seen[c]; ok {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *SQLiteFenceStore) lockPath(taskID string) string {
	sum := sha256.Sum256([]byte(taskID))
	return filepath.Join(s.lockDir, hex.EncodeToString(sum[:16])+".lock")
}

func (s *SQLiteFenceStore) WithExclusive(ctx context.Context, taskID string, fn func(ctx context.Context) error) error {
	if fn == nil {
		return fmt.Errorf("fence: WithExclusive requires fn")
	}
	s.localMu.Lock()
	if s.local == nil {
		s.local = make(map[string]*sync.Mutex)
	}
	lm, ok := s.local[taskID]
	if !ok {
		lm = &sync.Mutex{}
		s.local[taskID] = lm
	}
	s.localMu.Unlock()
	lm.Lock()
	defer lm.Unlock()

	path := s.lockPath(taskID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("fence lock open: %w", err)
	}
	defer f.Close()

	deadline := time.Now().Add(s.busyTimeout)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("fence exclusive lock timeout after %s for task %s: %w", s.busyTimeout, taskID, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn(ctx)
}

var (
	_ FenceStore = (*MemoryFenceStore)(nil)
	_ FenceStore = (*SQLiteFenceStore)(nil)
)
