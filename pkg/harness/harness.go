package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
	"github.com/Kampe/Herdforge/pkg/toolpolicy"
)

type HarnessType string

const (
	HarnessClaude   HarnessType = "claude"
	HarnessCodex    HarnessType = "codex"
	HarnessOpenCode HarnessType = "opencode"
	HarnessGrok     HarnessType = "grok"
	HarnessKimi     HarnessType = "kimi"
	HarnessAGY      HarnessType = "agy"
	HarnessPI       HarnessType = "pi"
	HarnessGeneric  HarnessType = "generic"
)

type HarnessConfig struct {
	Type           HarnessType
	BinaryName     string
	PromptFlag     string
	NonInteractive bool
	Supported      bool
	Hooks          []Hook
}

// PolicyInvocation is the launch handoff boundary. It compiles policy data
// for a future launcher; it never creates a child process or mutates a task.
type PolicyInvocation struct {
	Argv          []string
	PolicyDigest  string
	ParentSession string
}

func (h *HarnessConfig) BuildPolicyInvocation(prompt string, policy agentpolicy.Contract, key []byte) (PolicyInvocation, error) {
	if err := policy.Verify(key); err != nil {
		return PolicyInvocation{}, err
	}
	if !h.Supported {
		return PolicyInvocation{}, fmt.Errorf("harness %q has no fleet-only controls", h.Type)
	}
	if policy.ParentExecutionFamily != string(h.Type) {
		return PolicyInvocation{}, fmt.Errorf("policy family %q does not match harness %q", policy.ParentExecutionFamily, h.Type)
	}
	argv := h.BuildInvocation(prompt)
	if h.Type == HarnessClaude {
		argv = []string{h.BinaryName, "--mcp-config", "{}", "--strict-mcp-config", "--disable-slash-commands", "--disallowed-tools", "Agent", "Task", "ToolSearch", "-p", prompt}
	}
	if h.Type == HarnessCodex {
		promptIndex := -1
		for index := 1; index < len(argv); index++ {
			if argv[index] == prompt {
				promptIndex = index
				break
			}
		}
		if promptIndex < 0 {
			return PolicyInvocation{}, fmt.Errorf("harness %q compiled invocation lost prompt", h.Type)
		}
		composed := append([]string(nil), argv[:1]...)
		composed = append(composed, "--disable", "multi_agent", "--disable", "multi_agent_v2")
		composed = append(composed, argv[1:promptIndex]...)
		composed = append(composed, argv[promptIndex+1:]...)
		composed = append(composed, prompt)
		argv = composed
	}
	return PolicyInvocation{Argv: argv, PolicyDigest: policy.PolicyDigest, ParentSession: policy.HerdrSession}, nil
}

// HookRequirement controls whether a hook is part of the launch safety
// boundary or only provides optional telemetry.
type HookRequirement string

const (
	HookRequired HookRequirement = "required"
	HookOptional HookRequirement = "optional"
)

type Hook struct {
	Name        string
	URL         string
	Requirement HookRequirement
	Timeout     time.Duration
}

type HookStatus string

const (
	HookHealthy     HookStatus = "healthy"
	HookUnavailable HookStatus = "unavailable"
	HookTimeout     HookStatus = "timeout"
	HookMalformed   HookStatus = "malformed"
)

type HookResult struct {
	Hook   Hook
	Status HookStatus
	Reason string
}

type HookReport struct {
	Results         []HookResult
	RequiredHealthy bool
	DegradedWarning string
}

type HookIdentity struct {
	Provider string
	Model    string
	Effort   string
}

const (
	defaultHookTimeout = 2 * time.Second
	maxHealthBody      = 4096
)

// CheckHooks probes hooks in stable order. A hook is healthy only when its
// URL is valid and it returns a bounded JSON health response with status
// "ok" or "healthy". Required failures make RequiredHealthy false; optional
// failures are represented by one deduplicated warning on the report.
func CheckHooks(ctx context.Context, hooks []Hook, identity HookIdentity, client *http.Client) HookReport {
	ordered := append([]Hook(nil), hooks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Name != ordered[j].Name {
			return ordered[i].Name < ordered[j].Name
		}
		return ordered[i].URL < ordered[j].URL
	})
	if client == nil {
		client = http.DefaultClient
	}
	report := HookReport{RequiredHealthy: true}
	degraded := make([]string, 0)
	for _, hook := range ordered {
		result := checkHook(ctx, hook, identity, client)
		report.Results = append(report.Results, result)
		if result.Status != HookHealthy {
			if hook.Requirement == HookRequired {
				report.RequiredHealthy = false
			} else if hook.Requirement == HookOptional {
				degraded = append(degraded, hook.Name+"="+string(result.Status))
			} else {
				report.RequiredHealthy = false
			}
		}
	}
	if len(degraded) > 0 {
		report.DegradedWarning = "optional harness hooks degraded: " + strings.Join(degraded, ",")
	}
	return report
}

func checkHook(parent context.Context, hook Hook, identity HookIdentity, client *http.Client) HookResult {
	result := HookResult{Hook: hook}
	if strings.TrimSpace(hook.Name) == "" || (hook.Requirement != HookRequired && hook.Requirement != HookOptional) {
		result.Status, result.Reason = HookMalformed, "name and required/optional classification are required"
		return result
	}
	u, err := url.Parse(strings.TrimSpace(hook.URL))
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		result.Status, result.Reason = HookMalformed, "hook URL must be an http(s) URL with a host"
		return result
	}
	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		result.Status, result.Reason = HookMalformed, err.Error()
		return result
	}
	// These headers are informational only; launch validation remains the
	// authority and never substitutes a hook-provided identity.
	req.Header.Set("X-Herd-Provider", identity.Provider)
	req.Header.Set("X-Herd-Model", identity.Model)
	req.Header.Set("X-Herd-Effort", identity.Effort)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			result.Status = HookTimeout
			result.Reason = ctx.Err().Error()
		} else {
			result.Status = HookUnavailable
			result.Reason = err.Error()
		}
		return result
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBody+1))
	if err != nil || len(body) > maxHealthBody || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.Status = HookUnavailable
		if err != nil {
			result.Reason = err.Error()
		} else {
			result.Reason = fmt.Sprintf("health response status %d or body too large", resp.StatusCode)
		}
		return result
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &health); err != nil || (health.Status != "ok" && health.Status != "healthy") {
		result.Status = HookMalformed
		result.Reason = "health response must be JSON with status ok or healthy"
		return result
	}
	result.Status = HookHealthy
	return result
}

// GetHarnessConfig maps harness identifiers to CLI invocation conventions
func GetHarnessConfig(harness string) *HarnessConfig {
	switch strings.ToLower(harness) {
	case "claude":
		return &HarnessConfig{Type: HarnessClaude, BinaryName: "claude", PromptFlag: "-p", NonInteractive: true, Supported: true}
	case "codex":
		return &HarnessConfig{Type: HarnessCodex, BinaryName: "codex", NonInteractive: true, Supported: true}
	case "opencode":
		return &HarnessConfig{Type: HarnessOpenCode, BinaryName: "opencode", PromptFlag: "-p", NonInteractive: true}
	case "grok":
		return &HarnessConfig{Type: HarnessGrok, BinaryName: "grok", PromptFlag: "-p", NonInteractive: true}
	case "kimi":
		return &HarnessConfig{Type: HarnessKimi, BinaryName: "kimi", PromptFlag: "-p", NonInteractive: true}
	case "agy", "antigravity":
		return &HarnessConfig{Type: HarnessAGY, BinaryName: "antigravity-cli", PromptFlag: "-p", NonInteractive: true}
	case "pi":
		return &HarnessConfig{Type: HarnessPI, BinaryName: "pi", PromptFlag: "-p", NonInteractive: true}
	default:
		return &HarnessConfig{Type: HarnessGeneric, BinaryName: harness, PromptFlag: "-p", NonInteractive: true}
	}
}

// BuildInvocation constructs the exact CLI command array to spawn a subagent in the target harness
func (h *HarnessConfig) BuildInvocation(prompt string) []string {
	args, err := h.BuildInvocationE(prompt)
	if err != nil {
		return nil // callers cannot accidentally launch an uncompiled surface
	}
	return args
}

// BuildInvocationE is the fail-closed invocation compiler.
func (h *HarnessConfig) BuildInvocationE(prompt string) ([]string, error) {
	args := []string{h.BinaryName}
	if h.Type == HarnessCodex {
		// Preserve the current-main Codex positional prompt contract while
		// compiling the explicit tool-server boundary for child launches.
		args = append(args, prompt)
	} else if h.PromptFlag != "" {
		args = append(args, h.PromptFlag, prompt)
	} else {
		args = append(args, prompt)
	}
	if h.Type == HarnessCodex {
		// A Codex child must not inherit the operator's CRG MCP. The CRG CLI
		// remains available as a normal executable.
		return toolpolicy.CompileCodexArgs(args)
	}
	return args, nil
}

// LookPath checks if the harness binary is installed and executable in PATH
func (h *HarnessConfig) LookPath() (string, error) {
	path, err := exec.LookPath(h.BinaryName)
	if err != nil {
		return "", fmt.Errorf("harness binary '%s' not found in PATH: %w", h.BinaryName, err)
	}
	return path, nil
}
