package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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

func (h *Hook) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name        string          `json:"name"`
		URL         string          `json:"url"`
		Requirement HookRequirement `json:"requirement"`
		Timeout     json.RawMessage `json:"timeout"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	h.Name, h.URL, h.Requirement = raw.Name, raw.URL, raw.Requirement
	if len(raw.Timeout) == 0 || string(raw.Timeout) == "null" {
		return nil
	}
	var duration string
	if json.Unmarshal(raw.Timeout, &duration) == nil {
		parsed, err := time.ParseDuration(duration)
		if err != nil {
			return err
		}
		h.Timeout = parsed
		return nil
	}
	return json.Unmarshal(raw.Timeout, &h.Timeout)
}

type DiscoveryState string

const (
	DiscoveryNotDiscovered DiscoveryState = "not-discovered"
	DiscoveryNoHooks       DiscoveryState = "discovered-no-hooks"
	DiscoveryHooks         DiscoveryState = "discovered-hooks"
	DiscoveryFailed        DiscoveryState = "discovery-failed"
)

type HookDiscoveryResult struct {
	State               DiscoveryState
	Hooks               []Hook
	ApprovedAuthorities []string
}

type HookDiscovery interface {
	Discover(provider string) (HookDiscoveryResult, error)
}

type HookDiscoveryFunc func(provider string) (HookDiscoveryResult, error)

func (f HookDiscoveryFunc) Discover(provider string) (HookDiscoveryResult, error) {
	return f(provider)
}

func NoHooksDiscovery() HookDiscovery {
	return HookDiscoveryFunc(func(string) (HookDiscoveryResult, error) {
		return HookDiscoveryResult{State: DiscoveryNoHooks}, nil
	})
}

type FileDiscovery struct {
	Path string
}

func (d FileDiscovery) Discover(provider string) (HookDiscoveryResult, error) {
	path := strings.TrimSpace(d.Path)
	if path == "" {
		path = strings.TrimSpace(os.Getenv("HERD_HARNESS_HOOKS_FILE"))
	}
	if path == "" {
		path = ".herd/harness-hooks.json"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
	}
	var config struct {
		Providers map[string]struct {
			Hooks               []Hook   `json:"hooks"`
			ApprovedAuthorities []string `json:"approved_local_authorities"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(b, &config); err != nil {
		return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
	}
	entry, ok := config.Providers[strings.ToLower(strings.TrimSpace(provider))]
	if !ok {
		return HookDiscoveryResult{State: DiscoveryNotDiscovered}, nil
	}
	state := DiscoveryHooks
	if len(entry.Hooks) == 0 {
		state = DiscoveryNoHooks
	}
	return HookDiscoveryResult{State: state, Hooks: entry.Hooks, ApprovedAuthorities: entry.ApprovedAuthorities}, nil
}

type HookStatus string

const (
	HookHealthy     HookStatus = "healthy"
	HookUnavailable HookStatus = "unavailable"
	HookTimeout     HookStatus = "timeout"
	HookMalformed   HookStatus = "malformed"
)

type HookResult struct {
	Hook              Hook
	Status            HookStatus
	Code              HookCode
	EndpointClass     EndpointClass
	RedactedAuthority string
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

type HookCode string

const (
	HookCodeHealthy            HookCode = "hook.healthy"
	HookCodeUnavailable        HookCode = "hook.unavailable"
	HookCodeTimeout            HookCode = "hook.timeout"
	HookCodeMalformed          HookCode = "hook.malformed"
	HookCodeHTTPError          HookCode = "hook.http_error"
	HookCodeAuthority          HookCode = "hook.authority_rejected"
	HookCodeRedirect           HookCode = "hook.redirect_rejected"
	HookCodeDuplicate          HookCode = "hook.duplicate_identity"
	HookCodeUnknownRequirement HookCode = "hook.unknown_requirement"
	HookCodeTimeoutLimit       HookCode = "hook.timeout_limit"
	HookCodeDiscoveryFailed    HookCode = "hook.discovery_failed"
	HookCodeDiscoveryMissing   HookCode = "hook.discovery_missing"
	HookCodeDegraded           HookCode = "hook.degraded"
)

type EndpointClass string

const (
	EndpointLoopback      EndpointClass = "loopback"
	EndpointApprovedLocal EndpointClass = "approved-local"
	EndpointInvalid       EndpointClass = "invalid"
)

const (
	defaultHookTimeout = 2 * time.Second
	maxHookTimeout     = 2 * time.Second
	maxHookBudget      = 5 * time.Second
	maxHealthBody      = 4096
)

type HookCheckOptions struct {
	ApprovedAuthorities []string
	TotalTimeout        time.Duration
}

// CheckHooks probes hooks in stable order. A hook is healthy only when its
// URL is valid and it returns a bounded JSON health response with status
// "ok" or "healthy". Required failures make RequiredHealthy false; optional
// failures are represented by one deduplicated warning on the report.
func CheckHooks(ctx context.Context, hooks []Hook, identity HookIdentity, client *http.Client) HookReport {
	return CheckHooksWithOptions(ctx, hooks, identity, client, HookCheckOptions{})
}

func CheckHooksWithOptions(parent context.Context, hooks []Hook, identity HookIdentity, client *http.Client, options HookCheckOptions) HookReport {
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
	budget := options.TotalTimeout
	if budget <= 0 || budget > maxHookBudget {
		budget = maxHookBudget
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	report := HookReport{RequiredHealthy: true}
	degraded := make([]string, 0)
	seen := make(map[string]struct{}, len(ordered))
	for _, hook := range ordered {
		result := checkHook(ctx, hook, identity, client, options.ApprovedAuthorities, seen)
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

func checkHook(parent context.Context, hook Hook, identity HookIdentity, client *http.Client, approved []string, seen map[string]struct{}) HookResult {
	result := HookResult{Hook: hook}
	result.Code = HookCodeMalformed
	result.EndpointClass = EndpointInvalid
	if strings.TrimSpace(hook.Name) == "" {
		result.Status = HookMalformed
		return result
	}
	if hook.Requirement != HookRequired && hook.Requirement != HookOptional {
		result.Status, result.Code = HookMalformed, HookCodeUnknownRequirement
		return result
	}
	identityKey := strings.ToLower(strings.TrimSpace(hook.Name))
	if _, ok := seen[identityKey]; ok {
		result.Status, result.Code = HookMalformed, HookCodeDuplicate
		return result
	}
	seen[identityKey] = struct{}{}
	u, err := url.Parse(strings.TrimSpace(hook.URL))
	if err == nil && u != nil {
		result.RedactedAuthority = u.Host
	}
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		result.Status = HookMalformed
		return result
	}
	result.EndpointClass = endpointClass(u, approved)
	if result.EndpointClass == EndpointInvalid {
		result.Status, result.Code = HookMalformed, HookCodeAuthority
		return result
	}
	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	if timeout > maxHookTimeout {
		result.Status, result.Code = HookMalformed, HookCodeTimeoutLimit
		return result
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		result.Status = HookMalformed
		return result
	}
	// These headers are informational only; launch validation remains the
	// authority and never substitutes a hook-provided identity.
	req.Header.Set("X-Herd-Provider", identity.Provider)
	req.Header.Set("X-Herd-Model", identity.Model)
	req.Header.Set("X-Herd-Effort", identity.Effort)
	probeClient := *client
	probeClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if next.URL.User != nil || next.URL.RawQuery != "" || next.URL.Fragment != "" || endpointClass(next.URL, approved) == EndpointInvalid {
			return errRedirectRejected
		}
		return http.ErrUseLastResponse
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			result.Status, result.Code = HookTimeout, HookCodeTimeout
		} else if errors.Is(err, errRedirectRejected) {
			result.Status, result.Code = HookMalformed, HookCodeRedirect
		} else {
			result.Status, result.Code = HookUnavailable, HookCodeUnavailable
		}
		return result
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBody+1))
	if err != nil || len(body) > maxHealthBody || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		result.Status, result.Code = HookUnavailable, HookCodeHTTPError
		return result
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &health); err != nil || (health.Status != "ok" && health.Status != "healthy") {
		result.Status, result.Code = HookMalformed, HookCodeMalformed
		return result
	}
	result.Status, result.Code = HookHealthy, HookCodeHealthy
	return result
}

var errRedirectRejected = errors.New("hook redirect rejected")

func endpointClass(u *url.URL, approved []string) EndpointClass {
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "127.0.0.1" || host == "::1" || host == "localhost" {
		return EndpointLoopback
	}
	authority := strings.ToLower(strings.TrimSpace(u.Host))
	for _, candidate := range approved {
		if authority == strings.ToLower(strings.TrimSpace(candidate)) {
			return EndpointApprovedLocal
		}
	}
	return EndpointInvalid
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
