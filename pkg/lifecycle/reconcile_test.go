package lifecycle

import (
	"path/filepath"
	"testing"
	"time"
)

func tempMachineForReconcile(t *testing.T) *Machine {
	t.Helper()
	dir := t.TempDir()
	m, err := NewMachine(filepath.Join(dir, "lifecycle.db"))
	if err != nil {
		t.Fatalf("new machine: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestReconciler_SkipsFreshTasks(t *testing.T) {
	m := tempMachineForReconcile(t)
	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible, Actor: "a", IdempotencyKey: "k1", LeaseGeneration: 1,
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}
	ts, _ := m.EventStore().CurrentState("FAC-1")

	r := NewReconciler(m)
	r.Now = func() time.Time { return ts.UpdatedAt }
	actions, err := r.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("expected no actions for a fresh task, got %+v", actions)
	}
}

func TestReconciler_MarksStaleNonTerminalTaskAsRecovering(t *testing.T) {
	m := tempMachineForReconcile(t)
	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible, Actor: "a", IdempotencyKey: "k1", LeaseGeneration: 1,
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}
	ts, _ := m.EventStore().CurrentState("FAC-1")

	r := NewReconciler(m)
	r.Now = func() time.Time { return ts.UpdatedAt.Add(time.Hour) }
	actions, err := r.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %+v", actions)
	}
	if actions[0].TaskRef != "FAC-1" || actions[0].From != StateEligible || actions[0].To != StateRecovering {
		t.Errorf("unexpected action: %+v", actions[0])
	}

	after, _ := m.EventStore().CurrentState("FAC-1")
	if after.State != StateRecovering {
		t.Errorf("expected state recovering, got %s", after.State)
	}
}

func TestReconciler_EscalatesStaleRecoveringToBlocked(t *testing.T) {
	m := tempMachineForReconcile(t)
	if _, err := m.Transition(TransitionRequest{
		TaskRef: "FAC-1", Repo: "herdforge", To: StateEligible, Actor: "a", IdempotencyKey: "k1", LeaseGeneration: 1,
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}
	ts, _ := m.EventStore().CurrentState("FAC-1")

	r := NewReconciler(m)
	r.Now = func() time.Time { return ts.UpdatedAt.Add(time.Hour) }
	if _, err := r.Reconcile(); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	recovering, _ := m.EventStore().CurrentState("FAC-1")
	if recovering.State != StateRecovering {
		t.Fatalf("expected recovering after first sweep, got %s", recovering.State)
	}

	r.Now = func() time.Time { return recovering.UpdatedAt.Add(time.Hour) }
	actions, err := r.Reconcile()
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(actions) != 1 || actions[0].From != StateRecovering || actions[0].To != StateBlocked {
		t.Fatalf("expected Recovering -> Blocked escalation, got %+v", actions)
	}

	final, _ := m.EventStore().CurrentState("FAC-1")
	if final.State != StateBlocked {
		t.Errorf("expected state blocked, got %s", final.State)
	}
}

func TestReconciler_IgnoresTerminalTasks(t *testing.T) {
	m := tempMachineForReconcile(t)
	steps := []State{StateEligible, StateClaimed, StateDispatched, StateBuilding, StateVerifying, StateReviewing, StateIntegrationQueued, StateIntegrated, StateReconciled, StateCleaned}
	for i, to := range steps {
		if _, err := m.Transition(TransitionRequest{
			TaskRef: "FAC-1", Repo: "herdforge", To: to, Actor: "a",
			IdempotencyKey: string(rune('a' + i)), LeaseGeneration: 1,
		}); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	ts, _ := m.EventStore().CurrentState("FAC-1")
	if ts.State != StateCleaned {
		t.Fatalf("setup failed, expected cleaned, got %s", ts.State)
	}

	r := NewReconciler(m)
	r.Now = func() time.Time { return ts.UpdatedAt.Add(24 * time.Hour) }
	actions, err := r.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("expected terminal (Cleaned) task to never be swept, got %+v", actions)
	}
}

func TestReconciler_EmptyStoreProducesNoActions(t *testing.T) {
	m := tempMachineForReconcile(t)
	r := NewReconciler(m)
	actions, err := r.Reconcile()
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(actions) != 0 {
		t.Errorf("expected no actions on an empty store, got %+v", actions)
	}
}
