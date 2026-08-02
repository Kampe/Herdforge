package outbox

import (
	"path/filepath"
	"testing"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(filepath.Join(dir, "outbox.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStore_EnqueuePersistsItem(t *testing.T) {
	s := tempStore(t)
	item, err := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr", Payload: `{"cmd":"dispatch"}`})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if item.ID == 0 {
		t.Error("expected a non-zero id")
	}
	if item.Status != StatusPending {
		t.Errorf("expected pending status, got %s", item.Status)
	}
}

func TestStore_EnqueueRequiresIdempotencyKeyAndKind(t *testing.T) {
	s := tempStore(t)
	if _, err := s.Enqueue(Item{TaskRef: "FAC-1", Kind: "herdr"}); err == nil {
		t.Error("expected missing idempotency key to be rejected")
	}
	if _, err := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1"}); err == nil {
		t.Error("expected missing kind to be rejected")
	}
}

func TestStore_DuplicateEnqueueIsIdempotentNoOp(t *testing.T) {
	s := tempStore(t)
	first, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "herdr", Payload: "a"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	second, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "herdr", Payload: "b"})
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("expected replay to return the original item id %d, got %d", first.ID, second.ID)
	}
	if second.Payload != "a" {
		t.Errorf("expected original payload to survive replay, got %q", second.Payload)
	}

	pending, err := s.Pending("", 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected exactly one pending item after duplicate enqueue, got %d", len(pending))
	}
}

func TestStore_PendingFiltersByKind(t *testing.T) {
	s := tempStore(t)
	s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})
	s.Enqueue(Item{IdempotencyKey: "k2", TaskRef: "FAC-1", Kind: "git"})

	herdrOnly, err := s.Pending("herdr", 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(herdrOnly) != 1 || herdrOnly[0].Kind != "herdr" {
		t.Errorf("expected 1 herdr item, got %+v", herdrOnly)
	}

	all, err := s.Pending("", 10)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 items across kinds, got %d", len(all))
	}
}

func TestStore_MarkSentRemovesFromPending(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})
	if err := s.MarkSent(item.ID); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	pending, _ := s.Pending("", 10)
	if len(pending) != 0 {
		t.Errorf("expected 0 pending items after mark sent, got %d", len(pending))
	}
}

func TestStore_MarkFailedIncrementsAttemptsAndRecordsError(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})
	if err := s.MarkFailed(item.ID, "boom", 5); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	pending, _ := s.Pending("", 10)
	if len(pending) != 1 {
		t.Fatalf("expected item to remain pending under max attempts, got %d pending", len(pending))
	}
	if pending[0].Attempts != 1 {
		t.Errorf("expected attempts=1, got %d", pending[0].Attempts)
	}
	if pending[0].LastError != "boom" {
		t.Errorf("expected last_error=boom, got %q", pending[0].LastError)
	}
}

func TestStore_MarkFailedGivesUpAtMaxAttempts(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})
	for i := 0; i < 3; i++ {
		if err := s.MarkFailed(item.ID, "boom", 3); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
	}
	pending, _ := s.Pending("", 10)
	if len(pending) != 0 {
		t.Fatalf("expected item to leave pending queue after max attempts, got %d", len(pending))
	}
	got, err := s.Get(item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("expected status=failed after exhausting attempts, got %s", got.Status)
	}
}

func TestStore_EnqueueTxSharesTransactionWithCaller(t *testing.T) {
	s := tempStore(t)
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := s.EnqueueTx(tx, Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"}); err != nil {
		tx.Rollback()
		t.Fatalf("enqueue tx: %v", err)
	}
	// Roll back: the item must never have been durably committed.
	tx.Rollback()

	pending, _ := s.Pending("", 10)
	if len(pending) != 0 {
		t.Errorf("expected rollback to discard the enqueued item, got %d pending", len(pending))
	}
}
