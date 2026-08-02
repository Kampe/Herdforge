package herdr

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// ProbeResult reports whether a model is actually usable right now.
type ProbeResult struct {
	Model     string
	Available bool
	Reason    string // "" when available; otherwise the exhaustion signal
}

// exhaustionSignals are the substrings opencode/litellm emit when a surface
// is spent — quota, billing, or auth. Matching any means the model cannot
// run a build no matter what the config says. Learned the hard way: the
// OpenCode Zen free tier goes silent mid-generation and the launcher happily
// spawned agents that produced plans, not code (2026-08-02).
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

// ProbeModel runs a minimal opencode generation against a model and reports
// whether it actually produced output. Fast (short timeout) and side-effect
// free — it asks the model to echo a token. A non-zero exit, a timeout, or
// any exhaustion signal in the output means unavailable.
func ProbeModel(ctx context.Context, model string) ProbeResult {
	pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(pctx, "opencode", "run", "--model", model,
		"Reply with exactly: PROBE_OK")
	out, err := cmd.CombinedOutput()
	lower := strings.ToLower(string(out))

	for _, sig := range exhaustionSignals {
		if strings.Contains(lower, sig) {
			return ProbeResult{Model: model, Available: false, Reason: sig}
		}
	}
	if pctx.Err() == context.DeadlineExceeded {
		return ProbeResult{Model: model, Available: false, Reason: "probe timeout"}
	}
	if err != nil {
		return ProbeResult{Model: model, Available: false, Reason: "probe failed: " + firstLine(string(out))}
	}
	if !strings.Contains(lower, "probe_ok") {
		return ProbeResult{Model: model, Available: false, Reason: "no usable output"}
	}
	return ProbeResult{Model: model, Available: true}
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
