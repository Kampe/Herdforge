package lifecycle

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// nextRaceTarget always returns a legal next state from any state, so
// every attempted transition in the test below is well-formed and any
// failure is purely a concurrency loss (ErrConcurrentModification /
// ErrStaleLeaseGeneration), never an ErrInvalidTransition.
func nextRaceTarget(from State) State {
	switch from {
	case StateDraft:
		return StateEligible
	case StateRecovering:
		return StateBlocked
	case StateBlocked:
		return StateRecovering
	default:
		return StateRecovering
	}
}

// TestMachine_TwoHandlesConcurrentTransitionsNeverProduceDiscontinuousChain
// is the regression test for the bug an independent R3 review found: the
// old Machine.Transition derived FromState from a read taken OUTSIDE the
// write transaction, so two callers racing on the same task could each
// commit an event whose FromState didn't match the actual prior ToState.
//
// It opens TWO separate Machine instances (two separate *sql.DB, two
// separate in-process mutexes, exactly like two OS processes) against the
// SAME database file and hammers one task from both concurrently. No
// matter how the two handles interleave, the durable event chain must
// never show events[i].FromState != events[i-1].ToState — every legal
// transition is well-formed, so the only way to fail is a resurrected
// stale-read race.
func TestMachine_TwoHandlesConcurrentTransitionsNeverProduceDiscontinuousChain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lifecycle.db")

	m1, err := NewMachine(path)
	if err != nil {
		t.Fatalf("new machine 1: %v", err)
	}
	defer m1.Close()
	m2, err := NewMachine(path)
	if err != nil {
		t.Fatalf("new machine 2: %v", err)
	}
	defer m2.Close()

	const taskRef = "FAC-RACE-1"
	const workers = 8
	const attemptsPerWorker = 25
	const maxRetriesPerAttempt = 200

	var wg sync.WaitGroup
	var successCount int64
	for w := 0; w < workers; w++ {
		machine := m1
		if w%2 == 1 {
			machine = m2
		}
		wg.Add(1)
		go func(worker int, machine *Machine) {
			defer wg.Done()
			for i := 0; i < attemptsPerWorker; i++ {
				for attempt := 0; attempt < maxRetriesPerAttempt; attempt++ {
					ts, err := machine.EventStore().CurrentState(taskRef)
					if err != nil {
						t.Errorf("current state: %v", err)
						return
					}
					from := StateDraft
					if ts != nil {
						from = ts.State
					}
					to := nextRaceTarget(from)
					key := fmt.Sprintf("worker-%d-iter-%d-attempt-%d-%s->%s", worker, i, attempt, from, to)

					_, err = machine.Transition(TransitionRequest{
						TaskRef: taskRef, Repo: "herdforge", To: to,
						Actor: fmt.Sprintf("worker-%d", worker), IdempotencyKey: key,
						LeaseGeneration: 1,
					})
					if err == nil {
						atomic.AddInt64(&successCount, 1)
						break
					}
					// Lost the race — another handle committed first.
					// Retry with a fresh read, exactly what a real caller
					// must do on ErrConcurrentModification.
				}
			}
		}(w, machine)
	}
	wg.Wait()

	if successCount == 0 {
		t.Fatal("expected at least some transitions to succeed")
	}

	events, err := m1.EventStore().Events(taskRef)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("expected at least one recorded event")
	}
	if events[0].FromState != StateDraft {
		t.Errorf("expected first event from_state=draft, got %s", events[0].FromState)
	}
	for i := 1; i < len(events); i++ {
		if events[i].FromState != events[i-1].ToState {
			t.Fatalf("DISCONTINUOUS CHAIN at seq %d: prior to_state=%s but this from_state=%s",
				events[i].Seq, events[i-1].ToState, events[i].FromState)
		}
		if events[i].Seq != events[i-1].Seq+1 {
			t.Fatalf("non-monotonic seq: %d followed by %d", events[i-1].Seq, events[i].Seq)
		}
	}
}
