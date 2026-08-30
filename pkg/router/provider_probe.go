package router

import (
	"fmt"
	"strings"
	"time"
)

const providerProbeSentinel = "HERD_PROVIDER_PROBE_OK"

// providerProbeBudget is the explicit launch/admission deadline. It was an
// inline 45*time.Second literal in two places (defaultProviderProbe and
// runProviderProbe's fallback), which no caller or test could name.
const providerProbeBudget = 45 * time.Second

// ProbeUnknownPrefix marks a probe result that is UNKNOWN rather than a refusal.
//
// FAC-596: a deadline expiry on the admission path returned the bare "provider
// probe timeout", and callers surface the reason verbatim as the routing reason.
// It reads exactly like "quota" or "not logged in" -- a definitive no. It is not
// one: the provider may be healthy and merely slow, and routing away from it on
// that evidence is wrong. The package already has this vocabulary elsewhere
// ("live concurrency unknown: ..."); the probe did not use it.
const ProbeUnknownPrefix = "UNKNOWN (not a refusal)"

// probeTimeoutMarker is the internal token classifyProviderProbeOutput emits on
// deadline expiry and runProviderProbe matches on. It was a bare string written
// in one place and compared in another (CHA-2451), so a reword in either copy
// would silently disable the read-path deadline handling. One definition.
const probeTimeoutMarker = "provider probe timeout"

var providerProbeFailures = []string{
	"no configured provider", "no configured model", "not logged in", "authentication",
	"unauthorized", "forbidden", "no payment method", "insufficient", "quota",
	"rate limit", "rate_limit", "429", "exhausted", "out of credit", "billing",
	"usage limit",
}

// defaultProviderProbe performs one bounded request for launch/admission.
// Read paths (resolve-lane, herd route) use advertisingProviderProbe instead
// (CHA-2451). A successful process is not enough: the exact sentinel must be
// returned and known auth/quota errors are unavailable even when a CLI exits
// zero after printing the error.
func defaultProviderProbe(provider, model string) (bool, string) {
	return runProviderProbe(provider, model, providerProbeBudget)
}

func classifyProviderProbeOutput(output, combined string, runErr error, timedOut bool) (bool, string) {
	lower := strings.ToLower(combined)
	for _, signal := range providerProbeFailures {
		if strings.Contains(lower, signal) {
			return false, signal
		}
	}
	// The refusal signals above are checked first on purpose: a provider that
	// printed "quota" and then hung has still told us a definitive no.
	if timedOut {
		return false, probeTimeoutMarker
	}
	if runErr != nil {
		return false, "provider probe failed: " + firstProbeLine(combined, runErr.Error())
	}
	if strings.TrimSpace(output) != providerProbeSentinel {
		return false, "provider probe returned no exact readiness token"
	}
	return true, ""
}

func providerProbeCommand(provider, model string) (string, []string, string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if model == "" && provider != "kimi" {
		return "", nil, "", fmt.Errorf("provider probe requires a model")
	}
	prompt := "Reply with exactly: " + providerProbeSentinel
	switch provider {
	case "claude":
		return "claude", []string{"--model", model, "-p"}, prompt, nil
	case "agy":
		return "agy", []string{"--model", model, "--print", prompt}, "", nil
	case "codex":
		return "codex", []string{"exec", "--model", model, "-s", "read-only"}, prompt, nil
	case "grok":
		return "grok", []string{
			"--model", model,
			"--always-approve",
			"--no-subagents",
			"--disable-web-search",
			"-p", prompt,
		}, "", nil
	case "kimi":
		return "kimi", []string{"--auto"}, prompt, nil
	case "opencode", "ollama", "lazer":
		return "opencode", []string{"run", "--model", model, prompt}, "", nil
	default:
		return "", nil, "", fmt.Errorf("unsupported provider %q", provider)
	}
}

func firstProbeLine(text, fallback string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}
