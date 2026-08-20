package router

import (
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/classify"
)

func TestReviewRouteAdmissionUsesSharedRiskTable(t *testing.T) {
	tests := []struct {
		name, provider, model string
		risk                  classify.Tier
		want                  bool
	}{
		{"r0 known vendor", "claude", "claude-3-5-haiku", classify.TierR0, true},
		{"r2 known vendor", "grok", "grok-3", classify.TierR2, true},
		{"unknown model", "", "mystery-model", classify.TierR2, false},
		{"unknown risk", "grok", "grok-3", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ReviewRouteAdmission(tt.provider, tt.model, tt.risk)
			if got != tt.want {
				t.Fatalf("ReviewRouteAdmission(%q, %q, %q) = %v, want %v", tt.provider, tt.model, tt.risk, got, tt.want)
			}
		})
	}
}

func TestSurfaceCapabilityTableIsInternallyLaunchable(t *testing.T) {
	for _, surface := range SurfaceCapabilities() {
		if surface.Provider == "" || surface.Harness == "" || surface.CLI == "" {
			t.Fatalf("incomplete capability: %+v", surface)
		}
		if _, ok := SurfaceFor(surface.Provider); !ok {
			t.Fatalf("provider %q is not resolvable", surface.Provider)
		}
		argv := ArgvFor(surface.Provider, "test-model", "medium")
		if len(argv) == 0 || argv[0] != surface.Harness {
			t.Fatalf("%s argv=%v, want harness %q", surface.Provider, argv, surface.Harness)
		}
		if !surface.ModelOptional {
			model := map[string]string{
				"agy": "gemini-3.1-pro-high", "claude": "claude-sonnet-5",
				"codex": "gpt-5.6-luna", "grok": "grok-4.6", "lazer": "litellm/lazer/grok-4.6",
				"ollama": "litellm/ollama/glm-5.2:cloud", "opencode": "opencode/deepseek-v4-pro",
			}[surface.Provider]
			if _, _, err := HarnessArgvFor(surface.Provider, model, "medium"); err != nil {
				t.Fatalf("%s harness argv: %v", surface.Provider, err)
			}
		}
	}
}

func TestKimiIsHeadlessOnlyUntilHerdrSupportsItsKind(t *testing.T) {
	surface, ok := SurfaceFor("kimi")
	if !ok || surface.VendorHarness || !surface.ModelOptional || !surface.Headless {
		t.Fatalf("kimi capability = %+v, present=%t", surface, ok)
	}
	if harness, _, err := HarnessArgvFor("kimi", "", "medium"); err == nil || harness != "" {
		t.Fatalf("kimi unexpectedly admitted as lane harness: %s %v", harness, err)
	}
}

func TestEffortApplicabilityMatchesVerbatimArgvContract(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{"agy", false}, {"kimi", false}, {"opencode", false},
		{"ollama", false}, {"lazer", false},
		{"claude", true}, {"codex", true}, {"grok", true},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := EffortApplicable(tt.provider); got != tt.want {
				t.Fatalf("EffortApplicable(%q)=%v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

func TestRouteIdentityRejectsAGYFallback(t *testing.T) {
	err := VerifyModelIdentity("agy", "claude-opus-4-6-thinking", "anthropic", "agy", "gemini-3.7-flash", "google")
	if err == nil || !strings.Contains(err.Error(), "route identity mismatch") {
		t.Fatalf("AGY fallback must fail post-launch identity verification: %v", err)
	}
}

func TestRouterRejectsSurfaceWithoutLiveLaunchBinary(t *testing.T) {
	r := NewRouter(nil, nil)
	r.Probes = &Probes{
		CLIPresent: func(string) bool { return true },
		Launchable: func(provider, model string) (bool, string) {
			return false, "binary missing from test PATH"
		},
		Now: func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	_, err := r.Decide(LaunchRequest{Role: RoleReviewer, Shape: "qa", TaskRef: "FAC-336", Scope: ScopeLane, AuthorFamily: "xai", AuthorModel: "grok-4.5", CandidateSHA: "0123456789abcdef0123456789abcdef01234567"})
	if err == nil || err.Error() == "" {
		t.Fatal("missing live launchability must fail closed with a reason")
	}
	if got := err.Error(); !strings.Contains(got, "launchable candidate") || !strings.Contains(got, "binary missing") {
		t.Fatalf("error=%q lacks actionable launchability reason", got)
	}
}
