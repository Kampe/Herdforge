package store

import (
	"fmt"
	"os"
	"path/filepath"
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
