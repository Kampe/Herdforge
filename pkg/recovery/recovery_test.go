package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/runstate"
)

func TestDecisionAndAttemptsSurviveReopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "recovery.json")
	s, err := Open(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Decide(Decision{Run: "r", Task: "t", Actor: "a", Evidence: "log", Disposition: Retry, Revision: 1, Graph: "g"}); err != nil {
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

func TestReplanBuildRunPreservesCompletionsAndBindsRevision(t *testing.T) {
	current := runstate.RunState{BuildRun: runstate.BuildRun{
		SchemaVersion: runstate.SchemaVersion, ID: "r", Goal: "g", Ref: "main", DependencyGraphRevision: "graph",
		Policy: runstate.Policy{Lane: "lane", Model: "model"}, Tasks: []runstate.TaskState{
			{ID: "done", Ref: "done", Status: provider.StatusDone, Terminal: true},
			{ID: "work", Ref: "work", Status: provider.StatusToDo},
		}}, Revision: 4}
	next, err := ReplanBuildRun(current, Plan{Revision: 4, Graph: "graph", Tasks: []Task{
		{ID: "done", Terminal: true}, {ID: "work", Depends: []string{"done"}}, {ID: "new", Depends: []string{"work"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 5 || len(next.Tasks) != 3 {
		t.Fatalf("unexpected replanned run: revision=%d tasks=%+v", next.Revision, next.Tasks)
	}
	for _, task := range next.Tasks {
		if task.ID == "done" && (!task.Terminal || task.Status != provider.StatusDone) {
			t.Fatalf("completion changed: %+v", task)
		}
	}
	_, err = ReplanBuildRun(current, Plan{Revision: 3, Graph: "graph", Tasks: []Task{{ID: "done", Terminal: true}, {ID: "work"}}})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("stale packet error=%v", err)
	}
}

func TestOpenRejectsMalformedDurableDecision(t *testing.T) {
	p := filepath.Join(t.TempDir(), "recovery.json")
	if err := os.WriteFile(p, []byte(`{"decisions":[{"run":"r","task":"t","actor":"a","evidence":"x","disposition":"abort"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed decision error=%v", err)
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

func TestRevisionBoundPacketRejectsStaleAndTerminalDispositions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	decision := Decision{Run: "r", Task: "t", Actor: "sentinel", Evidence: "e1", Disposition: Escalation, Revision: 2, Graph: "g2"}
	if err := s.Decide(decision); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidatePacket(Packet{Run: "r", Task: "t", Actor: "dispatch", Evidence: "e1", Revision: 1, Graph: "g1"}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale packet error=%v", err)
	}
	if err := s.ValidatePacket(Packet{Run: "r", Task: "t", Actor: "dispatch", Evidence: "e1", Revision: 2, Graph: "g2"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("escalation packet error=%v", err)
	}
}
