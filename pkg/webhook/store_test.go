package webhook

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
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

func TestStore_Claim_New(t *testing.T) {
	s := newTestStore(t)

	ev, claimed, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Error("expected claimed=true for a brand new delivery id")
	}
	if ev.Status != StatusInFlight {
		t.Errorf("expected StatusInFlight, got %q", ev.Status)
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

func TestStore_Claim_WhileInFlight_NotClaimed(t *testing.T) {
	s := newTestStore(t)

	if _, claimed, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`); err != nil || !claimed {
		t.Fatalf("first Claim: claimed=%v err=%v", claimed, err)
	}

	// A second Claim for the same delivery id while the first is still
	// in flight (not yet MarkProcessed/MarkFailed) must NOT also be
	// granted — this is the exact race a mutant that drops the CAS
	// would reintroduce.
	ev, claimed, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if claimed {
		t.Error("expected the second concurrent Claim to be refused while the delivery is in flight")
	}
	if ev.Status != StatusInFlight {
		t.Errorf("expected the reported status to be InFlight, got %q", ev.Status)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE delivery_id = ?`, "d1").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected exactly one row for delivery_id=d1, got %d", count)
	}
}

func TestStore_Claim_Concurrent_OnlyOneWinner(t *testing.T) {
	s := newTestStore(t)

	const n = 20
	var wg sync.WaitGroup
	var claims int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`)
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			if claimed {
				atomic.AddInt32(&claims, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&claims); got != 1 {
		t.Errorf("expected exactly one of %d concurrent Claim calls to win, got %d", n, got)
	}
}

func TestStore_Claim_DuplicateDifferentPayload_Conflict(t *testing.T) {
	s := newTestStore(t)

	if _, _, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	_, _, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":2}`)
	if !errors.Is(err, ErrPayloadConflict) {
		t.Errorf("expected ErrPayloadConflict for a reused delivery id with a different payload, got %v", err)
	}
}

func TestStore_Claim_EmptyDeliveryID_Rejected(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Claim("", "kaneo", "task.created", "FAC-1", "proj", `{}`); err == nil {
		t.Error("expected Claim to reject an empty delivery id (fail-closed)")
	}
}

func TestStore_MarkProcessed(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{}`); err != nil {
		t.Fatalf("Claim: %v", err)
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

func TestStore_MarkProcessed_NotInFlight_Errors(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkProcessed("missing"); err == nil {
		t.Error("expected MarkProcessed to error for a delivery that was never claimed")
	}
}

func TestStore_Claim_DuplicateAfterProcessed_ReturnsProcessedNotClaimed(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.MarkProcessed("d1"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}

	ev, claimed, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{"a":1}`)
	if err != nil {
		t.Fatalf("duplicate Claim: %v", err)
	}
	if claimed {
		t.Error("expected a duplicate of a processed delivery to NOT be claimed (already done)")
	}
	if ev.Status != StatusProcessed {
		t.Errorf("expected the returned event to report StatusProcessed, got %q", ev.Status)
	}
}

func TestStore_MarkFailed_LeavesRowPendingForRetry(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{}`); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.MarkFailed("d1", "handler exploded"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, err := s.Get("d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusPending {
		t.Errorf("expected a failed delivery to return to StatusPending for retry, got %q", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("expected attempts=1, got %d", got.Attempts)
	}
	if got.LastError != "handler exploded" {
		t.Errorf("expected last_error to be recorded, got %q", got.LastError)
	}
}

func TestStore_Claim_AfterFailed_Reclaimable(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{}`); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := s.MarkFailed("d1", "boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	ev, claimed, err := s.Claim("d1", "kaneo", "task.created", "FAC-1", "proj", `{}`)
	if err != nil {
		t.Fatalf("retry Claim: %v", err)
	}
	if !claimed {
		t.Error("expected a delivery left pending by a handler failure to be re-claimable")
	}
	if ev.Status != StatusInFlight {
		t.Errorf("expected StatusInFlight after re-claiming, got %q", ev.Status)
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
