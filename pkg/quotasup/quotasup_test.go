package quotasup

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// A lane billed against the wrong pool reads as exhausted while its own pool
// is idle, and the supervisor reroutes work that never needed to move.
func TestQuotaPoolMapsIndependentlyMeteredPools(t *testing.T) {
	cases := []struct{ provider, model, want string }{
		{"codex", "gpt-5.3-codex-spark", "spark"},
		{"codex", "gpt-5.6-luna", "default"},
		{"claude", "claude-fable-5", "fable"},
		{"claude", "claude-sonnet-5", "default"},
		{"agy", "gemini-3-pro", "gemini"},
		{"agy", "claude-opus-4-6-thinking", "nonGemini"},
		{"grok", "grok-4.6", "default"},
		{"CODEX", "GPT-5.3-CODEX-SPARK", "spark"},
	}
	for _, c := range cases {
		if got := QuotaPool(c.provider, c.model); got != c.want {
			t.Errorf("QuotaPool(%q,%q) = %q, want %q", c.provider, c.model, got, c.want)
		}
	}
}

func TestQuotaProviderAliasesAgy(t *testing.T) {
	if got := QuotaProvider("agy"); got != "antigravity" {
		t.Fatalf("agy must map to the antigravity ledger, got %q", got)
	}
	if got := QuotaProvider("Claude"); got != "claude" {
		t.Fatalf("provider should normalise, got %q", got)
	}
}

func ptrB(b bool) *bool { return &b }
func ptrI(i int) *int   { return &i }

// An unreadable pool must never be treated as available.
func TestClassifyFailsClosedOnUnreadableQuota(t *testing.T) {
	if got := Classify(nil, DefaultWarnRunwayMinutes); got != Untracked {
		t.Fatalf("absent ledger row = %q, want untracked", got)
	}
	if got := Classify(&usage.BurnState{Stale: true}, DefaultWarnRunwayMinutes); got != Unknown {
		t.Fatalf("stale ledger = %q, want unknown", got)
	}
	for _, reason := range []string{"stale", "provider-error", "no-quota-data"} {
		if got := Classify(&usage.BurnState{Reason: reason}, DefaultWarnRunwayMinutes); got != Unknown {
			t.Fatalf("reason %q = %q, want unknown", reason, got)
		}
	}
}

func TestClassifyExhaustedAndAtRisk(t *testing.T) {
	if got := Classify(&usage.BurnState{Reason: "exhausted"}, DefaultWarnRunwayMinutes); got != Exhausted {
		t.Fatalf("exhausted reason = %q", got)
	}
	if got := Classify(&usage.BurnState{Class: usage.BurnExhausted}, DefaultWarnRunwayMinutes); got != Exhausted {
		t.Fatalf("exhausted class = %q", got)
	}
	atRisk := &usage.BurnState{ExhaustsBeforeReset: ptrB(true), RunwayMinutes: ptrI(30)}
	if got := Classify(atRisk, DefaultWarnRunwayMinutes); got != AtRisk {
		t.Fatalf("30m runway inside a 120m warning = %q, want at_risk", got)
	}
	// Projected to exhaust, but far enough out to be ordinary burn.
	far := &usage.BurnState{ExhaustsBeforeReset: ptrB(true), RunwayMinutes: ptrI(400)}
	if got := Classify(far, DefaultWarnRunwayMinutes); got != Healthy {
		t.Fatalf("400m runway = %q, want healthy", got)
	}
	// Burning, but not projected to exhaust before the window resets.
	fine := &usage.BurnState{ExhaustsBeforeReset: ptrB(false), RunwayMinutes: ptrI(10)}
	if got := Classify(fine, DefaultWarnRunwayMinutes); got != Healthy {
		t.Fatalf("not exhausting before reset = %q, want healthy", got)
	}
}

// A fresh supervisor run must not page the coordinator about a healthy fleet.
func TestFirstHealthyObservationIsBaselineNotAnIncident(t *testing.T) {
	if IsTransition(FirstObservation, Healthy) {
		t.Fatal("first healthy observation is a baseline, not a transition")
	}
	if !IsTransition(FirstObservation, Exhausted) {
		t.Fatal("first observation of an exhausted pool must be reported")
	}
	if IsTransition(Healthy, Healthy) {
		t.Fatal("no change is not a transition")
	}
	if !IsTransition(Exhausted, Healthy) {
		t.Fatal("recovery must be reported")
	}
	if !IsTransition(Healthy, AtRisk) {
		t.Fatal("degradation must be reported")
	}
}

func TestPriorFallsBackToFirstObservation(t *testing.T) {
	prev := &Snapshot{Agents: []Assignment{{Name: "smith", Capacity: AtRisk}}}
	if got := Prior(prev, "smith"); got != AtRisk {
		t.Fatalf("known lane = %q", got)
	}
	if got := Prior(prev, "scout"); got != FirstObservation {
		t.Fatalf("unseen lane = %q, want %q", got, FirstObservation)
	}
	if got := Prior(nil, "smith"); got != FirstObservation {
		t.Fatalf("no prior snapshot = %q", got)
	}
}

func TestCountsGroupUnknownWithUntracked(t *testing.T) {
	s := &Snapshot{Agents: []Assignment{
		{Capacity: Healthy}, {Capacity: Exhausted}, {Capacity: AtRisk},
		{Capacity: Unknown}, {Capacity: Untracked},
	}}
	c := s.Counts()
	if c.Agents != 5 || c.Exhausted != 1 || c.AtRisk != 1 || c.Unknown != 2 {
		t.Fatalf("counts = %+v", c)
	}
}

// `herdr agent list` reports no model, so a running lane's own argv is the
// only live evidence of which pool it bills.
func TestModelFromArgvRecoversTheLaunchedModel(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"separate value", []string{"codex", "--model", "gpt-5.3-codex-spark", "--effort", "low"}, "gpt-5.3-codex-spark"},
		{"equals form", []string{"claude", "--model=claude-fable-5"}, "claude-fable-5"},
		{"short flag", []string{"agy", "-m", "gemini-3.1-pro-high"}, "gemini-3.1-pro-high"},
		{"surface default", []string{"kimi", "--auto"}, ""},
		{"no argv at all", nil, ""},
		// A dangling flag must not read the next lane's argument or panic.
		{"dangling flag", []string{"codex", "--model"}, ""},
		// The first --model wins; a later mention is not the launch model.
		{"first wins", []string{"codex", "--model", "gpt-5.6-luna", "--model", "other"}, "gpt-5.6-luna"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ModelFromArgv(c.argv); got != c.want {
				t.Fatalf("ModelFromArgv(%v) = %q, want %q", c.argv, got, c.want)
			}
		})
	}
}

// The supervisor and the router must agree about which pool a model bills; if
// they diverge, every cap the supervisor sets lands on a surface the router
// never routes to.
func TestQuotaPoolAgreesWithTheRouterOnCanonicalModels(t *testing.T) {
	for _, c := range []struct{ provider, model string }{
		{"claude", "claude-fable-5"}, {"claude", "claude-sonnet-5"},
		{"agy", "gemini-3.1-pro-high"}, {"agy", "claude-opus-4-6-thinking"},
		{"codex", "gpt-5.3-codex-spark"}, {"codex", "gpt-5.6-luna"},
		{"grok", "grok-4.6"}, {"kimi", ""},
	} {
		if got, want := QuotaPool(c.provider, c.model), router.QuotaPoolFor(c.provider, c.model); got != want {
			t.Errorf("QuotaPool(%s,%s) = %q but the router bills %q",
				c.provider, c.model, got, want)
		}
	}
	// The ledger's own name for AGY still resolves to AGY's pools.
	if got := QuotaPool("antigravity", "gemini-3.1-pro-high"); got != "gemini" {
		t.Fatalf("ledger provider name = %q, want gemini", got)
	}
}

// A lane launched through the Pi harness carries a vendor-qualified model.
// Billed as-is it misses the pool rules entirely: "anthropic/claude-fable-5"
// fails the exact-match fable rule and "google/gemini-3.1-pro-high" fails the
// gemini prefix rule, so the supervisor caps a pool nobody is using while the
// real one runs uncapped.
func TestPiHarnessArgvBillsTheRoutedPool(t *testing.T) {
	for _, c := range []struct{ provider, model, wantPool string }{
		{"claude", "claude-fable-5", "fable"},
		{"claude", "claude-sonnet-5", "default"},
		{"agy", "gemini-3.1-pro-high", "gemini"},
		{"codex", "gpt-5.3-codex-spark", "spark"},
		{"codex", "gpt-5.6-luna", "default"},
		{"grok", "grok-4.6", "default"},
		// opencode and lazer models are their own routed names; Pi passes
		// them through, so nothing may be stripped off them either.
		{"opencode", "opencode/kimi-k3", "default"},
		{"lazer", "litellm/lazer/grok-4.6", "default"},
	} {
		harness, argv, err := router.HarnessArgvFor(c.provider, c.model, "high")
		if err != nil {
			t.Fatalf("HarnessArgvFor(%s,%s): %v", c.provider, c.model, err)
		}
		if harness != router.PiHarness {
			t.Fatalf("expected the pi harness, got %q", harness)
		}
		got := ModelFromArgv(argv)
		if got != c.model {
			t.Errorf("ModelFromArgv(%v) = %q, want the routed model %q", argv, got, c.model)
		}
		if pool := QuotaPool(c.provider, got); pool != c.wantPool {
			t.Errorf("%s/%s launched via pi bills pool %q, want %q", c.provider, c.model, pool, c.wantPool)
		}
	}
}

// Only Pi argv is de-qualified. A provider CLI invoked directly reports its
// own model verbatim, and a stray "anthropic/..." there is not ours to rewrite.
func TestOnlyPiArgvIsDeQualified(t *testing.T) {
	direct := []string{"claude", "--model", "anthropic/claude-fable-5"}
	if got := ModelFromArgv(direct); got != "anthropic/claude-fable-5" {
		t.Fatalf("non-pi argv = %q, want it verbatim", got)
	}
	// An absolute path to the harness still identifies it.
	piPath := []string{"/opt/homebrew/bin/pi", "--model", "anthropic/claude-fable-5"}
	if got := ModelFromArgv(piPath); got != "claude-fable-5" {
		t.Fatalf("pi by absolute path = %q, want the routed model", got)
	}
}
