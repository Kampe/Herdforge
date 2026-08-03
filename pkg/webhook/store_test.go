package webhook

import (
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "webhook.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_Record_New(t *testing.T) {
	s := newTestStore(t)

	ev, existed, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if existed {
		t.Error("expected existed=false for a brand new delivery id")
	}
	if ev.Status != StatusPending {
		t.Errorf("expected StatusPending, got %q", ev.Status)
	}
	if ev.ID == 0 {
		t.Error("expected a non-zero row id")
	}

	got, err := s.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected Get to find the persisted event")
	}
	if got.Payload != `{"a":1}` {
		t.Errorf("expected payload to round-trip, got %q", got.Payload)
	}
}

func TestStore_Record_DuplicateSamePayload_Idempotent(t *testing.T) {
	s := newTestStore(t)

	first, _, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`)
	if err != nil {
		t.Fatalf("first Record: %v", err)
	}

	second, existed, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`)
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}
	if !existed {
		t.Error("expected existed=true for a repeated delivery id with identical payload")
	}
	if second.ID != first.ID {
		t.Errorf("expected the same row to be returned, got id=%d want id=%d", second.ID, first.ID)
	}

	// Only one row must exist — a mutant that always inserts would leave two.
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE delivery_id = ?`, "d1").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly one row for delivery_id=d1, got %d", count)
	}
}

func TestStore_Record_DuplicateDifferentPayload_Conflict(t *testing.T) {
	s := newTestStore(t)

	if _, _, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`); err != nil {
		t.Fatalf("first Record: %v", err)
	}

	_, _, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":2}`)
	if !errors.Is(err, ErrPayloadConflict) {
		t.Errorf("expected ErrPayloadConflict for a reused delivery id with a different payload, got %v", err)
	}
}

func TestStore_Record_EmptyDeliveryID_Rejected(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Record("", "kaneo", "task.created", "FAC-1", "proj", `{}`); err == nil {
		t.Error("expected Record to reject an empty delivery id (fail-closed)")
	}
}

func TestStore_MarkProcessed(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{}`); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.MarkProcessed("d1"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	got, err := s.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusProcessed {
		t.Errorf("expected StatusProcessed, got %q", got.Status)
	}

	// Marking an already-processed row again must fail, not silently succeed.
	if err := s.MarkProcessed("d1"); err == nil {
		t.Error("expected MarkProcessed to fail closed on an already-processed delivery")
	}
}

func TestStore_Record_DuplicateAfterProcessed_ReturnsProcessed(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.MarkProcessed("d1"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	ev, existed, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`)
	if err != nil {
		t.Fatalf("duplicate Record: %v", err)
	}
	if !existed {
		t.Error("expected existed=true for a duplicate of a processed delivery")
	}
	if ev.Status != StatusProcessed {
		t.Errorf("expected the returned event to report StatusProcessed, got %q", ev.Status)
	}
}

func TestStore_MarkFailed_LeavesRowPendingForRetry(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Record("d1", "kaneo", "task.created", "FAC-1", "proj", `{}`); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.MarkFailed("d1", "handler exploded"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, err := s.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("expected a failed delivery to remain StatusPending for retry, got %q", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("expected attempts=1, got %d", got.Attempts)
	}
	if got.LastError != "handler exploded" {
		t.Errorf("expected last_error to be recorded, got %q", got.LastError)
	}
}

func TestStore_MarkFailed_UnknownDelivery_Errors(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkFailed("missing", "boom"); err == nil {
		t.Error("expected MarkFailed to error for an unknown delivery id")
	}
}

func TestStore_Get_Unknown_ReturnsNil(t *testing.T) {
	s := newTestStore(t)
	got, err := s.Get("nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for an unknown delivery id, got %+v", got)
	}
}
