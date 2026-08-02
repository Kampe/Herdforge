package lifecycle

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/outbox"
)

func tempMachine(t *testing.T) *Machine {
	t.Helper()
	dir := t.TempDir()
	m, err := NewMachine(filepath.Join(dir, "lifecycle.db"))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestMachine_TransitionFromDraftToEligible(t *testing.T) {
	m := tempMachine(t)
	res, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 1,
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if res.Replayed {
		t.Error("expected first transition to not be a replay")
	}
	if res.Event.ToState != StateEligible || res.Event.Seq != 1 {
		t.Errorf("unexpected event: %+v", res.Event)
	}

	ts, err := m.EventStore().CurrentState("FAC-1")
	if err != nil {
		t.Fatalf("current state: %v", err)
	}
	if ts.State != StateEligible {
		t.Errorf("expected eligible, got %s", ts.State)
	}
}

func TestMachine_RejectsInvalidTransition(t *testing.T) {
	m := tempMachine(t)
	_, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateDispatched,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 1,
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition, got %v", err)
	}

	ts, _ := m.EventStore().CurrentState("FAC-1")
	if ts != nil {
		t.Errorf("expected no durable state change from a rejected transition, got %+v", ts)
	}
}

func TestMachine_IdempotentReplayReturnsSameEventNoDuplicate(t *testing.T) {
	m := tempMachine(t)
	req := TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 1,
	}
	first, err := m.Transition(req)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	second, err := m.Transition(req)
	if err != nil {
		t.Fatalf("replay transition: %v", err)
	}
	if !second.Replayed {
		t.Error("expected second identical call to be reported as a replay")
	}
	if second.Event.ID != first.Event.ID {
		t.Errorf("expected replay to return the same event id, got %d vs %d", second.Event.ID, first.Event.ID)
	}

	events, _ := m.EventStore().Events("FAC-1")
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 durable event after replay, got %d", len(events))
	}
}

func TestMachine_ReplayWithConflictingTargetIsRejected(t *testing.T) {
	m := tempMachine(t)
	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 1,
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}

	_, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateClaimed,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 1,
	})
	if err == nil {
		t.Fatal("expected reusing an idempotency key for a different transition to fail")
	}
}

func TestMachine_EnqueuesOutboxItemsAtomicallyWithTransition(t *testing.T) {
	m := tempMachine(t)
	_, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 1,
		OutboxItems: []outbox.Item{
			{IdempotencyKey: "k1:board", Kind: "provider", Payload: `{"status":"eligible"}`},
		},
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}

	pending, err := m.Outbox().Pending("provider", 10, time.Now())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].TaskRef != "FAC-1" {
		t.Fatalf("expected 1 pending provider outbox item for FAC-1, got %+v", pending)
	}
}

func TestMachine_ReplayFillsOutboxTaskRefIdenticallyToFreshAttempt(t *testing.T) {
	m := tempMachine(t)
	req := TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 1,
		OutboxItems: []outbox.Item{
			// TaskRef deliberately empty: Machine must fill it in from
			// req.TaskRef, and do so IDENTICALLY whether this is the
			// first attempt or a replay of the same idempotency key.
			{IdempotencyKey: "k1:board", Kind: "provider", Payload: `{"status":"eligible"}`},
		},
	}

	first, err := m.Transition(req)
	if err != nil {
		t.Fatalf("first transition: %v", err)
	}
	if first.Replayed {
		t.Fatal("expected the first call to not be a replay")
	}

	// Replay the exact same request. If the replay path filled TaskRef
	// differently (e.g. left it empty) the outbox's own fail-closed
	// idempotency check would reject this as a conflicting reuse of
	// "k1:board" and this call would return an error.
	second, err := m.Transition(req)
	if err != nil {
		t.Fatalf("replay transition: %v", err)
	}
	if !second.Replayed {
		t.Fatal("expected the second call to be a replay")
	}

	pending, err := m.Outbox().Pending("provider", 10, time.Now())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected exactly 1 outbox item after replay, got %d", len(pending))
	}
	if pending[0].TaskRef != "FAC-1" {
		t.Fatalf("expected outbox item task_ref=FAC-1 filled identically on replay, got %q", pending[0].TaskRef)
	}
}

func TestMachine_StaleLeaseGenerationRejected(t *testing.T) {
	m := tempMachine(t)
	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 2,
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}

	_, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateClaimed,
		Actor: "worker-b", IdempotencyKey: "k2", LeaseGeneration: 1,
	})
	if !errors.Is(err, ErrStaleLeaseGeneration) {
		t.Fatalf("expected ErrStaleLeaseGeneration, got %v", err)
	}

	ts, _ := m.EventStore().CurrentState("FAC-1")
	if ts.State != StateEligible {
		t.Errorf("expected state to remain eligible after stale-generation rejection, got %s", ts.State)
	}
}

func TestMachine_AdvancingLeaseGenerationSucceeds(t *testing.T) {
	m := tempMachine(t)
	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible,
		Actor: "worker-a", IdempotencyKey: "k1", LeaseGeneration: 1,
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}

	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateClaimed,
		Actor: "worker-b", IdempotencyKey: "k2", LeaseGeneration: 2,
	}); err != nil {
		t.Fatalf("expected advancing generation to succeed, got %v", err)
	}
}

func TestMachine_RequiresIdempotencyKey(t *testing.T) {
	m := tempMachine(t)
	_, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible, Actor: "a",
	})
	if err == nil {
		t.Fatal("expected missing idempotency key to be rejected")
	}
}
