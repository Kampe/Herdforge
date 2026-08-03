package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close(); os.RemoveAll(dir) })
	return s
}

func TestNewStore(t *testing.T) {
	s := tempStore(t)
	if s == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestRecordPulse(t *testing.T) {
	s := tempStore(t)
	rec, err := s.RecordPulse("FAC-1", "t-1", "worker")
	if err != nil {
		t.Fatalf("record pulse: %v", err)
	}
	if rec.TaskRef != "FAC-1" {
		t.Errorf("expected FAC-1, got %s", rec.TaskRef)
	}
	if rec.Role != "worker" {
		t.Errorf("expected worker, got %s", rec.Role)
	}
}

func TestBlockedSelectionHistory(t *testing.T) {
	s := tempStore(t)
	if _, err := s.RecordBlockedSelection("FAC-7", "task-7", "pulse", "drift", "cycle detected", "graph-1", "provider-1"); err != nil {
		t.Fatalf("record blocked selection: %v", err)
	}
	history, err := s.BlockedSelectionHistory(10)
	if err != nil {
		t.Fatalf("blocked selection history: %v", err)
	}
	if len(history) != 1 || history[0].Ref != "FAC-7" || history[0].TaskID != "task-7" || history[0].Code != "drift" || history[0].Reason != "cycle detected" {
		t.Fatalf("unexpected blocked history: %+v", history)
	}
}

func TestBlockedSelectionIdentityIsIdempotentAndRevisionSensitive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restart.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.RecordBlockedSelection("FAC-7", "task-7", "pulse", "drift", "old reason", "graph-1", "provider-1")
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := s.RecordBlockedSelection("FAC-7", "task-7", "pulse", "drift", "changed reason", "graph-1", "provider-1")
	if err != nil || repeat.ID != first.ID || repeat.Reason != "changed reason" {
		t.Fatalf("same blocked identity must be idempotent: first=%+v repeat=%+v err=%v", first, repeat, err)
	}
	changed, err := s.RecordBlockedSelection("FAC-7", "task-7", "pulse", "drift", "new reason", "graph-2", "provider-1")
	if err != nil || changed.ID == first.ID {
		t.Fatalf("changed revision must create new evidence: first=%+v changed=%+v err=%v", first, changed, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	history, err := s.BlockedSelectionHistory(10)
	if err != nil || len(history) != 2 {
		t.Fatalf("restart must retain idempotent history: history=%+v err=%v", history, err)
	}
	if history[0].Reason != "new reason" || history[1].Reason != "changed reason" {
		t.Fatalf("changed evidence readback order/content wrong: %+v", history)
	}
}

func TestBlockedSelectionHistoryRefreshIsMostRecent(t *testing.T) {
	s := tempStore(t)
	if _, err := s.RecordBlockedSelection("FAC-old", "old", "pulse", "drift", "initial", "graph-old", "provider"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		ref := fmt.Sprintf("FAC-new-%02d", i)
		if _, err := s.RecordBlockedSelection(ref, ref, "pulse", "drift", "new", ref, "provider"); err != nil {
			t.Fatal(err)
		}
	}
	refreshed, err := s.RecordBlockedSelection("FAC-old", "old", "pulse", "drift", "refreshed", "graph-old", "provider")
	if err != nil || refreshed.Reason != "refreshed" {
		t.Fatalf("refresh must return persisted current content: %+v err=%v", refreshed, err)
	}
	history, err := s.BlockedSelectionHistory(10)
	if err != nil || len(history) != 10 {
		t.Fatalf("status history limit failed: len=%d err=%v", len(history), err)
	}
	if history[0].Ref != "FAC-old" || history[0].Reason != "refreshed" {
		t.Fatalf("refreshed identity must be first/current: %+v", history)
	}
}

func TestConcurrentStoreOpenAndRefreshSerializesRecency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	const writers = 2
	stores := make([]*Store, writers)
	errs := make(chan error, writers)
	start := make(chan struct{})
	var opens sync.WaitGroup
	for i := range stores {
		i := i
		opens.Add(1)
		go func() {
			defer opens.Done()
			<-start
			var err error
			stores[i], err = New(path)
			if err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	opens.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent migration failed: %v", err)
	}
	for _, s := range stores {
		if s == nil {
			t.Fatal("concurrent open returned nil store")
		}
		defer s.Close()
	}
	if _, err := stores[0].RecordBlockedSelection("FAC-shared", "shared", "pulse", "drift", "initial", "graph", "provider"); err != nil {
		t.Fatal(err)
	}

	const writesPerStore = 12
	var writes sync.WaitGroup
	writeErrs := make(chan error, writers*writesPerStore)
	for i, s := range stores {
		for j := 0; j < writesPerStore; j++ {
			i, j := i, j
			writes.Add(1)
			go func() {
				defer writes.Done()
				ref := fmt.Sprintf("FAC-%d-%02d", i, j)
				if _, err := s.RecordBlockedSelection(ref, ref, "pulse", "drift", "new", ref, "provider"); err != nil {
					writeErrs <- err
				}
			}()
		}
	}
	writes.Wait()
	close(writeErrs)
	for err := range writeErrs {
		t.Fatalf("concurrent write lost to contention: %v", err)
	}

	var refresh sync.WaitGroup
	refreshErrs := make(chan error, 2)
	for i, s := range stores {
		i, s := i, s
		refresh.Add(1)
		go func() {
			defer refresh.Done()
			if _, err := s.RecordBlockedSelection("FAC-shared", "shared", "pulse", "drift", fmt.Sprintf("refresh-%d", i), "graph", "provider"); err != nil {
				refreshErrs <- err
			}
		}()
	}
	refresh.Wait()
	close(refreshErrs)
	for err := range refreshErrs {
		t.Fatalf("concurrent refresh failed: %v", err)
	}

	history, err := stores[0].BlockedSelectionHistory(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1+writesPerStore*writers {
		t.Fatalf("lost evidence during concurrent writes: got %d records", len(history))
	}
	for i := 1; i < len(history); i++ {
		if history[i-1].RecencySeq <= history[i].RecencySeq {
			t.Fatalf("recency sequence is not strictly descending: %d then %d", history[i-1].RecencySeq, history[i].RecencySeq)
		}
	}
	if history[0].Ref != "FAC-shared" || history[0].Reason != "refresh-0" && history[0].Reason != "refresh-1" {
		t.Fatalf("latest concurrent refresh not persisted first: %+v", history[0])
	}
}

func TestBlockedSelectionExhaustedWriteContentionFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "locked.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	if _, err := s.RecordBlockedSelection("FAC-locked", "locked", "pulse", "drift", "blocked", "graph", "provider"); err == nil {
		t.Fatal("exhausted SQLite write contention must return non-nil error")
	}
}

func TestBlockedSelectionUpgradeBootstrapsRecencyFromExistingHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE blocked_selection_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ref TEXT NOT NULL, task_id TEXT NOT NULL, entrypoint TEXT NOT NULL,
		code TEXT NOT NULL, reason TEXT NOT NULL,
		graph_revision TEXT NOT NULL DEFAULT '', provider_revision TEXT NOT NULL DEFAULT '',
		recorded_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		recency_seq INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`INSERT INTO blocked_selection_history
		(ref, task_id, entrypoint, code, reason, graph_revision, provider_revision, recency_seq)
		VALUES ('FAC-old', 'old', 'pulse', 'drift', 'old', 'old-graph', 'provider', 41)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	refreshed, err := s.RecordBlockedSelection("FAC-old", "old", "pulse", "drift", "refreshed", "old-graph", "provider")
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RecencySeq <= 41 {
		t.Fatalf("upgrade recency regressed: got %d", refreshed.RecencySeq)
	}
	history, err := s.BlockedSelectionHistory(1)
	if err != nil || len(history) != 1 || history[0].Ref != "FAC-old" || history[0].Reason != "refreshed" {
		t.Fatalf("refreshed upgraded evidence must be current: history=%+v err=%v", history, err)
	}
}

func TestCompletePulse(t *testing.T) {
	s := tempStore(t)
	rec, _ := s.RecordPulse("FAC-1", "t-1", "worker")
	if err := s.CompletePulse(rec.ID); err != nil {
		t.Fatalf("complete pulse: %v", err)
	}
	history, err := s.PulseHistory(10)
	if err != nil {
		t.Fatalf("pulse history: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 record, got %d", len(history))
	}
	if history[0].Status != "done" {
		t.Errorf("expected status done, got %s", history[0].Status)
	}
	if history[0].CompletedAt == nil {
		t.Error("expected completed_at to be set")
	}
}

func TestClaimTask(t *testing.T) {
	s := tempStore(t)
	ct, err := s.ClaimTask("FAC-2", "t-2", "worker", "test-lane", "Test description")
	if err != nil {
		t.Fatalf("claim task: %v", err)
	}
	if ct.TaskRef != "FAC-2" {
		t.Errorf("expected FAC-2, got %s", ct.TaskRef)
	}
	if ct.LaneName != "test-lane" {
		t.Errorf("expected test-lane, got %s", ct.LaneName)
	}
}

func TestActiveClaims(t *testing.T) {
	s := tempStore(t)
	s.ClaimTask("FAC-1", "t-1", "worker", "forge-worker", "")
	s.ClaimTask("FAC-2", "t-2", "reviewer", "forge-reviewer", "")

	claims, err := s.ActiveClaims()
	if err != nil {
		t.Fatalf("active claims: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("expected 2 claims, got %d", len(claims))
	}
}

func TestUpsertLaneRuntime(t *testing.T) {
	s := tempStore(t)
	lr, err := s.UpsertLaneRuntime("worker", "running", "tab-1")
	if err != nil {
		t.Fatalf("upsert lane: %v", err)
	}
	if lr.LaneName != "worker" {
		t.Errorf("expected worker, got %s", lr.LaneName)
	}
	if lr.TabID != "tab-1" {
		t.Errorf("expected tab-1, got %s", lr.TabID)
	}
}

func TestUpsertLaneRuntimeUpdate(t *testing.T) {
	s := tempStore(t)
	s.UpsertLaneRuntime("worker", "running", "tab-1")
	s.UpsertLaneRuntime("worker", "idle", "tab-2")

	runtimes, err := s.LaneRuntimes()
	if err != nil {
		t.Fatalf("lane runtimes: %v", err)
	}
	if len(runtimes) != 1 {
		t.Fatalf("expected 1 runtime, got %d", len(runtimes))
	}
	if runtimes[0].Status != "idle" {
		t.Errorf("expected idle, got %s", runtimes[0].Status)
	}
}

func TestLaneRuntimes(t *testing.T) {
	s := tempStore(t)
	s.UpsertLaneRuntime("worker", "running", "tab-1")
	s.UpsertLaneRuntime("reviewer", "running", "tab-2")

	runtimes, err := s.LaneRuntimes()
	if err != nil {
		t.Fatalf("lane runtimes: %v", err)
	}
	if len(runtimes) != 2 {
		t.Fatalf("expected 2 runtimes, got %d", len(runtimes))
	}
}

func TestPulseHistoryEmpty(t *testing.T) {
	s := tempStore(t)
	history, err := s.PulseHistory(10)
	if err != nil {
		t.Fatalf("pulse history: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("expected empty history, got %d", len(history))
	}
}

func TestPulseHistoryLimit(t *testing.T) {
	s := tempStore(t)
	for i := 0; i < 5; i++ {
		ref := fmt.Sprintf("FAC-%d", i+1)
		s.RecordPulse(ref, "t-"+ref, "worker")
	}
	history, err := s.PulseHistory(3)
	if err != nil {
		t.Fatalf("pulse history: %v", err)
	}
	if len(history) != 3 {
		t.Errorf("expected 3 records (limit), got %d", len(history))
	}
}
