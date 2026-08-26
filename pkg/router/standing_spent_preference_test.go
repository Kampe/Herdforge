package router

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/usage"
)

// FAC-684. Reported live: "standing launch admitted a spent Grok preference and
// started forge-herd-smith into 0% weekly quota even though dry-run said
// Claude/default was available. After cooling Grok, standing refused no healthy
// candidate."
//
// Both halves are the same defect. Standing admission ADMITS a spent preference
// on the stated grounds that the router will reroute (FAC-642), but the router
// promoted that preference to a hard provider boundary, so it could not.

func TestStandingBoundaryReleasesWhenTheProviderIsProvenSpent(t *testing.T) {
	r := &SurfaceRouter{Computed: map[string]usage.BurnState{
		"grok":   {Available: false, Reason: "exhausted", Used: 100, Remaining: 0},
		"claude": {Available: true, Used: 20, Remaining: 80},
	}}
	if !r.standingProviderSpent("grok", "grok-4.6") {
		t.Fatal("an exhausted provider still held the standing family boundary; the lane launches into 0%")
	}
}

func TestStandingBoundaryHoldsForAHealthyProvider(t *testing.T) {
	r := &SurfaceRouter{Computed: map[string]usage.BurnState{
		"grok": {Available: true, Used: 30, Remaining: 70},
	}}
	if r.standingProviderSpent("grok", "grok-4.6") {
		t.Fatal("a healthy standing lane lost its family boundary and may now wander providers")
	}
}

func TestStandingBoundaryHoldsWhenQuotaIsUnknown(t *testing.T) {
	// The safety property. An unmeasured pool is not an exhausted one; if
	// unknown released the boundary, one failed snapshot would silently
	// dissolve every standing lane's provider pin at once.
	r := &SurfaceRouter{Computed: map[string]usage.BurnState{}}
	if r.standingProviderSpent("grok", "grok-4.6") {
		t.Fatal("unknown quota was treated as proven exhaustion")
	}
}

func TestStandingBoundaryHoldsWhenASiblingPoolOnTheSameProviderIsHealthy(t *testing.T) {
	// A sibling pool is a WITHIN-family fallback, which the standing boundary
	// already permits. Releasing the boundary there would send the lane across
	// providers when its own provider could still take it.
	r := &SurfaceRouter{Computed: map[string]usage.BurnState{
		"claude": {
			Available: false, Reason: "exhausted", Used: 100,
			Pools: map[string]usage.BurnState{
				"default": {Available: false, Reason: "exhausted", Used: 100},
				"fable":   {Available: true, Used: 10, Remaining: 90},
			},
		},
	}}
	if r.standingProviderSpent("claude", "claude-sonnet-5") {
		t.Fatal("boundary released while a sibling pool on the same provider had capacity")
	}
}
