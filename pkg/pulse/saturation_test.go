package pulse

import "testing"

// FAC-650: a concurrency bound must be compared against concurrency. Saturated
// was pending+RAW_VETOED >= cap, and RawVetoed is the ledger's HISTORICAL vetoed
// set -- a vetoed SHA stays vetoed forever. Measured live: pending=0,
// raw_vetoed=31, cap=3, reviews actually in flight=2. The fleet had capacity and
// dispatch_blocked=review-saturated was permanent.
func TestReviewSaturationIgnoresHistoricalVetoedSet(t *testing.T) {
	obs := Observation{Review: ReviewObservation{Known: true, Pending: 0, RawVetoed: 31, Cap: 3, Saturated: true}}
	agents := []AgentObservation{
		{Name: "r1", SafeRef: "sha-a"},
		{Name: "r2", AwaitingVerdict: true},
	}
	if reviewSaturated(obs, agents) {
		t.Fatalf("2 live reviews under a cap of 3 must not be saturated, whatever the vetoed history says")
	}
	if got := liveReviewsInFlight(agents); got != 2 {
		t.Errorf("in-flight must count pinned-out and awaiting-verdict lanes: got %d", got)
	}
}

// The bound is still real: reaching it blocks, and the reason names both numbers
// so the next operator does not have to guess which quantity was compared.
func TestReviewSaturationStillBlocksAtTheCap(t *testing.T) {
	obs := Observation{Review: ReviewObservation{Known: true, Cap: 2}}
	agents := []AgentObservation{{Name: "r1", SafeRef: "a"}, {Name: "r2", SafeRef: "b"}}
	if !reviewSaturated(obs, agents) {
		t.Fatal("live in-flight reviews at the cap must block")
	}
	reason := reviewSaturationReason(obs, agents)
	for _, want := range []string{"2 reviews in flight", "cap 2"} {
		if !contains(reason, want) {
			t.Errorf("reason %q must state %q", reason, want)
		}
	}
}

// An unconfigured cap is not a bound of zero, which would block every fleet.
func TestUnconfiguredCapIsNotAZeroBound(t *testing.T) {
	obs := Observation{Review: ReviewObservation{Known: true, Cap: 0}}
	if reviewSaturated(obs, nil) {
		t.Fatal("cap<=0 means unconfigured, not a bound of zero")
	}
	if reviewSaturated(obs, []AgentObservation{{Name: "r1", SafeRef: "a"}}) {
		t.Fatal("cap<=0 must not block a fleet with live reviews")
	}
}

func TestReviewSaturationDoesNotBlockReadyBuilder(t *testing.T) {
	obs := Observation{
		Provider: ProviderObservation{Known: true, Claimable: 1, NextTaskRef: "FAC-581", NextTaskID: "eow6"},
		Review:   ReviewObservation{Known: true, Cap: 2},
		Herdr: HerdrObservation{Known: true, Agents: []AgentObservation{
			{Name: "r1", SafeRef: "a"},
			{Name: "r2", SafeRef: "b"},
		}},
		Quota:    QuotaObservation{Known: true},
		WindDown: WindDownObservation{Known: true},
	}
	if !reviewSaturated(obs, obs.Herdr.Agents) {
		t.Fatal("fixture must be review-saturated")
	}
	snap, err := Plan(obs, Options{Act: true, Spawn: true})
	if err != nil {
		t.Fatal(err)
	}
	if snap.DispatchBlocked {
		t.Fatalf("ready builder FAC-581 must not be blocked by a full review slot: %s", snap.DispatchBlockReason)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
