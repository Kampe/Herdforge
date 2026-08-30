package router

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/classify"
	"github.com/Kampe/Herdforge/pkg/usage"
)

func TestCapabilityOfTable(t *testing.T) {
	cases := []struct {
		model string
		want  CapabilityTier
	}{
		{"", CapUnknown},
		{"opencode/deepseek-v4-flash", CapFlash},
		{"claude-haiku-4-5", CapFlash},
		{"gpt-5.3-codex-spark", CapFlash},
		{"claude-sonnet-5", CapStandard},
		{"gpt-5.6-luna", CapStandard},
		{"grok-4.5", CapStandard},
		{"claude-opus-5", CapFrontier},
		{"claude-fable-5", CapFrontier},
		{"gpt-5.6-sol", CapFrontier},
		{"gpt-5.6-terra", CapFrontier},
		{"totally-unknown-xyz", CapUnknown},
	}
	for _, c := range cases {
		if got := CapabilityOf(c.model); got != c.want {
			t.Errorf("CapabilityOf(%q) = %q, want %q", c.model, got, c.want)
		}
	}
}

func TestForbiddenDeepSeek(t *testing.T) {
	if ForbiddenDeepSeek("opencode/deepseek-v4-flash") {
		t.Fatal("v4 flash must be allowed")
	}
	if ForbiddenDeepSeek("opencode/deepseek-v4-pro") {
		t.Fatal("v4 pro must be allowed")
	}
	if !ForbiddenDeepSeek("deepseek-v3-chat") {
		t.Fatal("non-v4 deepseek must be forbidden")
	}
	if !ForbiddenDeepSeek("opencode/deepseek-chat") {
		t.Fatal("unversioned deepseek must be forbidden")
	}
	if ForbiddenDeepSeek("claude-sonnet-5") {
		t.Fatal("non-deepseek must not trip the gate")
	}
}

func TestModelRequiresProbeLunaAndDeepseek(t *testing.T) {
	if !ModelRequiresProbe("gpt-5.6-luna") {
		t.Fatal("luna must require tool-probe")
	}
	if !ModelRequiresProbe("opencode/deepseek-v4-flash") {
		t.Fatal("deepseek must require tool-probe")
	}
	if ModelRequiresProbe("claude-sonnet-5") {
		t.Fatal("sonnet must not require probe by default")
	}
}

// The whole point of unpinning builders: when the preferred surface is out of
// quota, a worker launch reroutes instead of failing. A vendor tuple gate made
// this impossible and stranded the fleet whenever one pool filled.
func TestWorkerDecideReroutesOnQuotaExhaustion(t *testing.T) {
	clearRouteEnv(t)
	computed := map[string]usage.BurnState{
		"claude": {Available: false, Reason: "exhausted", Pressure: 100},
		"codex":  {Available: false, Reason: "exhausted", Pressure: 100},
		"grok":   {Available: true, Pressure: 10},
	}
	r := testRouter(computed, "claude", "codex", "grok")
	// No RequestedProvider: a lane's configured provider is a soft preference,
	// so the CLI must not pass it through the hard --provider channel (which
	// narrows candidates to exactly one surface, chainseer herd-route parity).
	d, err := r.Decide(LaunchRequest{Role: RoleWorker, Shape: "implementation"})
	if err != nil {
		t.Fatalf("worker must reroute when the preferred surface is exhausted: %v", err)
	}
	if d.Provider != "grok" {
		t.Fatalf("worker routed to %s/%s, want the healthy grok surface", d.Provider, d.Model)
	}
	if d.Shape != "implementation" {
		t.Fatalf("rerouted worker lost its shape: %s", d.Shape)
	}
}

func TestStandingRouteHonorsProviderAndModelWithoutCrossFamilyFallback(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("HERD_USE_PI", "0")
	r := testRouter(map[string]usage.BurnState{
		"claude": {Available: true, Pressure: 1},
		"grok":   {Available: true, Pressure: 100},
	}, "claude", "grok")
	d, err := r.Decide(LaunchRequest{
		Role: RoleScoutPlanner, Shape: "architecture", Scope: ScopeLane,
		TaskRef: "scout-planner", Standing: true,
		PreferredProvider: "grok", PreferredModel: "grok-4.6",
	})
	if err != nil {
		t.Fatalf("standing route rejected: %v", err)
	}
	if d.Provider != "grok" || d.Model != "grok-4.6" || d.Harness != "grok" {
		t.Fatalf("standing route crossed or changed its configured tuple: %+v", d)
	}
}

func TestStandingRouteUsesSameProviderFallbackModel(t *testing.T) {
	clearRouteEnv(t)
	t.Setenv("HERD_USE_PI", "0")
	stateDir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", stateDir)
	if err := os.WriteFile(filepath.Join(stateDir, "grok--model--grok-4.6.cooldown.json"), []byte(`{"expiresAt":4102444800,"provider":"grok","model":"grok-4.6","reason":"exhausted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := testRouter(map[string]usage.BurnState{
		"grok": {Available: true, Pressure: 10, Pools: map[string]usage.BurnState{
			"default": {Available: true, Pressure: 10},
		}},
	}, "grok")
	d, err := r.Decide(LaunchRequest{
		Role: RoleScoutPlanner, Shape: "architecture", Scope: ScopeLane,
		TaskRef: "scout-planner", Standing: true,
		PreferredProvider: "grok", PreferredModel: "grok-4.6",
		PreferredFallbackModels: []string{"grok-4.5"},
	})
	if err != nil {
		t.Fatalf("standing fallback rejected: %v", err)
	}
	if d.Provider != "grok" || d.Model != "grok-4.5" {
		t.Fatalf("standing fallback crossed provider or did not select sibling model: %+v", d)
	}
}

// Builder roles are bound to the implementation shape but NOT to a vendor.
// Anything else is the quota-ranked waterfall's business.
func TestWorkerRoleIsBoundToImplementationShapeOnly(t *testing.T) {
	clearRouteEnv(t)
	bad := LaunchRequest{Role: RoleWorker, Shape: "bounded", RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium", ProbeResults: map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): true}}
	if _, err := testRouter(nil, "codex").Decide(bad); !errors.Is(err, ErrWorkerPolicy) {
		t.Fatalf("worker on a non-implementation shape must be rejected, got %v", err)
	}
	good := LaunchRequest{Role: RoleWorker, Shape: "implementation", RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium", ProbeResults: map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): true}}
	if _, err := testRouter(nil, "codex").Decide(good); err != nil {
		t.Fatalf("implementation-shape worker rejected: %v", err)
	}
}

// A probe-required model only routes when the probe for that EXACT
// (provider, model) passed. A probe for a sibling model must not carry it.
func TestProbeRequiredModelNeedsMatchingProbe(t *testing.T) {
	clearRouteEnv(t)
	if !ModelRequiresProbe("gpt-5.6-luna") {
		t.Fatal("fixture assumes luna requires a probe")
	}
	mismatched := LaunchRequest{Role: RoleWorker, Shape: "implementation", RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium",
		ProbeResults: map[string]bool{ProbeKey("codex", "gpt-5.3-codex-spark"): true}}
	if _, err := testRouter(nil, "codex").Decide(mismatched); err == nil {
		t.Fatal("probe for a different model must not satisfy the gate")
	}
	failed := LaunchRequest{Role: RoleWorker, Shape: "implementation", RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium",
		ProbeResults: map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): false}}
	if _, err := testRouter(nil, "codex").Decide(failed); err == nil {
		t.Fatal("failed probe must fail closed")
	}
}

func TestWorkerEffortUsesShapeUnlessExplicitlyRequested(t *testing.T) {
	base := LaunchRequest{Role: RoleWorker, Shape: "implementation", RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna"}
	if got := EffortForRequest(base); got != EffortFor("implementation") {
		t.Fatalf("worker effort = %q, want shape ladder %q", got, EffortFor("implementation"))
	}
	base.RequestedEffort = "medium"
	base.ProbeResults = map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): true}
	if got := EffortForRequest(base); got != "medium" {
		t.Fatalf("hard requested effort = %q, want medium", got)
	}
	d, err := testRouter(nil, "codex").Decide(base)
	if err != nil {
		t.Fatalf("explicit medium rejected: %v", err)
	}
	if d.Effort != "medium" {
		t.Fatalf("decision effort = %q, want medium", d.Effort)
	}
}

func TestUnknownRoleRejectedBeforeSignedDecision(t *testing.T) {
	if _, err := testRouter(nil, "codex").Decide(LaunchRequest{Role: Role("not-configured"), Shape: "implementation"}); !errors.Is(err, ErrRolePolicy) {
		t.Fatalf("unknown role error = %v", err)
	}
}

func TestCustomRoleUsesNativeRolePolicy(t *testing.T) {
	d, err := testRouter(nil, "codex").Decide(LaunchRequest{
		Role:              Role("docs-custodian"),
		NativeRole:        RoleWorker,
		Shape:             "implementation",
		RequestedProvider: "codex",
		RequestedModel:    "gpt-5.6-luna",
		RequestedEffort:   "medium",
		ProbeResults:      map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): true},
	})
	if err != nil {
		t.Fatalf("custom role with native worker policy rejected: %v", err)
	}
	if d.Role != RoleWorker {
		t.Fatalf("decision role = %q, want %q", d.Role, RoleWorker)
	}
}

func TestCustomRoleRejectsUnknownNativeRole(t *testing.T) {
	_, err := testRouter(nil, "codex").Decide(LaunchRequest{
		Role:       Role("docs-custodian"),
		NativeRole: Role("not-configured"),
		Shape:      "implementation",
	})
	if !errors.Is(err, ErrRolePolicy) {
		t.Fatalf("unknown native role error = %v, want %v", err, ErrRolePolicy)
	}
}

func TestEffortLadderReviewer(t *testing.T) {
	clearRouteEnv(t)
	cases := []struct {
		name string
		req  LaunchRequest
		want string
	}{
		{
			name: "normal review medium",
			req:  LaunchRequest{Role: RoleReviewer, Risk: classify.TierR1},
			want: "medium",
		},
		{
			name: "small delta stays medium",
			req: LaunchRequest{
				Role: RoleReviewer, Risk: classify.TierR2,
				SmallDelta: true, RiskChanged: false,
			},
			want: "medium",
		},
		{
			name: "final pass R2 high",
			req: LaunchRequest{
				Role: RoleReviewer, Risk: classify.TierR2, FinalPass: true,
			},
			want: "high",
		},
		{
			name: "critical R3 high",
			req: LaunchRequest{
				Role: RoleReviewer, Risk: classify.TierR3, Critical: true,
			},
			want: "high",
		},
		{
			name: "final pass R0 stays medium",
			req: LaunchRequest{
				Role: RoleReviewer, Risk: classify.TierR0, FinalPass: true,
			},
			want: "medium",
		},
		{
			name: "small delta with risk change into critical final → high",
			req: LaunchRequest{
				Role: RoleReviewer, Risk: classify.TierR3,
				SmallDelta: true, RiskChanged: true, FinalPass: true,
			},
			want: "high",
		},
		{
			name: "worker uses shape ladder",
			req:  LaunchRequest{Role: RoleWorker, Shape: "implementation"},
			want: "high", // EffortFor("implementation"); was hard-coded "medium"
		},
		{
			name: "worker bounded low",
			req:  LaunchRequest{Role: RoleWorker, Shape: "bounded"},
			want: "low",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffortForRequest(c.req); got != c.want {
				t.Fatalf("EffortForRequest = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFlashFrontierHighForbidden(t *testing.T) {
	if !FlashFrontierHighForbidden(CapFlash, CapFrontier, "high") {
		t.Fatal("flash+frontier+high must be forbidden")
	}
	if FlashFrontierHighForbidden(CapFlash, CapFrontier, "medium") {
		t.Fatal("flash+frontier+medium is coherent enough to allow")
	}
	if FlashFrontierHighForbidden(CapStandard, CapFrontier, "high") {
		t.Fatal("standard author + frontier high is allowed")
	}
	if FlashFrontierHighForbidden(CapFlash, CapStandard, "high") {
		t.Fatal("flash author + standard high is allowed")
	}
}

func TestCoordinatorOperationShapeTable(t *testing.T) {
	cases := []struct {
		operation CoordinatorOperation
		shape     string
	}{
		{CoordinatorOperationDispatch, "implementation"},
		{CoordinatorOperationReview, "qa"},
		{CoordinatorOperationVerification, "bounded"},
		{CoordinatorOperationRecovery, "bounded"},
		{CoordinatorOperationPulseRead, "qa-light"},
		{CoordinatorOperationRoutingCheck, "qa-light"},
	}
	for _, tc := range cases {
		t.Run(string(tc.operation), func(t *testing.T) {
			shape, ok := CoordinatorOperationShape(tc.operation)
			if !ok {
				t.Fatalf("operation %q has no explicit shape", tc.operation)
			}
			if shape != tc.shape {
				t.Fatalf("operation %q shape = %q, want %q", tc.operation, shape, tc.shape)
			}
			if _, err := Waterfall(shape); err != nil {
				t.Fatalf("operation %q shape %q is not routable: %v", tc.operation, shape, err)
			}
		})
	}
	if _, ok := CoordinatorOperationShape(CoordinatorOperation("unknown")); ok {
		t.Fatal("unknown coordinator operation must not inherit a shape")
	}
}

func TestDecideWorkerPicksModelAndEffort(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil, "claude", "grok", "codex", "opencode", "agy", "kimi")
	d, err := r.Decide(LaunchRequest{
		Role:              RoleWorker,
		Shape:             "implementation",
		Risk:              classify.TierR2,
		RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium",
		ProbeResults: map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): true},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Provider == "" || d.Model == "" || d.Effort == "" || d.Family == "" {
		t.Fatalf("LaunchDecision incomplete: %+v", d)
	}
	if d.CapabilityTier == CapUnknown {
		t.Fatal("capability must be known")
	}
	if d.Pool == "" {
		t.Fatal("pool required")
	}
	if d.Rationale == "" {
		t.Fatal("rationale required")
	}
	if d.Role != RoleWorker {
		t.Fatalf("role = %s", d.Role)
	}
	// An explicit hard effort request is authoritative.
	if d.Effort != "medium" {
		t.Fatalf("worker effort = %q, want medium", d.Effort)
	}
	if len(d.Argv) == 0 {
		t.Fatal("argv must be populated from decision")
	}
}

func TestDecideReviewerFamilyDisjoint(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil, "claude", "grok", "codex", "opencode", "agy", "kimi")
	d, err := r.Decide(LaunchRequest{
		Role:         RoleReviewer,
		Shape:        "qa",
		Risk:         classify.TierR1,
		AuthorFamily: "anthropic",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Family == "anthropic" {
		t.Fatalf("reviewer must not share author family, got %+v", d)
	}
	if d.Effort != "medium" {
		t.Fatalf("normal review effort = %q, want medium", d.Effort)
	}
	if !strings.Contains(d.Rationale, "author_family=anthropic") {
		t.Fatalf("rationale must record author family: %s", d.Rationale)
	}
}

func TestDecideReviewerMissingAuthorFamilyFailsClosed(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil, "claude", "grok")
	_, err := r.Decide(LaunchRequest{Role: RoleReviewer, Shape: "qa", Risk: classify.TierR1})
	if err == nil {
		t.Fatal("missing author_family must fail closed")
	}
}

func TestDecideFlashAuthorFrontierHighRejected(t *testing.T) {
	clearRouteEnv(t)
	// Force only frontier reviewers with high effort and a flash author — must
	// fail closed when no coherent alternative exists.
	//
	// FAC-595 moved claude:qa to sonnet-5, so `qa` no longer reaches a frontier
	// reviewer and can no longer construct this scenario. The invariant under
	// test is the capability-coherence rule, not the qa mapping, so the shape is
	// now `adversarial`, which still resolves to opus-5 deliberately.
	r := testRouter(nil, "claude")
	_, err := r.Decide(LaunchRequest{
		Role:              RoleReviewer,
		Shape:             "adversarial",
		Risk:              classify.TierR3,
		FinalPass:         true, // effort high
		AuthorFamily:      "deepseek",
		AuthorModel:       "opencode/deepseek-v4-flash",
		AuthorCapability:  CapFlash,
		RequestedProvider: "claude",
	})
	if err == nil {
		t.Fatal("flash author + frontier-high reviewer must be rejected")
	}
	if !strings.Contains(err.Error(), "no healthy") && !strings.Contains(err.Error(), "frontier") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDecideFlashAuthorFrontierMediumAllowed(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil, "claude", "grok")
	d, err := r.Decide(LaunchRequest{
		Role:         RoleReviewer,
		Shape:        "qa",
		Risk:         classify.TierR1, // medium effort
		AuthorFamily: "deepseek",
		AuthorModel:  "opencode/deepseek-v4-flash",
	})
	if err != nil {
		t.Fatalf("medium effort frontier reviewer should be allowed: %v", err)
	}
	if d.Effort != "medium" {
		t.Fatalf("effort = %s", d.Effort)
	}
}

func TestDecideQuotaHeadroomPick(t *testing.T) {
	clearRouteEnv(t)
	// claude exhausted → grok wins (headroom).
	computed := map[string]usage.BurnState{
		"claude": {Available: false, Reason: "exhausted", Pressure: 100},
		"grok":   {Available: true, Pressure: 12, Window: "5h", Used: 12},
	}
	r := testRouter(computed, "claude", "grok", "codex", "opencode", "agy", "kimi")
	d, err := r.Decide(LaunchRequest{
		Role:         RoleReviewer,
		Shape:        "implementation",
		Risk:         classify.TierR2,
		AuthorFamily: "zhipu",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Provider != "grok" {
		t.Fatalf("quota headroom must prefer grok over exhausted claude, got %s", d.Provider)
	}
}

func TestDecideWeeklyCapSkip(t *testing.T) {
	clearRouteEnv(t)
	// claude binding weekly at/over cap → skip; grok healthy wins.
	computed := map[string]usage.BurnState{
		"claude": {
			Available: true, Pressure: 10, Window: "weekly",
			WindowSeconds: usage.WindowWeekly, Used: 96, Class: usage.BurnExhausted,
		},
		"grok": {Available: true, Pressure: 20, Window: "5h", Used: 20},
	}
	r := testRouter(computed, "claude", "grok", "codex", "opencode", "agy", "kimi")
	d, err := r.Decide(LaunchRequest{
		Role:         RoleReviewer,
		Shape:        "implementation",
		Risk:         classify.TierR1,
		AuthorFamily: "zhipu",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Provider == "claude" {
		t.Fatal("weekly-capped surface must lose to healthy alternative")
	}
	if d.Provider != "grok" {
		t.Fatalf("want grok after weekly-cap skip, got %s", d.Provider)
	}
}

func TestPickWeeklyCapSkip(t *testing.T) {
	clearRouteEnv(t)
	computed := map[string]usage.BurnState{
		"claude": {
			Available: true, Pressure: 5, Window: "weekly",
			WindowSeconds: usage.WindowWeekly, Used: 97, Class: usage.BurnExhausted,
		},
		"grok": {Available: true, Pressure: 30},
	}
	r := testRouter(computed, "claude", "grok", "codex", "opencode", "agy", "kimi")
	route, err := r.Pick("implementation", "", "")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if route.Provider == "claude" {
		t.Fatal("Pick must also skip weekly-capped surfaces")
	}
}

// Sole weekly-capped candidate is the mutation-critical case for the hard skip:
// effectivePressure alone would still select that provider (pressure=200, only
// pick). weeklyAtOrOverCap continue is what empties the candidate set and fails
// closed. Removing the hard skip makes these tests fail.
func TestDecideWeeklyCapSoleCandidateFailsClosed(t *testing.T) {
	clearRouteEnv(t)
	computed := map[string]usage.BurnState{
		"claude": {
			Available:     true,
			Pressure:      1,
			Window:        "weekly",
			WindowSeconds: usage.WindowWeekly,
			Used:          99,
			Class:         usage.BurnExhausted,
		},
	}
	r := testRouter(computed, "claude")
	_, err := r.Decide(LaunchRequest{
		Role:              RoleReviewer,
		Shape:             "implementation",
		Risk:              classify.TierR1,
		AuthorFamily:      "zhipu",
		RequestedProvider: "claude",
	})
	if err == nil {
		t.Fatal("sole weekly-capped candidate must fail closed in Decide (hard skip load-bearing; pressure bump alone would still pick it)")
	}
	if !strings.Contains(err.Error(), "no healthy") {
		t.Fatalf("want no-healthy-candidate error, got: %v", err)
	}
}

func TestPickWeeklyCapSoleCandidateFailsClosed(t *testing.T) {
	clearRouteEnv(t)
	computed := map[string]usage.BurnState{
		"claude": {
			Available:     true,
			Pressure:      1,
			Window:        "weekly",
			WindowSeconds: usage.WindowWeekly,
			Used:          99,
			Class:         usage.BurnExhausted,
		},
	}
	// Only claude CLI present and requested: no alternative to absorb pressure ranking.
	r := testRouter(computed, "claude")
	_, err := r.Pick("implementation", "claude", "")
	if err == nil {
		t.Fatal("sole weekly-capped candidate must fail closed in Pick (hard skip load-bearing; pressure bump alone would still pick it)")
	}
	if !strings.Contains(err.Error(), "no healthy") {
		t.Fatalf("want no-healthy-provider error, got: %v", err)
	}
}

func TestDecideLunaRequiresProbePass(t *testing.T) {
	clearRouteEnv(t)
	// codex implementation → gpt-5.6-luna (probe-gated). Without probe PASS, skip to next.
	r := testRouter(nil, "codex", "grok")
	// Only codex+grok: codex luna skipped without probe → grok wins.
	d, err := r.Decide(LaunchRequest{
		Role:              RoleReviewer,
		Shape:             "implementation",
		Risk:              classify.TierR2,
		AuthorFamily:      "zhipu",
		RequestedProvider: "",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Provider == "codex" && strings.Contains(d.Model, "luna") {
		t.Fatal("luna without probe pass must not be selected")
	}

	// With probe PASS, codex luna is eligible and preferred (waterfall: claude absent → grok before codex? implementation: claude, grok, codex...)
	// Request codex only with probe.
	key := ProbeKey("codex", "gpt-5.6-luna")
	d2, err := r.Decide(LaunchRequest{
		Role:              RoleWorker,
		Shape:             "implementation",
		Risk:              classify.TierR2,
		RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium",
		ProbeResults: map[string]bool{key: true},
	})
	if err != nil {
		t.Fatalf("Decide with probe: %v", err)
	}
	if d2.Provider != "codex" || d2.Model != "gpt-5.6-luna" {
		t.Fatalf("want codex/luna with probe pass, got %+v", d2)
	}
	if !d2.ProbeRequired || d2.ProbeKey != key {
		t.Fatalf("probe fields incomplete: %+v", d2)
	}
}

func TestDecideLunaUnknownProbeFailsClosed(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil, "codex")
	_, err := r.Decide(LaunchRequest{
		Role:              RoleWorker,
		Shape:             "implementation",
		RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium",
		// ProbeResults nil → unknown → fail closed
	})
	if err == nil {
		t.Fatal("luna with unknown probe must fail closed when sole candidate")
	}
}

func TestDecideNoCandidateFailsClosed(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(map[string]usage.BurnState{
		"claude": {Available: false, Reason: "exhausted", Pressure: 100},
		"grok":   {Available: false, Reason: "exhausted", Pressure: 100},
	}, "claude", "grok")
	_, err := r.Decide(LaunchRequest{
		Role:              RoleWorker,
		Shape:             "implementation",
		RequestedProvider: "claude",
	})
	if err == nil {
		t.Fatal("exhausted sole candidate must fail closed")
	}
}

func TestDecideReviewerFinalPassEffortInDecision(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil, "claude", "grok", "codex", "opencode", "agy", "kimi")
	d, err := r.Decide(LaunchRequest{
		Role:         RoleReviewer,
		Shape:        "qa",
		Risk:         classify.TierR3,
		FinalPass:    true,
		AuthorFamily: "xai",
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if d.Effort != "high" {
		t.Fatalf("final R3 review effort = %q, want high", d.Effort)
	}
	// Ensure argv carries the decided effort for claude surfaces.
	if d.Provider == "claude" {
		joined := strings.Join(d.Argv, " ")
		if !strings.Contains(joined, "high") {
			t.Fatalf("argv must embed decided effort, got %v", d.Argv)
		}
	}
}

func TestDecideStrictQuotaMissingFailsClosed(t *testing.T) {
	clearRouteEnv(t)
	// No computed quota rows; StrictQuota must refuse.
	r := testRouter(nil, "claude", "grok")
	_, err := r.Decide(LaunchRequest{
		Role:              RoleWorker,
		Shape:             "implementation",
		RequestedProvider: "claude",
		StrictQuota:       true,
	})
	if err == nil {
		t.Fatal("StrictQuota with missing ledger must fail closed")
	}
}

func TestDecideStrictQuotaRequiresFreshEntitlementAndExactProbe(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stale        bool
		handoffError string
		wantErr      bool
	}{
		{name: "fresh authenticated flat rate", wantErr: false},
		{name: "stale entitlement", stale: true, wantErr: true},
		{name: "remote handoff unavailable", handoffError: "OpenUsage command not found (exit 127)", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearRouteEnv(t)
			fixture := `{"generatedAt":"2026-08-30T15:00:00Z","providers":{"grok":{"displayName":"Grok","entitlement":"` +
				"unmetered" + `","resources":{},"stale":` + fmt.Sprint(tc.stale) + `}}}`
			var snap usage.UsageSnapshot
			if err := json.Unmarshal([]byte(fixture), &snap); err != nil {
				t.Fatal(err)
			}
			snap.QuotaHandoffError = tc.handoffError
			computed := usage.NewQuotaEngine().ComputeAll(&snap)
			r := testRouter(computed, "grok")
			probeCalls := 0
			r.Probes.Launchable = func(provider, model string) (bool, string) {
				probeCalls++
				if provider != "grok" || model != "grok-4.6" {
					t.Fatalf("probe target = %s/%s, want exact Grok tuple", provider, model)
				}
				return true, ""
			}
			_, err := r.Decide(LaunchRequest{
				Role: RoleReviewSupervisor, Shape: "qa",
				RequestedProvider: "grok", RequestedModel: "grok-4.6",
				ExcludedFamily: "openai", StrictQuota: true,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("strict route error = %v, wantErr=%t", err, tc.wantErr)
			}
			if tc.handoffError != "" && (err == nil || !strings.Contains(strings.ToLower(err.Error()), "handoff")) {
				t.Fatalf("strict route hid the handoff gate: %v", err)
			}
			if probeCalls == 0 {
				t.Fatal("strict route did not obtain exact live probe evidence")
			}
		})
	}
}

// Two-candidate weekly-cap preference check. Note: effectivePressure=200 on a
// capped surface already steers ranking away from claude when grok is healthy,
// so this alone does NOT mutation-prove the hard skip — see
// TestDecideWeeklyCapSoleCandidateFailsClosed / TestPickWeeklyCapSoleCandidateFailsClosed.
func TestDecideWeeklyCapIsLoadBearing(t *testing.T) {
	clearRouteEnv(t)
	computed := map[string]usage.BurnState{
		"claude": {
			Available: true, Pressure: 1, Window: "weekly",
			WindowSeconds: usage.WindowWeekly, Used: 99, Class: usage.BurnExhausted,
		},
		"grok": {Available: true, Pressure: 80},
	}
	r := testRouter(computed, "claude", "grok", "codex", "opencode", "agy", "kimi")
	// Preferential shape: implementation lists claude first.
	d, err := r.Decide(LaunchRequest{Role: RoleReviewer, Shape: "implementation", AuthorFamily: "zhipu"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Provider == "claude" {
		t.Fatal("REGRESSION: weekly-capped preferred provider was selected")
	}
	// Control: without weekly exhaustion, claude wins.
	healthy := map[string]usage.BurnState{
		"claude": {Available: true, Pressure: 1, Window: "5h", Used: 1},
		"grok":   {Available: true, Pressure: 80},
	}
	r2 := testRouter(healthy, "claude", "grok", "codex", "opencode", "agy", "kimi")
	d2, err := r2.Decide(LaunchRequest{Role: RoleReviewer, Shape: "implementation", AuthorFamily: "zhipu"})
	if err != nil {
		t.Fatal(err)
	}
	if d2.Provider != "claude" {
		t.Fatalf("control: healthy claude must win, got %s", d2.Provider)
	}
}

func TestProbeKeyStable(t *testing.T) {
	if ProbeKey("codex", "gpt-5.6-luna") != "codex|gpt-5.6-luna" {
		t.Fatal("probe key format drift")
	}
}

func TestLaunchDecisionFieldsComplete(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil, "grok", "claude")
	d, err := r.Decide(LaunchRequest{
		Role:         RoleReviewer,
		Shape:        "qa",
		Risk:         classify.TierR2,
		AuthorFamily: "deepseek",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Acceptance: provider, model, effort, pool, role, risk, family,
	// capability tier, probe key (may be empty if not required), rationale.
	if d.Provider == "" || d.Model == "" || d.Effort == "" || d.Pool == "" {
		t.Fatalf("missing core fields: %+v", d)
	}
	if d.Role == "" || d.Family == "" || d.CapabilityTier == "" || d.Rationale == "" {
		t.Fatalf("missing policy fields: %+v", d)
	}
	if d.Risk != classify.TierR2 {
		t.Fatalf("risk not plumbed: %s", d.Risk)
	}
}

func TestVerifyDecisionRequiresRouterIssuanceAndExactContext(t *testing.T) {
	clearRouteEnv(t)
	d, err := testRouter(nil, "codex").Decide(LaunchRequest{
		Role: RoleWorker, Shape: "implementation", RequestedProvider: "codex",
		RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium", TaskRef: "FAC-A",
		Scope:        ScopeLane,
		ProbeResults: map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDecision(d, "FAC-B", 0); err == nil {
		t.Fatal("decision issued for FAC-A must fail closed for FAC-B")
	}
	if err := VerifyDecision(d, "FAC-A", 0); err != nil {
		t.Fatalf("router-issued decision should verify for exact context: %v", err)
	}
	if _, err := RebindDecision(d, "FAC-B", 0); err == nil {
		t.Fatal("zero-generation task assignment must fail closed")
	}
	bound, err := RebindDecision(d, "FAC-B", 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyDecision(bound, "FAC-B", 7); err != nil {
		t.Fatalf("rebound decision should verify: %v", err)
	}
	if err := VerifyDecision(bound, "FAC-A", 0); err == nil {
		t.Fatal("rebound decision must not replay against original task")
	}
}

func TestVerifyDecisionRejectsPublicCanonicalForgery(t *testing.T) {
	forged := &LaunchDecision{Role: RoleWorker, Shape: "implementation", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", Argv: ArgvFor("codex", "gpt-5.6-luna", "medium")}
	forged.Proof = decisionProof(*forged)
	if err := VerifyDecision(forged, "", 0); err == nil {
		t.Fatal("recomputed public canonical proof must not establish router issuance")
	}
}

func TestTaskDecisionCannotBeIssuedWithoutPositiveLease(t *testing.T) {
	clearRouteEnv(t)
	_, err := testRouter(nil, "codex").Decide(LaunchRequest{
		Role: RoleWorker, Shape: "implementation", RequestedProvider: "codex",
		RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium", TaskRef: "FAC-175",
		Scope: ScopeTask, LeaseGeneration: 0,
		ProbeResults: map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): true},
	})
	if err == nil {
		t.Fatal("task decision with zero lease generation must fail closed")
	}
}

func TestDecisionProofBindsExplicitScope(t *testing.T) {
	clearRouteEnv(t)
	d, err := testRouter(nil, "codex").Decide(LaunchRequest{
		Role: RoleWorker, Shape: "implementation", RequestedProvider: "codex",
		RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium",
		ProbeResults: map[string]bool{ProbeKey("codex", "gpt-5.6-luna"): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.Scope, d.TaskRef = ScopeLane, "worker"
	if err := VerifyDecision(d, "worker", 0); err == nil {
		t.Fatal("changing lane/task scope without reissuance must fail proof verification")
	}
}
