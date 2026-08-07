package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/spin"
)

func spinGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := testgit.Command(dir, args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// spinRepo builds a repo on branch `branch` with one commit beyond an
// origin/main that also exists locally.
func spinRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	spinGit(t, dir, "init", "-q", "-b", "main")
	spinGit(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	spinGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	spinGit(t, dir, "checkout", "-q", "-b", branch)
	return dir
}

// The lifecycle signals are only reachable if the task ref can be recovered
// from the branch worktree.TaskBranch minted. A wrong answer here silently
// drops candidate SHA and lifecycle sequence from the evidence.
func TestTaskRefForWorktreeRecoversTheRefOrNothing(t *testing.T) {
	if got := taskRefForWorktree(spinRepo(t, "herd/fac-90")); got != "FAC-90" {
		t.Fatalf("taskRefForWorktree = %q, want FAC-90", got)
	}
	// A branch this repo did not mint must not be guessed into a task ref.
	if got := taskRefForWorktree(spinRepo(t, "feature/whatever")); got != "" {
		t.Fatalf("non-herd branch must yield no task ref, got %q", got)
	}
	if got := taskRefForWorktree(t.TempDir()); got != "" {
		t.Fatalf("non-repo must yield no task ref, got %q", got)
	}
	if got := taskRefForWorktree(""); got != "" {
		t.Fatalf("empty dir must yield no task ref, got %q", got)
	}
}

// A failed unique-work check must read Unknown, never No — No authorizes a
// recovery transition.
func TestUniqueWorkStateFailsClosedOnAFailedCheck(t *testing.T) {
	ctx := context.Background()
	root := spinRepo(t, "herd/fac-90")
	h := harvest.NewHarvester(root)

	if got := uniqueWorkState(ctx, h, root); got != spin.TriNo {
		t.Fatalf("a branch with no commits past origin/main must be TriNo, got %q", got)
	}

	spinGit(t, root, "commit", "-q", "--allow-empty", "-m", "unique work")
	if got := uniqueWorkState(ctx, h, root); got != spin.TriYes {
		t.Fatalf("a branch with a unique commit must be TriYes, got %q", got)
	}

	// Not a git worktree at all: the check cannot answer, so neither can we.
	if got := uniqueWorkState(ctx, h, t.TempDir()); got != spin.TriUnknown {
		t.Fatalf("an unanswerable check must be TriUnknown, got %q", got)
	}
}

// The recovery transition is the only durable side effect spin can take.
// It must land on Recovering, be idempotent across repeated sweeps, and
// refuse outright when there is no lifecycle store to transition in.
func TestPerformSpinActionRecoveryIsDurableAndIdempotent(t *testing.T) {
	machine, err := lifecycle.NewMachine(filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()

	const ref = "FAC-90"
	for _, to := range []lifecycle.State{
		lifecycle.StateEligible, lifecycle.StateClaimed,
		lifecycle.StateDispatched, lifecycle.StateBuilding,
	} {
		if _, err := machine.Transition(lifecycle.TransitionRequest{
			TaskRef: ref, Repo: "herdforge", To: to, Actor: "test",
			IdempotencyKey: "setup:" + string(to), CandidateSHA: "abc123",
		}); err != nil {
			t.Fatalf("setup %s: %v", to, err)
		}
	}

	ts, err := machine.EventStore().CurrentState(ref)
	if err != nil || ts == nil {
		t.Fatalf("current state: %v", err)
	}
	assessment := spin.Assessment{
		Cause: spin.CauseCrashLoop, NextAction: spin.ActionRecover,
		NoProgressCycles: 4, RestartCycles: 2,
	}
	agent := herdr.AgentEntry{PaneID: "p1", Name: "smith"}

	if err := performSpinAction(assessment, agent, machine, ts); err != nil {
		t.Fatalf("recovery transition: %v", err)
	}
	after, err := machine.EventStore().CurrentState(ref)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != lifecycle.StateRecovering {
		t.Fatalf("state = %s, want recovering", after.State)
	}

	// Re-running the same sweep must replay, not pile up a second event.
	before := len(eventsFor(t, machine, ref))
	if err := performSpinAction(assessment, agent, machine, ts); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := len(eventsFor(t, machine, ref)); got != before {
		t.Fatalf("replay appended events: %d -> %d", before, got)
	}

	// Without a lifecycle store there is nothing to transition; the caller
	// must learn that rather than believe a recovery happened.
	if err := performSpinAction(assessment, agent, nil, nil); err == nil {
		t.Fatal("recovery without a lifecycle store must fail")
	}
	if err := performSpinAction(assessment, agent, machine, nil); err == nil {
		t.Fatal("recovery without task state must fail")
	}
}

func eventsFor(t *testing.T, m *lifecycle.Machine, ref string) []lifecycle.Event {
	t.Helper()
	events, err := m.EventStore().Events(ref)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// Nudge and recover are the only performable actions; everything else is a
// recommendation an operator owns.
func TestPerformSpinActionRefusesNonPerformableActions(t *testing.T) {
	agent := herdr.AgentEntry{PaneID: "p1", Name: "smith"}
	for _, action := range []spin.Action{spin.ActionNone, spin.ActionObserve, spin.ActionOperator} {
		err := performSpinAction(spin.Assessment{NextAction: action}, agent, nil, nil)
		if err == nil {
			t.Errorf("action %q must not be performable", action)
		}
	}
}
