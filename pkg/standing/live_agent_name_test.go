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
	got := LiveAgentName([]Agent{{Name: legacy, Status: "working"}}, lane, repo)
	if got != legacy {
		t.Fatalf("legacy live supervisor must be adopted: got %q want %q", got, legacy)
	}

	// Qualified present -> prefer it even when a legacy agent also exists.
	got = LiveAgentName([]Agent{{Name: legacy}, {Name: qualified}}, lane, repo)
	if got != qualified {
		t.Fatalf("qualified agent must win: got %q want %q", got, qualified)
	}

	// Neither live -> mint the current qualified identity.
	if got = LiveAgentName(nil, lane, repo); got != qualified {
		t.Fatalf("absent fleet must mint qualified: got %q want %q", got, qualified)
	}
}
