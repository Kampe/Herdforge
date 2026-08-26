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

// FAC-666: FAC-650 fixed how saturation is MEASURED but it stayed a cause of the
// GLOBAL DispatchBlocked, so a full review queue still stopped builders. That is
// the leak pkg/broker was built to close, still live in the surface that
// actually decides: measured on this fleet, dispatch blocked with 6 healthy-idle
// lanes while 2 reviews ran against a cap of 3.
func TestReviewSaturationDoesNotBlockBuilderDispatch(t *testing.T) {
	obs := Observation{Review: ReviewObservation{Known: true, Cap: 2}}
	agents := []AgentObservation{{Name: "r1", SafeRef: "a"}, {Name: "r2", SafeRef: "b"}}
	reason := reviewSaturationReason(obs, agents)

	if !reviewSaturationOnly(reason, obs, agents) {
		t.Fatal("the saturation reason must be recognised as review-only")
	}
	// Any OTHER block reason must still stop builders: each of those genuinely
	// applies to a builder too.
	for _, other := range []string{
		"wind-down enabled",
		"quota exhausted",
		"coordinator broker unavailable",
		"no claimable work",
		"unknown critical source: provider",
	} {
		if reviewSaturationOnly(other, obs, agents) {
			t.Errorf("%q must still block builders; only review capacity is exempt", other)
		}
	}
	// An empty reason is not a block at all.
	if reviewSaturationOnly("", obs, agents) {
		t.Error("no block reason is not a review-only block")
	}
}

// The exemption must be exact, so a future reason cannot inherit it by merely
// resembling this one.
func TestTheBuilderExemptionIsExactNotFuzzy(t *testing.T) {
	obs := Observation{Review: ReviewObservation{Known: true, Cap: 3}}
	agents := []AgentObservation{{Name: "r1", SafeRef: "a"}}
	for _, near := range []string{
		"review saturated",
		"review saturated: something else",
		"review capacity is full",
	} {
		if reviewSaturationOnly(near, obs, agents) {
			t.Errorf("%q must not inherit the builder exemption by resemblance", near)
		}
	}
}
