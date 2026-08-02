package outbox

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
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

func TestStore_DuplicateEnqueueWithIdenticalFieldsIsIdempotentNoOp(t *testing.T) {
	s := tempStore(t)
	first, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "herdr", Payload: "a"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	second, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "herdr", Payload: "a"})
	if err != nil {
		t.Fatalf("re-enqueue with identical fields: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("expected replay to return the original item id %d, got %d", first.ID, second.ID)
	}

	pending, err := s.Pending("", 10, time.Now())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected exactly one pending item after duplicate enqueue, got %d", len(pending))
	}
}

func TestStore_DuplicateEnqueueWithDifferentPayloadFailsClosed(t *testing.T) {
	s := tempStore(t)
	original, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "herdr", Payload: "a"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, err = s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "herdr", Payload: "b"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict for a differing payload, got %v", err)
	}

	got, _ := s.Get(original.ID)
	if got.Payload != "a" {
		t.Errorf("expected original payload to survive a rejected reuse, got %q", got.Payload)
	}
}

func TestStore_DuplicateEnqueueWithDifferentTaskRefFailsClosed(t *testing.T) {
	s := tempStore(t)
	if _, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "herdr"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-2", Kind: "herdr"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict for a differing task_ref, got %v", err)
	}
}

func TestStore_DuplicateEnqueueWithDifferentKindFailsClosed(t *testing.T) {
	s := tempStore(t)
	if _, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "herdr"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	_, err := s.Enqueue(Item{IdempotencyKey: "dup", TaskRef: "FAC-1", Kind: "git"})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict for a differing kind, got %v", err)
	}
}

func TestStore_PendingFiltersByKind(t *testing.T) {
	s := tempStore(t)
	s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})
	s.Enqueue(Item{IdempotencyKey: "k2", TaskRef: "FAC-1", Kind: "git"})

	herdrOnly, err := s.Pending("herdr", 10, time.Now())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(herdrOnly) != 1 || herdrOnly[0].Kind != "herdr" {
		t.Errorf("expected 1 herdr item, got %+v", herdrOnly)
	}

	all, err := s.Pending("", 10, time.Now())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 items across kinds, got %d", len(all))
	}
}

func TestStore_ClaimIsExclusive(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})

	first, err := s.Claim(item.ID)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.Status != StatusInFlight {
		t.Errorf("expected in_flight after claim, got %s", first.Status)
	}

	_, err = s.Claim(item.ID)
	if !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("expected second concurrent claim to fail with ErrNotClaimable, got %v", err)
	}

	pending, _ := s.Pending("", 10, time.Now())
	if len(pending) != 0 {
		t.Errorf("expected claimed item to disappear from Pending, got %d", len(pending))
	}
}

func TestStore_ClaimUnknownOrAlreadyResolvedItemFails(t *testing.T) {
	s := tempStore(t)
	if _, err := s.Claim(999); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("expected ErrNotClaimable for unknown id, got %v", err)
	}
}

func TestStore_MarkSentRequiresInFlight(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})

	if err := s.MarkSent(item.ID); !errors.Is(err, ErrNotInFlight) {
		t.Fatalf("expected ErrNotInFlight for a still-pending item, got %v", err)
	}

	claimed, _ := s.Claim(item.ID)
	if err := s.MarkSent(claimed.ID); err != nil {
		t.Fatalf("mark sent after claim: %v", err)
	}
	got, _ := s.Get(item.ID)
	if got.Status != StatusSent {
		t.Errorf("expected sent, got %s", got.Status)
	}

	if err := s.MarkSent(item.ID); !errors.Is(err, ErrNotInFlight) {
		t.Fatalf("expected double mark-sent to fail closed, got %v", err)
	}
}

func TestStore_MarkFailedRequiresInFlightAndIncrementsAttempts(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})

	if err := s.MarkFailed(item.ID, "boom", 5); !errors.Is(err, ErrNotInFlight) {
		t.Fatalf("expected ErrNotInFlight for a still-pending item, got %v", err)
	}

	claimed, _ := s.Claim(item.ID)
	if err := s.MarkFailed(claimed.ID, "boom", 5); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got, _ := s.Get(item.ID)
	if got.Attempts != 1 {
		t.Errorf("expected attempts=1, got %d", got.Attempts)
	}
	if got.LastError != "boom" {
		t.Errorf("expected last_error=boom, got %q", got.LastError)
	}
	if got.Status != StatusPending {
		t.Errorf("expected status back to pending under max attempts, got %s", got.Status)
	}
	if got.NextAttemptAt == nil {
		t.Fatal("expected next_attempt_at to be set on a retryable failure")
	}
}

func TestStore_MarkFailedGivesUpAtMaxAttempts(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})

	for i := 0; i < 3; i++ {
		claimed, err := s.Claim(item.ID)
		if err != nil {
			t.Fatalf("claim attempt %d: %v", i, err)
		}
		if err := s.MarkFailed(claimed.ID, "boom", 3); err != nil {
			t.Fatalf("mark failed attempt %d: %v", i, err)
		}
	}

	pending, _ := s.Pending("", 10, time.Now().Add(time.Hour))
	if len(pending) != 0 {
		t.Fatalf("expected item to leave the pending pool after max attempts, got %d", len(pending))
	}
	got, err := s.Get(item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("expected status=failed after exhausting attempts, got %s", got.Status)
	}
	if got.NextAttemptAt != nil {
		t.Errorf("expected no next_attempt_at once terminally failed, got %v", got.NextAttemptAt)
	}
}

func TestStore_PendingExcludesItemsBeforeNextAttempt(t *testing.T) {
	s := tempStore(t)
	item, _ := s.Enqueue(Item{IdempotencyKey: "k1", TaskRef: "FAC-1", Kind: "herdr"})
	claimed, _ := s.Claim(item.ID)
	if err := s.MarkFailed(claimed.ID, "boom", 10); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	tooSoon, err := s.Pending("", 10, time.Now())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(tooSoon) != 0 {
		t.Fatalf("expected backoff to exclude the item immediately after failure, got %d", len(tooSoon))
	}

	due, err := s.Pending("", 10, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected the item to become due after its backoff elapses, got %d", len(due))
	}
}

func TestStore_BackoffIsBoundedAndIncreasing(t *testing.T) {
	d1 := nextAttemptDelay(1)
	d2 := nextAttemptDelay(2)
	d3 := nextAttemptDelay(10)
	if d2 <= d1 {
		t.Errorf("expected backoff to increase: attempt1=%v attempt2=%v", d1, d2)
	}
	if d3 > maxBackoff {
		t.Errorf("expected backoff to stay bounded at %v, got %v for a large attempt count", maxBackoff, d3)
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

	pending, _ := s.Pending("", 10, time.Now())
	if len(pending) != 0 {
		t.Errorf("expected rollback to discard the enqueued item, got %d pending", len(pending))
	}
}

func TestStore_AppliesSQLiteConcurrencyContract(t *testing.T) {
	s := tempStore(t)
	var journalMode string
	if err := s.DB().QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Errorf("expected journal_mode=wal, got %s", journalMode)
	}

	var busyTimeout int
	if err := s.DB().QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeout != sqliteBusyTimeoutMillis {
		t.Errorf("expected busy_timeout=%d, got %d", sqliteBusyTimeoutMillis, busyTimeout)
	}
}
