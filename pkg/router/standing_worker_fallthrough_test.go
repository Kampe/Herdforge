package router

import (
	"strings"
	"testing"
)

// FAC-615, second attempt. The first version fixed standingProviderSpent and
// was UNREACHABLE for every lane in the incident.
//
// launchStandingLane sets RequestedProvider (not PreferredProvider) for
// pinnedBuilder roles -- worker, forge-smith, recovery. All five broken lanes
// are workers: defi-crusader, platform-ops, api-crusader, chain-indexer,
// nft-data-engineer. With PreferredProvider empty, the standing block never
// ran and standingProviderSpent was never called.
//
// The first version's tests drove standingProviderSpent DIRECTLY, so they
// passed while the shipped decision path skipped it entirely. That is the same
// vacuous shape that has now produced four independent FAILs in this
// repository in one day.
//
// These drive Decide with the LIVE REQUEST SHAPE a standing worker lane
// actually produces. If the fallthrough is removed, they go red.

func standingWorkerRequest(provider, model string) LaunchRequest {
	// Exactly what launchStandingLane builds for a pinnedBuilder standing lane:
	// RequestedProvider set, PreferredProvider EMPTY, Standing true.
	return LaunchRequest{
		Role:              RoleWorker,
		NativeRole:        RoleWorker,
		Shape:             "implementation",
		Standing:          true,
		RequestedProvider: provider,
		RequestedModel:    model,
		RequestedEffort:   "medium",
		TaskRef:           "chain-indexer",
		Scope:             ScopeLane,
		// Probe-gated models fail closed without a recorded result. Supply
		// passes for the surfaces under test so the fixture exercises the
		// ROUTING decision rather than the probe gate.
		ProbeResults: map[string]bool{
			ProbeKey("codex", "gpt-5.6-luna"):                        true,
			ProbeKey("grok", ModelFor("grok", "implementation")):     true,
			ProbeKey("claude", ModelFor("claude", "implementation")): true,
		},
	}
}

// THE regression. A standing worker whose requested provider cannot take work
// must reach a healthy alternate instead of refusing.
func TestStandingWorkerFallsThroughAnUnavailableRequestedProvider(t *testing.T) {
	r := newStandingTestRouter(t, quotaHealthy, liveAtCap) // codex at concurrency cap

	d, err := r.Decide(standingWorkerRequest("codex", "gpt-5.6-luna"))
	if err != nil {
		t.Fatalf("standing worker refused while its requested provider was merely at concurrency cap: %v\n"+
			"this is the live failure: five worker lanes died beside a healthy surface that direct herdr launch used successfully", err)
	}
	if strings.EqualFold(d.Provider, "codex") {
		t.Fatalf("routed back onto codex (%s) despite it being unable to take work", d.Provider)
	}
}

// A standing worker whose provider IS healthy must still get it. The
// fallthrough must not become a wander.
func TestStandingWorkerKeepsAHealthyRequestedProvider(t *testing.T) {
	r := newStandingTestRouter(t, quotaHealthy, liveBelowCap)

	d, err := r.Decide(standingWorkerRequest("codex", "gpt-5.6-luna"))
	if err != nil {
		t.Fatalf("healthy standing worker was refused: %v", err)
	}
	if !strings.EqualFold(d.Provider, "codex") {
		t.Fatalf("standing worker abandoned a HEALTHY requested provider, routed to %s instead", d.Provider)
	}
}

// UNKNOWN must not trigger the fallthrough. Unreadable concurrency is not
// evidence of exhaustion, and wandering on ignorance is worse than refusing.
func TestUnknownConcurrencyDoesNotMoveAStandingWorker(t *testing.T) {
	r := newStandingTestRouter(t, quotaHealthy, liveUnknown)

	d, err := r.Decide(standingWorkerRequest("codex", "gpt-5.6-luna"))
	if err != nil {
		return // refusing on unknown is acceptable; wandering is not
	}
	if !strings.EqualFold(d.Provider, "codex") {
		t.Fatalf("unreadable concurrency moved a standing worker off codex to %s; unknown is not exhaustion", d.Provider)
	}
}

// A NON-standing pinned builder is a real operator override and must stay
// pinned. The fallthrough is scoped to standing lanes only.
func TestANonStandingPinnedBuilderKeepsItsPin(t *testing.T) {
	r := newStandingTestRouter(t, quotaHealthy, liveAtCap)

	req := standingWorkerRequest("codex", "gpt-5.6-luna")
	req.Standing = false

	d, err := r.Decide(req)
	if err == nil && !strings.EqualFold(d.Provider, "codex") {
		t.Fatalf("a non-standing pinned builder was rerouted to %s; that pin is an operator override and must be honoured or refused, never silently moved", d.Provider)
	}
}
