package stop

import "testing"

// The core safety property: an agent that may hold uncommitted work is never
// closed by a plain stop.
func TestActiveAgentsArePreservedNotClosed(t *testing.T) {
	agents := []Agent{
		{Name: "smith", Status: "working", PaneID: "p1", TabID: "t1"},
		{Name: "scout", Status: "starting", PaneID: "p2", TabID: "t2"},
		{Name: "assayer", Status: "idle", PaneID: "p3", TabID: "t3"},
	}
	plan := Plan(agents, Options{})
	if plan[0].Action != Preserve || plan[1].Action != Preserve {
		t.Fatalf("active agents must be preserved: %+v", plan)
	}
	if !plan[0].RequestStop || !plan[1].RequestStop {
		t.Fatal("preserved agents must still be asked to stop")
	}
	if plan[2].Action != Close {
		t.Fatalf("settled agent must be closed: %+v", plan[2])
	}
	if s := Summarize(plan); s.Close != 1 || s.Preserved != 2 {
		t.Fatalf("summary = %+v", s)
	}
}

// --force-working is the explicit "kill them all" instruction.
func TestForceWorkingClosesActiveAgents(t *testing.T) {
	plan := Plan([]Agent{{Name: "smith", Status: "working", TabID: "t1"}}, Options{ForceWorking: true})
	if plan[0].Action != Close {
		t.Fatalf("force-working must close active agents: %+v", plan[0])
	}
	if !plan[0].RequestStop {
		t.Fatal("force-working must still request a graceful stop first")
	}
}

// Closing the tab that issued the stop would orphan the fleet mid-wind-down.
func TestCoordinatorProtectedUnlessExplicit(t *testing.T) {
	agents := []Agent{
		{Name: "herdforge-orchestrator", Status: "working", TabID: "t1"},
		{Name: "coordinator", Status: "idle", TabID: "t2"},
	}
	for _, d := range Plan(agents, Options{}) {
		if d.Action != Protect {
			t.Fatalf("coordinator must be protected by default: %+v", d)
		}
	}
	plan := Plan(agents, Options{IncludeCoordinator: true, ForceWorking: true})
	for _, d := range plan {
		if d.Action != Close {
			t.Fatalf("--include-coordinator must allow closing: %+v", d)
		}
	}
}

func TestStandingLanesAreHeldOutOfTheKickLoop(t *testing.T) {
	plan := Plan([]Agent{
		{Name: "smith", Status: "idle", TabID: "t1"},
		{Name: "oneoff", Status: "idle", TabID: "t2"},
	}, Options{StandingLanes: map[string]bool{"smith": true}})
	if !plan[0].Hold {
		t.Fatal("standing lane must be held so the kick loop does not revive it")
	}
	if plan[1].Hold {
		t.Fatal("one-off tab must not be held")
	}
}

func TestIsCoordinatorMatchesShellPredicate(t *testing.T) {
	for _, n := range []string{"coordinator", "herdforge-orchestrator", "ORCHESTRATOR-2"} {
		if !IsCoordinator(n) {
			t.Fatalf("%q must match", n)
		}
	}
	for _, n := range []string{"smith", "scout", "assayer", "coord-helper"} {
		if IsCoordinator(n) {
			t.Fatalf("%q must not match", n)
		}
	}
}
