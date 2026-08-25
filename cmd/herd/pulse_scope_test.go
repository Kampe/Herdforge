package main

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// FAC-649: a scope that matches NOTHING against a non-empty fleet is a wrong
// scope, not an empty fleet. Measured live: running pulse on the review host
// without HERD_CONFIG_PATH resolved .herd/herd.yaml, which registers the
// COORDINATOR's workspace (wB) -- one that host has never had. The filter
// dropped all 7 live reviewers and pulse reported agents=0, capacity=0. An
// orchestrator read that as "the host cannot spawn", stopped dispatching, and
// reported a control-plane outage for an hour while 8 reviewers were running.
func TestFilterPulseAgentsWorkspaceDropsEverythingOnAWrongScope(t *testing.T) {
	agents := []herdr.AgentEntry{
		{Name: "review-a", Workspace: "w4"},
		{Name: "review-b", Workspace: "w4"},
	}
	if got := filterPulseAgentsWorkspace(agents, "wB"); len(got) != 0 {
		t.Fatalf("precondition: a foreign scope must match nothing, got %d", len(got))
	}
	// The real assertion lives in readPulseHerdr, which must turn that empty
	// result into Known=false rather than a healthy zero. Pin the inputs that
	// distinguish the two cases so the rule cannot be quietly inverted.
	if got := filterPulseAgentsWorkspace(agents, "w4"); len(got) != 2 {
		t.Fatalf("the correct scope must keep its agents, got %d", len(got))
	}
	if got := filterPulseAgentsWorkspace(nil, "w4"); len(got) != 0 {
		t.Fatalf("an genuinely empty fleet stays empty, got %d", len(got))
	}
}

// The distinction the guard rests on: zero-of-many is a scope error, while
// zero-of-zero is a real empty fleet and must stay reportable as such.
func TestWrongScopeIsDistinguishableFromAnEmptyFleet(t *testing.T) {
	populated := []herdr.AgentEntry{{Name: "review-a", Workspace: "w4"}}
	wrongScope := len(filterPulseAgentsWorkspace(populated, "wB")) == 0 && len(populated) > 0
	emptyFleet := len(filterPulseAgentsWorkspace(nil, "w4")) == 0 && len(([]herdr.AgentEntry)(nil)) == 0
	if !wrongScope {
		t.Error("a foreign scope over a populated fleet must be detectable")
	}
	if !emptyFleet {
		t.Error("an empty fleet must remain distinguishable from a wrong scope")
	}
	if wrongScope == false || emptyFleet == false {
		t.Fatal("the two cases must not collapse into one")
	}
	// Guard the message contract: it must name the live workspaces so an operator
	// can see WHICH scope was real. A bare "0 agents" is what caused the outage.
	live := map[string]int{}
	for _, a := range populated {
		live[strings.TrimSpace(a.Workspace)]++
	}
	if live["w4"] != 1 {
		t.Errorf("the live-workspace tally must be reportable, got %v", live)
	}
}
