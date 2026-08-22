package herdr

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/router"
)

// TestEveryLaunchableProviderIsProbeable is the FAC-578 gate.
//
// The probe carried its own switch over providers, listing only codex and the
// opencode family. Native claude could be routed and launched but not probed, so
// a readiness preflight built on the probe refused a valid route with
// `unsupported probe provider "claude"` before reaching the credential boundary
// it existed to check.
//
// Which providers exist is now read from the headless contract. If a provider
// can be launched headlessly, it must be probeable — otherwise any preflight
// built on the probe silently refuses working routes.
func TestEveryLaunchableProviderIsProbeable(t *testing.T) {
	providers := router.HeadlessProviders()
	if len(providers) == 0 {
		t.Fatal("headless provider set is empty; the contract cannot be verified")
	}
	for _, p := range providers {
		model := router.ModelFor(p, "qa")
		cmd, _, _, err := providerProbeCommand(p, model, "medium")
		if err != nil {
			t.Errorf("provider %q is launchable headlessly but not probeable: %v", p, err)
			continue
		}
		if strings.TrimSpace(cmd) == "" {
			t.Errorf("provider %q resolved an empty probe command", p)
		}
	}
}

// Native claude is the exact provider that was missing. Pinned by name so a
// future refactor cannot quietly drop it again.
func TestNativeClaudeIsProbeable(t *testing.T) {
	cmd, args, delivery, err := providerProbeCommand("claude", "claude-sonnet-5", "medium")
	if err != nil {
		t.Fatalf("native claude must be probeable: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("probe command = %q, want claude", cmd)
	}
	if delivery.Mode != router.DeliverByStdin {
		t.Errorf("claude takes its prompt on stdin, got delivery %v", delivery.Mode)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "claude-sonnet-5") {
		t.Errorf("probe argv must pin the exact model: %v", args)
	}
	if !strings.Contains(joined, "-p") {
		t.Errorf("probe argv must be noninteractive: %v", args)
	}
}

// An unknown provider must still fail closed, and the message must say WHY
// rather than implying the provider list is arbitrary.
func TestUnknownProviderStillFailsClosed(t *testing.T) {
	_, _, _, err := providerProbeCommand("definitely-not-a-provider", "m", "medium")
	if err == nil {
		t.Fatal("an unlaunchable provider must not be probeable")
	}
	if !strings.Contains(err.Error(), "cannot probe what cannot be launched") {
		t.Errorf("refusal should explain the contract, got: %v", err)
	}
}

// Prompt delivery must match each surface's declared contract. Getting this
// wrong is silent: agy ignores stdin and answers as though asked nothing.
func TestProbeDeliveryMatchesDeclaredContract(t *testing.T) {
	for _, p := range router.HeadlessProviders() {
		model := router.ModelFor(p, "qa")
		_, _, delivery, err := providerProbeCommand(p, model, "medium")
		if err != nil {
			continue // covered by the coverage test above
		}
		if useLegacyPiProbe(p, model, "medium") {
			continue // Pi keeps its own hardened argv contract
		}
		_, declared := router.HeadlessArgvFor(p, model, "medium", "/tmp/probe")
		want := probeDeliveryFor(p, declared)
		if delivery.Mode != want {
			t.Errorf("provider %q: probe delivery %v != resolved contract %v", p, delivery.Mode, want)
		}
		// The opencode family is the one documented divergence from the
		// declared contract. Every other surface must match it exactly, so a
		// second silent exception cannot creep in.
		switch p {
		case "opencode", "ollama", "lazer":
		default:
			if want != declared {
				t.Errorf("provider %q must follow the declared delivery contract (%v), not %v", p, declared, want)
			}
		}
	}
}
