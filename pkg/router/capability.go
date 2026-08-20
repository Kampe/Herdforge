package router

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/classify"
)

// ReviewCapability is the shared admission table for review routes. Keeping
// risk admission beside the launch-surface table prevents a supervisor from
// selecting a route that the launcher cannot create or that is too weak for
// the candidate's risk tier.
type ReviewCapability struct {
	Provider    string
	Harness     string
	Family      string
	MinimumRisk classify.Tier
}

// SurfaceCapability is the single admission record for a routed execution
// surface. Consumers must use this record instead of maintaining provider
// allowlists of their own: a surface is only launchable when every boundary
// agrees on its harness, argv, and model contract.
type SurfaceCapability struct {
	Provider      string
	Harness       string
	CLI           string
	VendorHarness bool
	Headless      bool
	ModelOptional bool
	// EffortApplicable tells route consumers whether Effort is a launch
	// control for this surface.  It is false when the surface ignores effort;
	// consumers must never append it speculatively.
	EffortApplicable bool
}

var surfaceCapabilities = []SurfaceCapability{
	{Provider: "agy", Harness: "agy", CLI: "agy", VendorHarness: true, Headless: true, EffortApplicable: false},
	{Provider: "claude", Harness: "claude", CLI: "claude", VendorHarness: true, Headless: true, EffortApplicable: true},
	{Provider: "codex", Harness: "codex", CLI: "codex", VendorHarness: true, Headless: true, EffortApplicable: true},
	{Provider: "grok", Harness: "grok", CLI: "grok", VendorHarness: true, Headless: true, EffortApplicable: true},
	// Kimi is headless-only until Herdr advertises a native kimi tab kind.
	// Keeping it in the surface table lets shot routing use its argv while
	// preventing READY routes from reaching a lane launcher that cannot spawn it.
	{Provider: "kimi", Harness: "kimi", CLI: "kimi", Headless: true, ModelOptional: true, EffortApplicable: false},
	{Provider: "lazer", Harness: "opencode", CLI: "opencode", Headless: true, EffortApplicable: false},
	{Provider: "ollama", Harness: "opencode", CLI: "opencode", Headless: true, EffortApplicable: false},
	{Provider: "opencode", Harness: "opencode", CLI: "opencode", VendorHarness: true, Headless: true, EffortApplicable: false},
}

// EffortApplicable reports whether Effort may be applied to a route's
// launch surface. Unknown surfaces are inapplicable so callers fail closed.
func EffortApplicable(provider string) bool {
	surface, ok := SurfaceFor(provider)
	return ok && surface.EffortApplicable
}

var reviewCapabilities = []ReviewCapability{
	{Provider: "agy", Harness: "agy", Family: "google", MinimumRisk: classify.TierR1},
	{Provider: "claude", Harness: "claude", Family: "anthropic", MinimumRisk: classify.TierR1},
	{Provider: "codex", Harness: "codex", Family: "codex", MinimumRisk: classify.TierR1},
	{Provider: "grok", Harness: "grok", Family: "grok", MinimumRisk: classify.TierR1},
	{Provider: "opencode", Harness: "opencode", Family: "lazer", MinimumRisk: classify.TierR1},
}

// ReviewCapabilities returns a stable copy of the single review admission
// table used by routing and the review supervisor.
func ReviewCapabilities() []ReviewCapability {
	result := make([]ReviewCapability, len(reviewCapabilities))
	copy(result, reviewCapabilities)
	return result
}

// ReviewProviderForModel maps the native vendor model prefixes to their
// launch provider. An explicit provider remains authoritative for aliases.
func ReviewProviderForModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(m, "claude"), strings.Contains(m, "anthropic"):
		return "claude"
	case strings.Contains(m, "gemini"), strings.Contains(m, "google"), strings.Contains(m, "agy"):
		return "agy"
	case strings.Contains(m, "gpt"), strings.Contains(m, "openai"), strings.Contains(m, "codex"):
		return "codex"
	case strings.Contains(m, "grok"), strings.Contains(m, "xai"):
		return "grok"
	case strings.Contains(m, "deepseek"), strings.Contains(m, "lazer"), strings.Contains(m, "opencode"):
		return "opencode"
	default:
		return ""
	}
}

// ReviewRouteAdmission checks the exact provider/model route against the
// shared risk table. Unknown providers, unknown models, and risk tiers outside
// R0-R3 are refused rather than guessed.
func ReviewRouteAdmission(provider, model string, risk classify.Tier) (bool, string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = ReviewProviderForModel(model)
	}
	if provider == "" {
		return false, "review route has no known provider"
	}
	if risk == "" {
		return false, "review route has no risk tier"
	}
	for _, capability := range reviewCapabilities {
		if capability.Provider != provider {
			continue
		}
		if model == "" {
			return false, fmt.Sprintf("review provider %q requires a model", provider)
		}
		// R0 is compatible with every known vendor surface; higher-risk work
		// must meet the table's minimum admission tier.
		if risk != classify.TierR0 && risk < capability.MinimumRisk {
			return false, fmt.Sprintf("review route %s/%s is not admitted for %s", provider, model, risk)
		}
		return true, ""
	}
	return false, fmt.Sprintf("review provider %q is not a configured vendor harness", provider)
}

// SurfaceCapabilities returns a stable, sorted snapshot for validation and
// help/config consumers. The returned slice cannot mutate the source table.
func SurfaceCapabilities() []SurfaceCapability {
	result := make([]SurfaceCapability, len(surfaceCapabilities))
	copy(result, surfaceCapabilities)
	return result
}

// SurfaceFor resolves a provider or harness name to its canonical capability.
func SurfaceFor(name string) (SurfaceCapability, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, surface := range surfaceCapabilities {
		if surface.Provider == name {
			return surface, true
		}
	}
	for _, surface := range surfaceCapabilities {
		if surface.Harness == name {
			return surface, true
		}
	}
	return SurfaceCapability{}, false
}

// ModelOptional reports whether an empty model means the surface's own CLI
// default rather than an incomplete launch decision.
func ModelOptional(provider string) bool {
	surface, ok := SurfaceFor(provider)
	return ok && surface.ModelOptional
}

// ValidateSurface checks the cross-boundary provider/harness contract.
func ValidateSurface(provider, harness string) error {
	surface, ok := SurfaceFor(provider)
	if !ok {
		return fmt.Errorf("unsupported routed provider %q", provider)
	}
	harness = strings.ToLower(strings.TrimSpace(harness))
	if harness != surface.Harness && !(harness == PiHarness && surface.Provider != "kimi") {
		return fmt.Errorf("provider %q requires harness %q, got %q", provider, surface.Harness, harness)
	}
	return nil
}

// ProbeSurface performs the cheap live admission check that must happen
// before a tab or pane is created. It deliberately checks the exact CLI named
// by the capability table, so a router candidate cannot be a provider that
// launch validation later cannot execute.
func ProbeSurface(surface SurfaceCapability) (bool, string) {
	if strings.TrimSpace(surface.CLI) == "" {
		return false, "capability has no launch CLI"
	}
	if _, err := exec.LookPath(surface.CLI); err != nil {
		return false, fmt.Sprintf("%s binary %q is not executable in PATH", surface.Provider, surface.CLI)
	}
	return true, ""
}
