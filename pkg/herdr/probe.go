package herdr

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
)

const (
	probeToken  = "PROBE_OK"
	probePrompt = "Reply with exactly: " + probeToken
)

// ProbeResult is the outcome of a single model probe.
type ProbeResult struct {
	Model     string `json:"model"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // set when Available=false
}

// exhaustionSignals are substrings that indicate the surface is rate-limited
// or quota-exhausted rather than merely broken. Matched case-insensitively
// against combined stdout+stderr.
var exhaustionSignals = []string{
	"no payment method",
	"insufficient",
	"quota",
	"rate limit",
	"exhausted",
	"out of credit",
	"payment required",
	"unauthorized",
	"forbidden",
	"rate_limit",
	"429",
	"billing",
	"usage limit",
}

// ProbeModel preserves the legacy OpenCode-backed model probe API.
func ProbeModel(ctx context.Context, model string) ProbeResult {
	return ProbeProviderModel(ctx, "opencode", model, "")
}

// ProbeProviderModel runs a minimal generation through the configured provider
// CLI for the exact model. Unsupported providers fail closed instead of being
// silently measured through a different execution surface.
//
// Codex logical routes execute through the Pi harness with exact noninteractive
// flags and require stdout to be exactly PROBE_OK after trim. OpenCode-backed
// routes (opencode/ollama/lazer) keep the legacy OpenCode probe surface.
func ProbeProviderModel(ctx context.Context, provider, model, effort string) ProbeResult {
	pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	command, args, err := providerProbeCommand(provider, model, effort)
	if err != nil {
		return ProbeResult{Model: model, Reason: err.Error()}
	}
	cmd := exec.CommandContext(pctx, command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	combined := string(out) + "\n" + stderr.String()
	lower := strings.ToLower(combined)

	for _, sig := range exhaustionSignals {
		if strings.Contains(lower, sig) {
			return ProbeResult{Model: model, Reason: sig}
		}
	}
	if pctx.Err() == context.DeadlineExceeded {
		return ProbeResult{Model: model, Reason: "probe timeout"}
	}
	if runErr != nil {
		detail := firstLine(combined)
		if detail == "" {
			detail = runErr.Error()
		}
		return ProbeResult{Model: model, Reason: "probe failed: " + detail}
	}
	if strings.TrimSpace(string(out)) != probeToken {
		return ProbeResult{Model: model, Reason: "no exact probe output"}
	}
	return ProbeResult{Model: model, Available: true}
}

// firstLine returns the first non-empty trimmed line from s.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

func providerProbeCommand(provider, model, effort string) (string, []string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if model == "" {
		return "", nil, fmt.Errorf("probe model is required")
	}

	switch provider {
	case "codex":
		return piProbeCommand(provider, model, effort)
	case "opencode", "ollama", "lazer":
		return "opencode", []string{"run", "--model", model, probePrompt}, nil
	default:
		return "", nil, fmt.Errorf("unsupported probe provider %q", provider)
	}
}

// piProbeCommand builds the exact noninteractive Pi probe argv for a codex
// logical route. Model and thinking are derived from router.HarnessArgvFor so
// mapping stays single-sourced; malformed harness output fails closed.
func piProbeCommand(provider, model, effort string) (string, []string, error) {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return "", nil, fmt.Errorf("codex probe effort is required")
	}
	harness, harnessArgv, err := router.HarnessArgvFor(provider, model, effort)
	if err != nil {
		return "", nil, err
	}
	if harness != router.PiHarness {
		return "", nil, fmt.Errorf("probe requires pi harness, got %q", harness)
	}
	// Expected shape from HarnessArgvFor: [pi, --model, <piModel>, --thinking, <effort>]
	if len(harnessArgv) != 5 ||
		harnessArgv[0] != router.PiHarness ||
		harnessArgv[1] != "--model" ||
		harnessArgv[3] != "--thinking" {
		return "", nil, fmt.Errorf("malformed pi harness argv: %v", harnessArgv)
	}
	piModel := strings.TrimSpace(harnessArgv[2])
	thinking := strings.TrimSpace(harnessArgv[4])
	if piModel == "" || thinking == "" {
		return "", nil, fmt.Errorf("malformed pi harness argv: %v", harnessArgv)
	}
	return router.PiHarness, []string{
		"--no-session",
		"--no-approve",
		"--no-context-files",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-tools",
		"--model", piModel,
		"--thinking", thinking,
		"-p", probePrompt,
	}, nil
}

// ResolveHealthyModel probes primary then fallbacks in order and returns the
// first available, plus the probe trail for logging. Returns ("", trail)
// when every candidate is exhausted so the caller can fail loudly instead of
// launching a dead lane.
func ResolveHealthyModel(ctx context.Context, primary string, fallbacks []string) (string, []ProbeResult) {
	var trail []ProbeResult
	for _, m := range append([]string{primary}, fallbacks...) {
		if m == "" {
			continue
		}
		r := ProbeModel(ctx, m)
		trail = append(trail, r)
		if r.Available {
			return m, trail
		}
	}
	return "", trail
}
