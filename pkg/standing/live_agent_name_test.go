package standing

import "testing"

// FAC-547: pulse targeted the repo-qualified supervisor name on a fleet whose
// supervisor predated qualification, so five open_review actions in a row went
// to "forge-review-harvest-su-<digest>" — a name no agent held — and dispatch
// stayed review-saturated.
func TestLiveAgentNameAdoptsLegacySupervisor(t *testing.T) {
	const lane, repo = "review-harvest-supervisor", "github.com/Kampe/Chainseer"
	legacy := AgentName(lane)
	qualified := AgentNameForRepository(lane, repo)
	if legacy == qualified {
		t.Fatalf("fixture invalid: qualified name must differ from legacy (%q)", legacy)
	}

	// Legacy agent live, qualified absent -> adopt the legacy name.
	got, live := LiveAgentName([]Agent{{Name: legacy, Status: "working"}}, lane, repo)
	if !live {
		t.Fatal("a live legacy supervisor was reported as not live")
	}
	if got != legacy {
		t.Fatalf("legacy live supervisor must be adopted: got %q want %q", got, legacy)
	}

	// Qualified present -> prefer it even when a legacy agent also exists.
	got, live = LiveAgentName([]Agent{{Name: legacy}, {Name: qualified}}, lane, repo)
	if !live {
		t.Fatal("a live qualified supervisor was reported as not live")
	}
	if got != qualified {
		t.Fatalf("qualified agent must win: got %q want %q", got, qualified)
	}

	// Neither live -> mint the current qualified identity.
	// Neither live -> the NAME is still the qualified identity, so a caller
	// minting a new agent keeps current naming. FAC-597 adds the second return:
	// a caller that TARGETS an agent must be able to tell that nothing holds it.
	got, live = LiveAgentName(nil, lane, repo)
	if got != qualified {
		t.Fatalf("absent fleet must mint qualified: got %q want %q", got, qualified)
	}
	if live {
		t.Fatalf("absent fleet reported %q as live", got)
	}
}

// FAC-597: LiveAgentName's whole stated purpose (FAC-547) is to avoid targeting
// "a qualified name no agent holds" -- and with an EMPTY census it returned
// exactly that. haveQualified=false and haveLegacy=false takes the
// `!haveLegacy` branch, so a reaped supervisor resolved to a synthesized
// forge-<lane>-<digest> name that nothing answered to.
//
// Both callers use the result as a DELIVERY TARGET: pulse sends the review
// handoff to it, and drain embeds it in the review packet as the address the
// reviewer must return its verdict to. So a phantom here does not fail loudly;
// it burns a scarce review slot on a reviewer that has been told to report to
// nobody.
//
// Measured: this is how a review supervisor that had been reaped kept being
// addressed, and a NEEDS_REVIEW handoff stalled behind it.
func TestLiveAgentNameReportsWhetherTheTargetIsActuallyLive(t *testing.T) {
	qualified := AgentNameForRepository("review-supervisor", "repo")

	name, live := LiveAgentName([]Agent{{Name: qualified}}, "review-supervisor", "repo")
	if !live || name != qualified {
		t.Fatalf("a live qualified agent was not resolved: name=%q live=%v", name, live)
	}

	legacy := AgentName("review-supervisor")
	name, live = LiveAgentName([]Agent{{Name: legacy}}, "review-supervisor", "repo")
	if !live || name != legacy {
		t.Fatalf("a live legacy-named agent was not adopted (FAC-547): name=%q live=%v", name, live)
	}

	// The regression: nothing live, yet a confident synthesized target.
	name, live = LiveAgentName(nil, "review-supervisor", "repo")
	if live {
		t.Fatalf("an empty census reported a live target %q", name)
	}

	// A census that holds other agents but not this lane is equally not-live.
	name, live = LiveAgentName([]Agent{{Name: "forge-orchestrator-39a9827d2b"}}, "review-supervisor", "repo")
	if live {
		t.Fatalf("a census without this lane reported a live target %q", name)
	}
}
