package herdr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/toolpolicy"
)

const (
	probeToken  = "PROBE_OK"
	probePrompt = "Reply with exactly: " + probeToken
)

// ProbeResult reports whether a model is actually usable right now.
type ProbeResult struct {
	Model     string
	Available bool
	Reason    string // "" when available; otherwise the exhaustion signal
}

// exhaustionSignals are emitted by supported model CLIs when a surface is
// spent, unauthenticated, or otherwise unavailable.
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
}

// ProbeModel preserves the legacy OpenCode-backed model probe API.
func ProbeModel(ctx context.Context, model string) ProbeResult {
	return ProbeProviderModel(ctx, "opencode", model, "")
}

// ProbeProviderModel runs a minimal generation through the configured provider
// CLI for the exact model. Unsupported providers fail closed instead of being
// silently measured through a different execution surface.
func ProbeProviderModel(ctx context.Context, provider, model, effort string) ProbeResult {
	pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	var lastMessagePath string
	if strings.EqualFold(strings.TrimSpace(provider), "codex") {
		file, err := os.CreateTemp("", "herd-codex-probe-*.txt")
		if err != nil {
			return ProbeResult{Model: model, Reason: "create probe output: " + err.Error()}
		}
		lastMessagePath = file.Name()
		if err := file.Close(); err != nil {
			os.Remove(lastMessagePath)
			return ProbeResult{Model: model, Reason: "close probe output: " + err.Error()}
		}
		defer os.Remove(lastMessagePath)
	}

	command, args, err := providerProbeCommand(provider, model, effort, lastMessagePath)
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
	if !probeOutputMatches(provider, out, lastMessagePath) {
		return ProbeResult{Model: model, Reason: "no exact probe output"}
	}
	return ProbeResult{Model: model, Available: true}
}

func probeOutputMatches(provider string, out []byte, lastMessagePath string) bool {
	if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return strings.TrimSpace(string(out)) == probeToken
	}
	message, err := os.ReadFile(lastMessagePath)
	if err != nil || strings.TrimSpace(string(message)) != probeToken {
		return false
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	seenEvent := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return false
		}
		seenEvent = true
		eventType := strings.ToLower(strings.TrimSpace(event.Type))
		itemType := strings.ToLower(strings.TrimSpace(event.Item.Type))
		if eventType == "error" || strings.HasSuffix(eventType, ".failed") || itemType == "error" {
			return false
		}
	}
	return scanner.Err() == nil && seenEvent
}

func providerProbeCommand(provider, model, effort, lastMessagePath string) (string, []string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if model == "" {
		return "", nil, fmt.Errorf("probe model is required")
	}

	switch provider {
	case "codex":
		effort = strings.TrimSpace(effort)
		if effort == "" {
			return "", nil, fmt.Errorf("codex probe effort is required")
		}
		if strings.TrimSpace(lastMessagePath) == "" {
			return "", nil, fmt.Errorf("codex probe output path is required")
		}
		return "codex", []string{
			"exec",
			"--model", model,
			"--sandbox", "read-only",
			"--skip-git-repo-check",
			"--json",
			"--ephemeral",
			"--output-last-message", lastMessagePath,
			"--config", "model_reasoning_effort=" + effort,
			"--config", toolpolicy.CodexDisableCodeReviewGraph,
			probePrompt,
		}, nil
	case "opencode", "ollama", "lazer":
		return "opencode", []string{"run", "--model", model, probePrompt}, nil
	default:
		return "", nil, fmt.Errorf("unsupported probe provider %q", provider)
	}
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

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
