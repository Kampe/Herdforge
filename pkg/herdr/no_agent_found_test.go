package herdr

import (
	"strings"
	"testing"
)

// FAC-597: "no agent 'X' found" was the one refusal in this file that named
// nothing but the failed target. Its immediate neighbours already name their
// candidates ("exact live agent %q and forge-derived live agent %q"), so an
// operator hitting an AMBIGUOUS target learned more than one hitting an ABSENT
// target.
//
// Measured: the review supervisor lane was reaped, and both `herd send
// review-supervisor` and `herd send forge-review-supervisor-4922de28` answered
// only "no agent found". Nothing in that output said the fleet was reachable,
// that three other agents were live, or what to target instead -- so the
// refusal was indistinguishable from herdr itself being down, and a handoff
// stalled behind it.
//
// A refusal that names no dispatchable identity cannot be acted on. This is
// FAC-593 in a second place.
func TestNoAgentFoundNamesTheLiveAlternatives(t *testing.T) {
	live := []AgentEntry{
		{Name: "herdforge-coordinator", Workspace: "wK"},
		{Name: "forge-orchestrator-39a9827d2b", Workspace: "wK"},
		{Name: "forge-chain-indexer-2918de97b5", Workspace: "wB"},
	}
	err := noAgentFoundError("forge-review-supervisor-4922de28", live, "wK")
	if err == nil {
		t.Fatal("a missing agent was not refused")
	}
	msg := err.Error()
	for _, want := range []string{"forge-review-supervisor-4922de28", "herdforge-coordinator", "forge-orchestrator-39a9827d2b", "wK"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal does not name %q: %s", want, msg)
		}
	}
	// Another workspace's agent is not a dispatchable alternative here and
	// offering it would send this repo's handoff into another fleet.
	if strings.Contains(msg, "forge-chain-indexer-2918de97b5") {
		t.Fatalf("refusal offered an out-of-workspace agent as an alternative: %s", msg)
	}
}

// An empty fleet and a fleet that simply lacks this target are different
// faults: the first says raise anything, the second says raise this one.
func TestNoAgentFoundDistinguishesAnEmptyFleet(t *testing.T) {
	err := noAgentFoundError("review-supervisor", nil, "wK")
	if err == nil {
		t.Fatal("a missing agent was not refused")
	}
	if !strings.Contains(err.Error(), "no live agents") {
		t.Fatalf("an empty fleet reads the same as a missing target: %v", err)
	}
}
