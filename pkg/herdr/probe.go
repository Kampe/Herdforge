package herdr

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/spin"
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
	"weekly limit",
	"daily limit",
	"monthly limit",
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

	command, args, delivery, err := providerProbeCommand(provider, model, effort)
	if err != nil {
		return ProbeResult{Model: model, Reason: err.Error()}
	}
	var promptFile string
	switch delivery.Mode {
	case router.DeliverByArg:
		// piProbeCommand already embeds the prompt in its argv; every other
		// arg-delivery surface takes it as the final positional.
		if !useLegacyPiProbe(provider, model, effort) {
			args = append(args, probePrompt)
		}
	case router.DeliverByFile:
		f, ferr := os.CreateTemp("", "herd-probe-*.txt")
		if ferr != nil {
			return ProbeResult{Model: model, Reason: "probe prompt file: " + ferr.Error()}
		}
		promptFile = f.Name()
		defer func() { _ = os.Remove(promptFile) }()
		if _, werr := f.WriteString(probePrompt); werr != nil {
			_ = f.Close()
			return ProbeResult{Model: model, Reason: "probe prompt file: " + werr.Error()}
		}
		_ = f.Close()
		// The headless contract names a prompt path placeholder; substitute the
		// real file rather than guessing a flag.
		argv, _ := router.HeadlessArgvFor(provider, model, effort, promptFile)
		if len(argv) == 0 {
			return ProbeResult{Model: model, Reason: "no headless contract for file delivery"}
		}
		command, args = argv[0], argv[1:]
	}
	cmd := exec.CommandContext(pctx, command, args...)
	if delivery.Mode == router.DeliverByStdin {
		cmd.Stdin = strings.NewReader(probePrompt)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	sanitizedOut := spin.StripTerminalControlSequences(string(out))
	combined := sanitizedOut + "\n" + spin.StripTerminalControlSequences(stderr.String())
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
			detail = spin.StripTerminalControlSequences(runErr.Error())
		}
		return ProbeResult{Model: model, Reason: "probe failed: " + detail}
	}
	if strings.TrimSpace(sanitizedOut) != probeToken {
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

// providerProbeCommand builds the noninteractive probe invocation.
//
// FAC-578: this used to carry its OWN switch over providers, listing only codex
// and the opencode family. So a surface the router could route and launch --
// native claude -- was rejected by the probe with `unsupported probe provider
// "claude"`, and a readiness preflight built on it refused a perfectly valid
// route before ever reaching the credential boundary it was meant to check.
//
// Which providers exist, and how each one accepts a prompt, are now read from
// router.HeadlessArgvFor: the single headless contract, already kept in lockstep
// with HeadlessProviders by its own test. A provider that can be launched
// headlessly can therefore always be probed.
func providerProbeCommand(provider, model, effort string) (string, []string, probeDelivery, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if model == "" && !modelOptionalForProbe(provider) {
		return "", nil, probeDelivery{}, fmt.Errorf("probe model is required")
	}
	// The Pi harness keeps its exact hardened argv: it is a different execution
	// contract, not another headless surface.
	if useLegacyPiProbe(provider, model, effort) {
		cmd, argv, err := piProbeCommand(provider, model, effort)
		return cmd, argv, probeDelivery{Mode: router.DeliverByArg}, err
	}
	argv, delivery := router.HeadlessArgvFor(provider, model, effort, "")
	if len(argv) == 0 {
		return "", nil, probeDelivery{}, fmt.Errorf("no headless contract for provider %q (cannot probe what cannot be launched)", provider)
	}
	return argv[0], argv[1:], probeDelivery{Mode: probeDeliveryFor(provider, delivery)}, nil
}

// probeDeliveryFor returns how the probe hands its prompt to a surface.
//
// This follows the headless contract, with ONE documented exception: the
// opencode family. The contract declares stdin for `opencode run`, but the
// probe has always passed the prompt as a positional argument and that is the
// form known to work here. Flipping it on the strength of the table alone would
// risk breaking a working probe, and the failure mode is silent — a surface
// that ignores stdin answers as though asked nothing, which reads as a dead
// model rather than a wiring bug.
//
// I could not settle this empirically: both forms failed against the configured
// gateway model, which is itself unavailable, so the experiment says nothing
// about delivery. The contract's opencode delivery is therefore SUSPECT and
// tracked separately; it is not silently trusted here.
func probeDeliveryFor(provider string, declared router.PromptDelivery) router.PromptDelivery {
	switch provider {
	case "opencode", "ollama", "lazer":
		return router.DeliverByArg
	}
	return declared
}

// probeDelivery records how the prompt reaches the probe process. Getting this
// wrong is SILENT: agy ignores stdin entirely and answers as though it were
// asked nothing, which reads as a dead model rather than a wiring bug.
type probeDelivery struct {
	Mode router.PromptDelivery
}

// useLegacyPiProbe reports whether this provider probes through the Pi harness
// rather than its headless contract.
//
// The EXACT provider/model/effort tuple must be passed: asking with an empty
// model makes PiModelFor fail, which silently reads as "no Pi harness" and
// routes a legacy-Pi deployment onto the wrong probe surface.
func useLegacyPiProbe(provider, model, effort string) bool {
	if provider != "codex" {
		return false
	}
	harness, _, err := router.HarnessArgvFor(provider, model, effort)
	return err == nil && harness == router.PiHarness
}

// modelOptionalForProbe covers model-default surfaces whose exact argv carries
// no model at all.
func modelOptionalForProbe(provider string) bool {
	argv, _ := router.HeadlessArgvFor(provider, "", "", "")
	return len(argv) > 0
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
