package router

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/usage"
)

// FAC-627: quotaState excluded the pool literally named "default" from its
// per-pool lookup and fell back to the PROVIDER AGGREGATE.
//
// Codex meters its default pool and its Spark pool separately. Live evidence:
// default 13% used / 87% remaining, Spark exhausted. The aggregate was
// exhausted solely because Spark was, so every codex/default candidate was
// rejected as "quota exhausted" while 87% of the pool it would actually bill
// remained -- and the whole fleet reported no healthy launch candidate.
//
// It is also the isolated cause of the herdr-route-doctor vs herd-standing
// disagreement: the shell router reads per-pool and selects codex/default,
// the Go router aggregated and refused. Two authorities on one dataset, only
// the aggregating one gating launches.

// codexSparkExhausted reproduces the live shape: a healthy default pool inside
// a provider whose aggregate is exhausted because a SIBLING pool is spent.
func codexSparkExhausted() map[string]usage.BurnState {
	return map[string]usage.BurnState{
		"codex": {
			Available: false, Reason: "weekly-exhausted",
			Pools: map[string]usage.BurnState{
				"default": {Available: true, Reason: "ok"},
				"spark":   {Available: false, Reason: "weekly-exhausted"},
			},
		},
	}
}

// THE regression. A healthy default pool must be reported healthy even when the
// provider aggregate is exhausted by a sibling.
func TestAHealthyDefaultPoolIsNotJudgedByTheProviderAggregate(t *testing.T) {
	r := &SurfaceRouter{Computed: codexSparkExhausted()}

	st, ok := r.quotaState("codex", "default")
	if !ok {
		t.Fatal("no quota state for codex/default")
	}
	if !st.Available {
		t.Fatalf("codex/default reported unavailable (%q) while the POOL is healthy; "+
			"the provider aggregate is exhausted only because the separately metered "+
			"spark pool is spent, and billing the default pool is unaffected by that",
			st.Reason)
	}
}

// The exhausted sibling must still be reported exhausted -- the fix must not
// make every pool inherit the healthiest one.
func TestAnExhaustedSiblingPoolIsStillExhausted(t *testing.T) {
	r := &SurfaceRouter{Computed: codexSparkExhausted()}

	st, ok := r.quotaState("codex", "spark")
	if !ok {
		t.Fatal("no quota state for codex/spark")
	}
	if st.Available {
		t.Fatal("an exhausted pool was reported available; a spent pool must stay spent")
	}
}

// A pool the provider does not meter separately has no per-pool answer, so the
// aggregate is the only honest one. That fallback must survive.
func TestAnUnmeteredPoolStillFallsBackToTheAggregate(t *testing.T) {
	r := &SurfaceRouter{Computed: codexSparkExhausted()}

	st, ok := r.quotaState("codex", "not-a-metered-pool")
	if !ok {
		t.Fatal("no quota state for an unmetered pool")
	}
	if st.Available {
		t.Fatal("an unmetered pool ignored an exhausted provider aggregate; " +
			"with no per-pool evidence the aggregate is the only honest answer")
	}
}

// An empty pool name means "no pool scope given" and must read the aggregate.
func TestNoPoolScopeReadsTheAggregate(t *testing.T) {
	r := &SurfaceRouter{Computed: codexSparkExhausted()}

	st, ok := r.quotaState("codex", "")
	if !ok {
		t.Fatal("no quota state for codex")
	}
	if st.Available {
		t.Fatal("an unscoped lookup ignored the exhausted aggregate")
	}
}

// The end-to-end consequence: with claude, grok and agy exhausted and codex's
// default pool healthy, a review-shaped pick must land on codex rather than
// refusing with "no healthy launch candidate".
func TestReviewPickSelectsCodexWhenOnlyItsDefaultPoolIsHealthy(t *testing.T) {
	clearRouteEnv(t)

	computed := map[string]usage.BurnState{
		"claude": {Available: false, Reason: "weekly-exhausted"},
		"grok":   {Available: false, Reason: "weekly-exhausted"},
		"agy":    {Available: false, Reason: "weekly-exhausted"},
		"codex": {
			Available: false, Reason: "weekly-exhausted",
			Pools: map[string]usage.BurnState{
				"default": {Available: true, Reason: "ok"},
				"spark":   {Available: false, Reason: "weekly-exhausted"},
			},
		},
	}
	r := testRouter(computed, "codex", "claude", "grok")

	d, err := r.Decide(LaunchRequest{
		Role: RoleWorker, NativeRole: RoleWorker, Shape: "implementation",
		Scope: ScopeLane, TaskRef: "FAC-627",
		ProbeResults: map[string]bool{
			ProbeKey("codex", ModelFor("codex", "implementation")): true,
		},
	})
	if err != nil {
		t.Fatalf("refused every surface while codex's default pool was healthy: %v\n"+
			"This is the live outage: the fleet reported no healthy launch candidate "+
			"with 87%% of the codex default pool remaining.", err)
	}
	if !strings.EqualFold(d.Provider, "codex") {
		t.Fatalf("routed to %s/%s; want codex, the only surface with a healthy pool", d.Provider, d.Model)
	}
}
