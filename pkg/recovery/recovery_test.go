package recovery

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestDecisionAndAttemptsSurviveReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "recovery.json")
	s, err := Open(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(Decision{Run: "r", Task: "t", Actor: "a", Evidence: "log", Disposition: Retry}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Attempt("r", "t"); err != nil || n != 1 {
		t.Fatalf("attempt=%d err=%v", n, err)
	}
	if n, err := s.Attempt("r", "t"); err != nil || n != 2 {
		t.Fatalf("attempt=%d err=%v", n, err)
	}
	s, err = Open(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Decisions("r", "t")) != 1 || s.Attempts("r", "t") != 2 {
		t.Fatal("state did not survive reopen")
	}
	if _, err := s.Attempt("r", "t"); !errors.Is(err, ErrMaxAttempts) {
		t.Fatalf("err=%v", err)
	}
}

func TestDecisionValidationAndReplan(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(Decision{Run: "r", Task: "t", Disposition: Retry}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err=%v", err)
	}
	p := Plan{Tasks: []Task{{ID: "done", Terminal: true}, {ID: "next", Depends: []string{"done"}}}}
	if err := s.Replan("r", p); err != nil {
		t.Fatal(err)
	}
	if err := s.Replan("r", Plan{Tasks: []Task{{ID: "done"}, {ID: "next", Depends: []string{"done"}}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("terminal mutation err=%v", err)
	}
	if err := s.Replan("cycle", Plan{Tasks: []Task{{ID: "a", Depends: []string{"b"}}, {ID: "b", Depends: []string{"a"}}}}); !errors.Is(err, ErrCycle) {
		t.Fatalf("cycle err=%v", err)
	}
	if err := s.Replan("orphan", Plan{Tasks: []Task{{ID: "a", Depends: []string{"missing"}}}}); !errors.Is(err, ErrOrphan) {
		t.Fatalf("orphan err=%v", err)
	}
}
