package router

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const providerProbeToken = "HERD_PROVIDER_PROBE_OK"

var providerProbeFailures = []string{
	"no configured provider", "no configured model", "not logged in", "authentication",
	"unauthorized", "forbidden", "no payment method", "insufficient", "quota",
	"rate limit", "rate_limit", "429", "exhausted", "out of credit", "billing",
	"usage limit",
}

// defaultProviderProbe performs one bounded request. A successful process is
// not enough: the exact sentinel must be returned and known auth/quota errors
// are unavailable even when a CLI exits zero after printing the error.
func defaultProviderProbe(provider, model string) (bool, string) {
	command, args, stdin, err := providerProbeCommand(provider, model)
	if err != nil {
		return false, err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	combined := string(out) + "\n" + stderr.String()
	return classifyProviderProbeOutput(string(out), combined, runErr, ctx.Err() == context.DeadlineExceeded)
}

func classifyProviderProbeOutput(output, combined string, runErr error, timedOut bool) (bool, string) {
	lower := strings.ToLower(combined)
	for _, signal := range providerProbeFailures {
		if strings.Contains(lower, signal) {
			return false, signal
		}
	}
	if timedOut {
		return false, "provider probe timeout"
	}
	if runErr != nil {
		return false, "provider probe failed: " + firstProbeLine(combined, runErr.Error())
	}
	if strings.TrimSpace(output) != providerProbeToken {
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
	prompt := "Reply with exactly: " + providerProbeToken
	switch provider {
	case "claude":
		return "claude", []string{"--model", model, "-p"}, prompt, nil
	case "agy":
		return "agy", []string{"--model", model, "--print", prompt}, "", nil
	case "codex":
		return "codex", []string{"exec", "--model", model, "-s", "read-only"}, prompt, nil
	case "grok":
		return "grok", []string{"--model", model, "--always-approve", prompt}, "", nil
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
