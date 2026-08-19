package goalguard

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testGoal(t *testing.T) (*Store, Goal, Evidence) {
	t.Helper()
	now := time.Date(2026, time.August, 16, 2, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "goal.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	g := Goal{Lane: "forge-worker", Task: "FAC-308", Owner: "coordinator", Generation: 7, MaxContinuations: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.Set(g); err != nil {
		t.Fatal(err)
	}
	e := Evidence{Lane: g.Lane, Task: g.Task, Owner: g.Owner, Generation: g.Generation, LeaseHeld: true, Now: now}
	return s, g, e
}

func TestEvaluateContinuesUntilBoundThenStops(t *testing.T) {
	s, _, e := testGoal(t)
	first, err := s.Evaluate(e)
	if err != nil || !first.Continue || first.Continuations != 1 {
		t.Fatalf("first decision = %+v, err=%v", first, err)
	}
	second, err := s.Evaluate(e)
	if err != nil || !second.Continue || second.Continuations != 2 {
		t.Fatalf("second decision = %+v, err=%v", second, err)
	}
	third, err := s.Evaluate(e)
	if err != nil || third.Continue || third.Reason != "max_continuations" || third.Continuations != 2 {
		t.Fatalf("bounded decision = %+v, err=%v", third, err)
	}
}

func TestEvaluateStopsForEveryTerminalCondition(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Evidence, *Goal)
		want   string
	}{
		{"completed", func(e *Evidence, g *Goal) { e.Completed = true }, "completed"},
		{"lease lost", func(e *Evidence, g *Goal) { e.LeaseHeld = false }, "lease_lost"},
		{"held", func(e *Evidence, g *Goal) { e.Held = true }, "held"},
		{"wind down", func(e *Evidence, g *Goal) { e.WindDown = true }, "wind_down"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, g, e := testGoal(t)
			tc.mutate(&e, &g)
			got, err := s.Evaluate(e)
			if err != nil || got.Continue || got.Reason != tc.want || got.Continuations != 0 {
				t.Fatalf("decision = %+v, err=%v", got, err)
			}
		})
	}
}

func TestEvaluateStopsAfterExpiry(t *testing.T) {
	s, g, e := testGoal(t)
	expiry := e.Now.Add(-time.Second)
	g.ExpiresAt = &expiry
	if err := s.Set(g); err != nil {
		t.Fatal(err)
	}
	got, err := s.Evaluate(e)
	if err != nil || got.Continue || got.Reason != "expired" {
		t.Fatalf("decision = %+v, err=%v", got, err)
	}
}

func TestRestartReloadsConsumedBudget(t *testing.T) {
	s, _, e := testGoal(t)
	if _, err := s.Evaluate(e); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Load()
	if err != nil || got.Continuations != 1 {
		t.Fatalf("restarted goal = %+v, err=%v", got, err)
	}
}

func TestCorruptAndStaleEvidenceFailClosed(t *testing.T) {
	s, _, e := testGoal(t)
	if err := os.WriteFile(s.path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evaluate(e); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt state err=%v, want ErrCorrupt", err)
	}
	s, _, e = testGoal(t)
	e.Generation++
	if _, err := s.Evaluate(e); !errors.Is(err, ErrStale) {
		t.Fatalf("stale evidence err=%v, want ErrStale", err)
	}
	missing, err := Open(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := missing.Evaluate(e); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing state err=%v, want ErrMissing", err)
	}
}

func TestContradictoryGoalCannotBePersisted(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "goal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(Goal{Lane: "lane", Task: "task", Owner: "owner", Generation: 1, MaxContinuations: -1}); err == nil {
		t.Fatal("negative continuation budget must be rejected")
	}
	if err := s.Set(Goal{Lane: "lane", Task: "task", Owner: "owner", Generation: 1, MaxContinuations: 0}); err != nil {
		t.Fatalf("zero budget means unbounded and must persist, got %v", err)
	}
}

func TestUnboundedGoalContinuesUntilStopCondition(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "goal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(Goal{Lane: "lane", Task: "task", Owner: "owner", Generation: 1, MaxContinuations: 0}); err != nil {
		t.Fatal(err)
	}
	e := Evidence{Lane: "lane", Task: "task", Owner: "owner", Generation: 1, LeaseHeld: true, Now: time.Now().UTC()}
	for i := 0; i < 50; i++ {
		d, err := s.Evaluate(e)
		if err != nil || !d.Continue {
			t.Fatalf("iteration %d: decision=%+v err=%v, want unbounded continue", i, d, err)
		}
	}
	e.Completed = true
	if d, err := s.Evaluate(e); err != nil || d.Continue || d.Reason != "completed" {
		t.Fatalf("completed evidence must stop: decision=%+v err=%v", d, err)
	}
}
