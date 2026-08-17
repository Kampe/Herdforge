package router

import (
	"fmt"
	"os/exec"
	"strings"
)

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
}

var surfaceCapabilities = []SurfaceCapability{
	{Provider: "agy", Harness: "agy", CLI: "agy", VendorHarness: true, Headless: true},
	{Provider: "claude", Harness: "claude", CLI: "claude", VendorHarness: true, Headless: true},
	{Provider: "codex", Harness: "codex", CLI: "codex", VendorHarness: true, Headless: true},
	{Provider: "grok", Harness: "grok", CLI: "grok", VendorHarness: true, Headless: true},
	{Provider: "kimi", Harness: "kimi", CLI: "kimi", VendorHarness: true, Headless: true, ModelOptional: true},
	{Provider: "lazer", Harness: "opencode", CLI: "opencode", Headless: true},
	{Provider: "ollama", Harness: "opencode", CLI: "opencode", Headless: true},
	{Provider: "opencode", Harness: "opencode", CLI: "opencode", VendorHarness: true, Headless: true},
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
		if surface.Provider == name || surface.Harness == name {
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
