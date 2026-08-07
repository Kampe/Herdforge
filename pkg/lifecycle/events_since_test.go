package lifecycle

import "testing"

func TestEventsSince_TailsFromCursorInCommitOrder(t *testing.T) {
	m := tempMachineForReconcile(t)
	for i, to := range []State{StateEligible, StateClaimed} {
		if _, err := m.Transition(TransitionRequest{
			TaskRef: "FAC-1", Repo: "herdforge", To: to, Actor: "a",
			IdempotencyKey: string(rune('a'+i)) + "-fac1", LeaseGeneration: 1,
		}); err != nil {
			t.Fatalf("transition %s: %v", to, err)
		}
	}
	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-2", Repo: "herdforge", To: StateEligible, Actor: "a",
		IdempotencyKey: "fac2-eligible", LeaseGeneration: 1,
	}); err != nil {
		t.Fatalf("transition FAC-2: %v", err)
	}

	all, err := m.EventStore().EventsSince(0, 10)
	if err != nil {
		t.Fatalf("events since 0: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d events, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("ids are not ascending: %d then %d", all[i-1].ID, all[i].ID)
		}
	}

	tail, err := m.EventStore().EventsSince(all[0].ID, 10)
	if err != nil {
		t.Fatalf("events since cursor: %v", err)
	}
	if len(tail) != 2 || tail[0].ID != all[1].ID {
		t.Fatalf("cursor did not skip applied events: %+v", tail)
	}

	limited, err := m.EventStore().EventsSince(0, 1)
	if err != nil {
		t.Fatalf("events since with limit: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != all[0].ID {
		t.Fatalf("limit not honoured: %+v", limited)
	}

	if _, err := m.EventStore().EventsSince(0, 0); err == nil {
		t.Fatal("a non-positive limit was accepted")
	}
}
