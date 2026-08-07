package containerlifecycle

import (
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "receipts.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRegisterThenGet(t *testing.T) {
	s := newTestStore(t)
	r, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1", ImageDigest: "sha256:abc"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r.State != StateRegistered {
		t.Fatalf("state = %s, want registered", r.State)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.ContainerID != "c1" || got.TaskRef != "FAC-200" || got.Generation != "g1" {
		t.Fatalf("Get = %+v", got)
	}
}

func TestRegisterIsIdempotentForSameIdentity(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	r2, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"})
	if err != nil {
		t.Fatalf("replay Register: %v", err)
	}
	if r2.State != StateRegistered {
		t.Fatalf("replay changed state to %s", r2.State)
	}
	all, err := s.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("replay created a duplicate row: %d rows", len(all))
	}
}

func TestRegisterRefusesIdentityConflict(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Same container ID claimed by a different task — Docker reuses IDs
	// only after removal, but a stale/forged receipt reuse must still be
	// refused, not silently take over ownership.
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-201", Generation: "g1"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("err = %v, want ErrIdentityConflict", err)
	}
	// Same task, later generation — also a conflict, not a silent update.
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g2"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("err = %v, want ErrIdentityConflict", err)
	}
}

func TestTransitionsOnUnknownContainerAreRefused(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkStarted("ghost"); !errors.Is(err, ErrUnknownContainer) {
		t.Fatalf("MarkStarted err = %v, want ErrUnknownContainer", err)
	}
	if err := s.MarkAwaitingCleanup("ghost", "success"); !errors.Is(err, ErrUnknownContainer) {
		t.Fatalf("MarkAwaitingCleanup err = %v, want ErrUnknownContainer", err)
	}
	if err := s.MarkRemoved("ghost", true); !errors.Is(err, ErrUnknownContainer) {
		t.Fatalf("MarkRemoved err = %v, want ErrUnknownContainer", err)
	}
	if err := s.MarkQuarantined("ghost", "reason"); !errors.Is(err, ErrUnknownContainer) {
		t.Fatalf("MarkQuarantined err = %v, want ErrUnknownContainer", err)
	}
}

func TestFullLifecycleTransitions(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.MarkStarted("c1"); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	if err := s.MarkAwaitingCleanup("c1", "success"); err != nil {
		t.Fatalf("MarkAwaitingCleanup: %v", err)
	}
	if err := s.MarkRemoved("c1", true); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateRemoved || !got.AbsenceProved || got.RemovedAt == nil {
		t.Fatalf("got = %+v", got)
	}
	if got.ExpectedTerminalState != "success" {
		t.Fatalf("expected_terminal_state = %q, want success", got.ExpectedTerminalState)
	}
}

func TestMarkRemovedRequiresAbsenceProof(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.MarkRemoved("c1", false); !errors.Is(err, ErrAbsenceNotProved) {
		t.Fatalf("err = %v, want ErrAbsenceNotProved", err)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateRegistered {
		t.Fatalf("state changed to %s despite unproved absence", got.State)
	}
}

func TestInvalidTransitionRefused(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.MarkRemoved("c1", true); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}
	// Removed is terminal; starting it again must be refused, not replayed.
	if err := s.MarkStarted("c1"); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
}

func TestQuarantineIsTerminalAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{ContainerID: "c1", TaskRef: "FAC-200", Generation: "g1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.MarkQuarantined("c1", "remove failed"); err != nil {
		t.Fatalf("MarkQuarantined: %v", err)
	}
	// Re-quarantining the same receipt is a no-op, not an error.
	if err := s.MarkQuarantined("c1", "remove failed again"); err != nil {
		t.Fatalf("re-quarantine: %v", err)
	}
	got, err := s.Get("c1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateQuarantined || got.LastError != "remove failed" {
		t.Fatalf("got = %+v", got)
	}
}

func TestListNonTerminalExcludesRemovedAndQuarantined(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"active", "removed", "quarantined"} {
		if _, err := s.Register(Receipt{ContainerID: id, TaskRef: "FAC-200", Generation: "g1"}); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
	}
	if err := s.MarkRemoved("removed", true); err != nil {
		t.Fatalf("MarkRemoved: %v", err)
	}
	if err := s.MarkQuarantined("quarantined", "boom"); err != nil {
		t.Fatalf("MarkQuarantined: %v", err)
	}
	nonTerminal, err := s.ListNonTerminal()
	if err != nil {
		t.Fatalf("ListNonTerminal: %v", err)
	}
	if len(nonTerminal) != 1 || nonTerminal[0].ContainerID != "active" {
		t.Fatalf("ListNonTerminal = %+v, want only 'active'", nonTerminal)
	}
}
