package lifecycle

import (
	"path/filepath"
	"testing"
)

func tempEventStore(t *testing.T) *EventStore {
	t.Helper()
	dir := t.TempDir()
	s, err := NewEventStore(filepath.Join(dir, "lifecycle.db"))
	if err != nil {
		t.Fatalf("new event store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustAppend(t *testing.T, s *EventStore, ev Event) Event {
	t.Helper()
	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	out, err := s.AppendTx(tx, ev)
	if err != nil {
		tx.Rollback()
		t.Fatalf("append: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return out
}

func TestEventStore_FirstEventStartsTaskAtSeqOne(t *testing.T) {
	s := tempEventStore(t)
	ev := mustAppend(t, s, Event{
		TaskRef: "FAC-1", Repo: "herdforge", FromState: StateDraft, ToState: StateEligible,
		Actor: "worker-a", IdempotencyKey: "k1",
	})
	if ev.Seq != 1 {
		t.Errorf("expected seq 1, got %d", ev.Seq)
	}

	ts, err := s.CurrentState("FAC-1")
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if ts.State != StateEligible {
		t.Errorf("expected state eligible, got %s", ts.State)
	}
	if ts.Seq != 1 {
		t.Errorf("expected task-state seq 1, got %d", ts.Seq)
	}
}

func TestEventStore_SeqIsMonotonicPerTask(t *testing.T) {
	s := tempEventStore(t)
	mustAppend(t, s, Event{TaskRef: "FAC-1", Repo: "herdforge", FromState: StateDraft, ToState: StateEligible, Actor: "a", IdempotencyKey: "k1"})
	ev2 := mustAppend(t, s, Event{TaskRef: "FAC-1", Repo: "herdforge", FromState: StateEligible, ToState: StateClaimed, Actor: "a", IdempotencyKey: "k2"})
	if ev2.Seq != 2 {
		t.Errorf("expected seq 2, got %d", ev2.Seq)
	}

	// A second, unrelated task starts its own sequence at 1.
	ev3 := mustAppend(t, s, Event{TaskRef: "FAC-2", Repo: "herdforge", FromState: StateDraft, ToState: StateEligible, Actor: "a", IdempotencyKey: "k3"})
	if ev3.Seq != 1 {
		t.Errorf("expected FAC-2 to start at seq 1, got %d", ev3.Seq)
	}
}

func TestEventStore_DuplicateIdempotencyKeyIsRejectedAtDBLevel(t *testing.T) {
	s := tempEventStore(t)
	mustAppend(t, s, Event{TaskRef: "FAC-1", Repo: "herdforge", FromState: StateDraft, ToState: StateEligible, Actor: "a", IdempotencyKey: "dup"})

	tx, err := s.DB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = s.AppendTx(tx, Event{TaskRef: "FAC-1", Repo: "herdforge", FromState: StateEligible, ToState: StateClaimed, Actor: "a", IdempotencyKey: "dup"})
	tx.Rollback()
	if err == nil {
		t.Fatal("expected duplicate idempotency key to be rejected")
	}
}

func TestEventStore_RequiresIdempotencyKey(t *testing.T) {
	s := tempEventStore(t)
	tx, _ := s.DB().Begin()
	_, err := s.AppendTx(tx, Event{TaskRef: "FAC-1", Repo: "herdforge", FromState: StateDraft, ToState: StateEligible, Actor: "a"})
	tx.Rollback()
	if err == nil {
		t.Fatal("expected empty idempotency key to be rejected")
	}
}

func TestEventStore_EventByIdempotencyKeyReturnsStoredEvent(t *testing.T) {
	s := tempEventStore(t)
	mustAppend(t, s, Event{TaskRef: "FAC-1", Repo: "herdforge", FromState: StateDraft, ToState: StateEligible, Actor: "a", IdempotencyKey: "k1"})

	ev, err := s.EventByIdempotencyKey("k1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ev == nil || ev.TaskRef != "FAC-1" {
		t.Fatalf("expected to find event for FAC-1, got %+v", ev)
	}

	missing, err := s.EventByIdempotencyKey("nope")
	if err != nil {
		t.Fatalf("lookup missing: %v", err)
	}
	if missing != nil {
		t.Errorf("expected nil for unknown key, got %+v", missing)
	}
}

func TestEventStore_CurrentStateUnknownTaskReturnsNil(t *testing.T) {
	s := tempEventStore(t)
	ts, err := s.CurrentState("FAC-999")
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if ts != nil {
		t.Errorf("expected nil task state for unknown task, got %+v", ts)
	}
}

func TestEventStore_EventsReturnsInSeqOrder(t *testing.T) {
	s := tempEventStore(t)
	mustAppend(t, s, Event{TaskRef: "FAC-1", Repo: "herdforge", FromState: StateDraft, ToState: StateEligible, Actor: "a", IdempotencyKey: "k1"})
	mustAppend(t, s, Event{TaskRef: "FAC-1", Repo: "herdforge", FromState: StateEligible, ToState: StateClaimed, Actor: "a", IdempotencyKey: "k2"})

	events, err := s.Events("FAC-1")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Errorf("expected events ordered by seq, got %+v", events)
	}
}

func TestEventStore_PersistsSchemaFields(t *testing.T) {
	s := tempEventStore(t)
	mustAppend(t, s, Event{
		TaskRef: "FAC-1", Repo: "herdforge", FromState: StateDraft, ToState: StateEligible,
		ProviderRevision: "rev-7", LeaseGeneration: 3, Branch: "task/fac-1", CandidateSHA: "abc123",
		Actor: "worker-a", EvidenceDigest: "sha256:deadbeef", Payload: `{"note":"hi"}`,
		IdempotencyKey: "k1",
	})

	ts, err := s.CurrentState("FAC-1")
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if ts.LeaseGeneration != 3 {
		t.Errorf("expected lease generation 3, got %d", ts.LeaseGeneration)
	}
	if ts.Branch != "task/fac-1" || ts.CandidateSHA != "abc123" {
		t.Errorf("expected branch/candidate sha to persist, got %+v", ts)
	}

	events, _ := s.Events("FAC-1")
	if events[0].EvidenceDigest != "sha256:deadbeef" || events[0].ProviderRevision != "rev-7" {
		t.Errorf("expected evidence digest/provider revision to persist, got %+v", events[0])
	}
}
