package mutationprobe

import (
	"errors"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "probes.db"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRegisterThenGet(t *testing.T) {
	s := newTestStore(t)
	r, err := s.Register(Receipt{
		ProbeID: "p1", TaskRef: "FAC-157", Generation: "g1",
		CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProbeName:    "herd-mutprobe.p1",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r.State != StateRegistered {
		t.Fatalf("state = %s, want registered", r.State)
	}
	got, err := s.Get("p1")
	if err != nil || got == nil || got.ProbeID != "p1" || got.TaskRef != "FAC-157" {
		t.Fatalf("Get = %+v err=%v", got, err)
	}
}

func TestRegisterRefusesAbsoluteProbeName(t *testing.T) {
	s := newTestStore(t)
	_, err := s.Register(Receipt{
		ProbeID: "p1", TaskRef: "FAC-157", Generation: "g1",
		CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProbeName:    "/tmp/herd-mutprobe.p1",
	})
	if err == nil {
		t.Fatal("absolute probe name must be refused")
	}
}

func TestRegisterIsIdempotentForSameIdentity(t *testing.T) {
	s := newTestStore(t)
	r := Receipt{
		ProbeID: "p1", TaskRef: "FAC-157", Generation: "g1",
		CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProbeName:    "herd-mutprobe.p1",
	}
	if _, err := s.Register(r); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := s.Register(r); err != nil {
		t.Fatalf("replay: %v", err)
	}
	all, err := s.ListAll()
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAll = %d err=%v", len(all), err)
	}
}

func TestRegisterRefusesIdentityConflict(t *testing.T) {
	s := newTestStore(t)
	r := Receipt{
		ProbeID: "p1", TaskRef: "FAC-157", Generation: "g1",
		CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProbeName:    "herd-mutprobe.p1",
	}
	if _, err := s.Register(r); err != nil {
		t.Fatalf("first: %v", err)
	}
	r.TaskRef = "FAC-999"
	if _, err := s.Register(r); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("err = %v, want ErrIdentityConflict", err)
	}
}

func TestMarkRemovedRequiresAbsenceProof(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Register(Receipt{
		ProbeID: "p1", TaskRef: "FAC-157", Generation: "g1",
		CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProbeName:    "herd-mutprobe.p1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRemoved("p1", false); !errors.Is(err, ErrAbsenceNotProved) {
		t.Fatalf("err = %v, want ErrAbsenceNotProved", err)
	}
}

func TestTransitionsOnUnknownProbeAreRefused(t *testing.T) {
	s := newTestStore(t)
	if err := s.MarkActive("ghost"); !errors.Is(err, ErrUnknownProbe) {
		t.Fatalf("err = %v", err)
	}
	if err := s.MarkAwaitingCleanup("ghost", "success"); !errors.Is(err, ErrUnknownProbe) {
		t.Fatalf("err = %v", err)
	}
}
