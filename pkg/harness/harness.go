package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	HealthURL   string
	Requirement HookRequirement
	Timeout     time.Duration
	kind        hookKind
	executable  string
}

type hookKind string

const (
	hookHTTP    hookKind = "http"
	hookCommand hookKind = "command"
	hookPassive hookKind = "passive"
)

func (h *Hook) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name        string            `json:"name"`
		URL         string            `json:"url"`
		HealthURL   string            `json:"health_url"`
		Requirement HookRequirement   `json:"requirement"`
		Headers     map[string]string `json:"headers"`
		AllowedEnv  []string          `json:"allowedEnvVars"`
		If          string            `json:"if"`
		Status      string            `json:"statusMessage"`
		Once        bool              `json:"once"`
		Timeout     json.RawMessage   `json:"timeout"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	h.Name, h.URL, h.HealthURL, h.Requirement = raw.Name, raw.URL, raw.HealthURL, raw.Requirement
	h.kind = hookHTTP
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
	var seconds int64
	if err := json.Unmarshal(raw.Timeout, &seconds); err != nil || seconds < 0 {
		return fmt.Errorf("invalid hook timeout")
	}
	h.Timeout = time.Duration(seconds) * time.Second
	return nil
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
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
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

// DefaultDiscovery resolves a repo policy override when present and otherwise
// applies the selected provider's standard harness policy. Supported standard
// harnesses without endpoint hooks explicitly discover no hooks.
type DefaultDiscovery struct {
	OverridePath string
	Claude       ClaudeDiscovery
}

func (d DefaultDiscovery) Discover(provider string) (HookDiscoveryResult, error) {
	overridePath := strings.TrimSpace(d.OverridePath)
	if overridePath == "" {
		overridePath = strings.TrimSpace(os.Getenv("HERD_HARNESS_HOOKS_FILE"))
	}
	if overridePath == "" {
		overridePath = ".herd/harness-hooks.json"
	}
	if _, err := os.Stat(overridePath); err == nil {
		result, err := (FileDiscovery{Path: overridePath}).Discover(provider)
		if err != nil {
			return HookDiscoveryResult{State: DiscoveryFailed}, err
		}
		if result.State != DiscoveryNotDiscovered {
			return result, nil
		}
	} else if !os.IsNotExist(err) || strings.TrimSpace(d.OverridePath) != "" || strings.TrimSpace(os.Getenv("HERD_HARNESS_HOOKS_FILE")) != "" {
		return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
	}
	if strings.EqualFold(strings.TrimSpace(provider), "claude") {
		return d.Claude.Discover(provider)
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "grok", "kimi", "agy", "antigravity", "pi", "opencode":
		return HookDiscoveryResult{State: DiscoveryNoHooks}, nil
	default:
		return HookDiscoveryResult{State: DiscoveryNotDiscovered}, nil
	}
}

type ClaudeDiscovery struct {
	Paths []string
}

func (d ClaudeDiscovery) Discover(string) (HookDiscoveryResult, error) {
	paths := append([]string(nil), d.Paths...)
	if len(paths) == 0 {
		if path := strings.TrimSpace(os.Getenv("HERD_CLAUDE_SETTINGS_FILE")); path != "" {
			paths = []string{path}
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
			}
			// Low-to-high precedence: user settings, project settings, then
			// project-local settings. Paths are runtime inputs only and are
			// never placed in receipts or generated artifacts.
			paths = []string{filepath.Join(home, ".claude", "settings.json"), ".claude/settings.json", ".claude/settings.local.json"}
		}
	}
	merged := make(map[string]Hook)
	order := make([]string, 0)
	found := false
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			if len(paths) == 1 && strings.TrimSpace(os.Getenv("HERD_CLAUDE_SETTINGS_FILE")) != "" {
				return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
			}
			continue
		}
		if err != nil {
			return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
		}
		found = true
		hooks, err := parseClaudeHooks(b)
		if err != nil {
			return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
		}
		layerKeys := make(map[string]struct{}, len(hooks))
		for _, hook := range hooks {
			key := strings.ToLower(strings.TrimSpace(hook.Name))
			if _, duplicate := layerKeys[key]; duplicate {
				return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
			}
			layerKeys[key] = struct{}{}
			if _, exists := merged[key]; !exists {
				order = append(order, key)
			}
			merged[key] = hook
		}
	}
	if !found || len(merged) == 0 {
		return HookDiscoveryResult{State: DiscoveryNoHooks}, nil
	}
	sort.Strings(order)
	result := make([]Hook, 0, len(order))
	for _, key := range order {
		result = append(result, merged[key])
	}
	return HookDiscoveryResult{State: DiscoveryHooks, Hooks: result}, nil
}

func parseClaudeHooks(data []byte) ([]Hook, error) {
	var root map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	raw, ok := root["hooks"]
	if !ok {
		return nil, nil
	}
	var groups map[string][]struct {
		Matcher string            `json:"matcher"`
		Hooks   []json.RawMessage `json:"hooks"`
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&groups); err != nil {
		return nil, err
	}
	result := make([]Hook, 0)
	events := make([]string, 0, len(groups))
	for event := range groups {
		events = append(events, event)
	}
	sort.Strings(events)
	for _, event := range events {
		entries := groups[event]
		for _, entry := range entries {
			for index, rawItem := range entry.Hooks {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(rawItem, &fields); err != nil {
					return nil, err
				}
				var kind string
				if err := json.Unmarshal(fields["type"], &kind); err != nil {
					return nil, fmt.Errorf("invalid claude hook")
				}
				switch kind {
				case "command":
					var command string
					if err := json.Unmarshal(fields["command"], &command); err != nil || strings.TrimSpace(command) == "" {
						return nil, fmt.Errorf("invalid claude command hook")
					}
					hook := Hook{Name: claudeHookIdentityWithMaterial(event, entry.Matcher, "", "", index, command), Requirement: claudeHookRequirement(event), Timeout: defaultHookTimeout, kind: hookCommand, executable: commandExecutable(command)}
					if len(fields["timeout"]) > 0 {
						parsed, err := parseHookTimeout(fields["timeout"])
						if err != nil {
							return nil, err
						}
						hook.Timeout = parsed
					}
					result = append(result, hook)
				case "mcp_tool", "prompt", "agent":
					// Valid standard handlers are bound only by a digest. Their
					// tool, prompt, and agent bodies are never retained or inspected.
					result = append(result, Hook{Name: claudeHookIdentityWithMaterial(event, entry.Matcher, "", "", index, string(rawItem)), Requirement: claudeHookRequirement(event), Timeout: defaultHookTimeout, kind: hookPassive})
				case "http":
					var item struct {
						Type           string            `json:"type"`
						URL            string            `json:"url"`
						Name           string            `json:"name"`
						Headers        map[string]string `json:"headers"`
						AllowedEnvVars []string          `json:"allowedEnvVars"`
						If             string            `json:"if"`
						StatusMessage  string            `json:"statusMessage"`
						Once           bool              `json:"once"`
						Timeout        json.RawMessage   `json:"timeout"`
					}
					if err := decodeStrict(rawItem, &item); err != nil || strings.TrimSpace(item.URL) == "" {
						return nil, fmt.Errorf("invalid claude http hook")
					}
					hook := Hook{Name: claudeHookIdentity(event, entry.Matcher, item.Name, item.URL, index), URL: item.URL, Requirement: claudeHookRequirement(event), Timeout: defaultHookTimeout, kind: hookHTTP}
					if len(item.Timeout) > 0 {
						parsed, err := parseHookTimeout(item.Timeout)
						if err != nil {
							return nil, err
						}
						hook.Timeout = parsed
					}
					result = append(result, hook)
				default:
					return nil, fmt.Errorf("invalid claude hook")
				}
			}
		}
	}
	return result, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func parseHookTimeout(raw json.RawMessage) (time.Duration, error) {
	var duration string
	if json.Unmarshal(raw, &duration) == nil {
		return time.ParseDuration(duration)
	}
	var seconds int64
	if err := json.Unmarshal(raw, &seconds); err != nil || seconds < 0 {
		return 0, fmt.Errorf("invalid hook timeout")
	}
	return time.Duration(seconds) * time.Second, nil
}

func claudeHookIdentity(event, matcher, name, endpoint string, index int) string {
	return claudeHookIdentityWithMaterial(event, matcher, name, endpoint, index, "")
}

func claudeHookIdentityWithMaterial(event, matcher, name, endpoint string, index int, materialExtra string) string {
	u, err := url.Parse(endpoint)
	authority := "invalid"
	if err == nil && u != nil && u.Host != "" {
		authority = strings.ToLower(u.Host)
	}
	material := event + "\x00" + matcher + "\x00" + name + "\x00" + materialExtra
	if strings.TrimSpace(name) == "" {
		material += "\x00" + authority + fmt.Sprintf("\x00%d", index)
	}
	sum := sha256.Sum256([]byte(material))
	return fmt.Sprintf("claude:%s:%x", claudeEventClass(event), sum[:8])
}

func claudeEventClass(event string) string {
	switch strings.ToLower(event) {
	case "pretooluse":
		return "pre-tool"
	case "permissionrequest":
		return "permission"
	case "userpromptsubmit":
		return "prompt"
	case "stop", "subagentstop", "taskcompleted":
		return "stop"
	default:
		return "lifecycle"
	}
}

func claudeHookRequirement(event string) HookRequirement {
	switch strings.ToLower(event) {
	case "pretooluse", "permissionrequest", "userpromptsubmit", "stop", "subagentstop", "taskcompleted":
		return HookRequired
	case "sessionstart", "notification", "precompact", "sessionend", "teammateidle":
		return HookOptional
	default:
		return HookRequirement("unknown")
	}
}

func commandExecutable(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], "'\"")
}

type HookStatus string

const (
	HookHealthy     HookStatus = "healthy"
	HookUnavailable HookStatus = "unavailable"
	HookTimeout     HookStatus = "timeout"
	HookMalformed   HookStatus = "malformed"
)

type HookResult struct {
	Name              string
	Requirement       HookRequirement
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
	HookCodeHealthMalformed    HookCode = "hook.health_malformed"
)

type EndpointClass string

const (
	EndpointLoopback      EndpointClass = "loopback"
	EndpointApprovedLocal EndpointClass = "approved-local"
	EndpointInvalid       EndpointClass = "invalid"
	EndpointCommand       EndpointClass = "command-local"
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
			if hook.Requirement == HookRequired || IsPolicyCode(result.Code) {
				report.RequiredHealthy = false
			} else if hook.Requirement == HookOptional {
				degraded = append(degraded, result.Name+"="+string(result.Status))
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

func IsPolicyCode(code HookCode) bool {
	switch code {
	case HookCodeMalformed, HookCodeAuthority, HookCodeRedirect, HookCodeDuplicate,
		HookCodeUnknownRequirement, HookCodeTimeoutLimit:
		return true
	default:
		return false
	}
}

func checkHook(parent context.Context, hook Hook, identity HookIdentity, client *http.Client, approved []string, seen map[string]struct{}) HookResult {
	result := HookResult{Name: stableHookName(hook.Name), Requirement: hook.Requirement}
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
	if hook.kind == hookCommand {
		if hook.executable == "" {
			result.Status, result.Code = HookMalformed, HookCodeMalformed
			return result
		}
		if _, err := exec.LookPath(hook.executable); err != nil {
			result.Status, result.Code = HookUnavailable, HookCodeUnavailable
			result.EndpointClass = EndpointCommand
			return result
		}
		result.Status, result.Code, result.EndpointClass = HookHealthy, HookCodeHealthy, EndpointCommand
		return result
	}
	if hook.kind == hookPassive {
		result.Status, result.Code, result.EndpointClass = HookHealthy, HookCodeHealthy, EndpointCommand
		return result
	}
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
		timeout = maxHookTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if strings.TrimSpace(hook.HealthURL) != "" {
		return probeHealthURL(ctx, hook, identity, client, approved, result)
	}
	return probeReachability(ctx, u, result)
}

func probeReachability(ctx context.Context, u *url.URL, result HookResult) HookResult {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(u.Hostname(), port)
	var conn net.Conn
	var err error
	if u.Scheme == "https" {
		dialer := tls.Dialer{NetDialer: &net.Dialer{}}
		conn, err = dialer.DialContext(ctx, "tcp", address)
	} else {
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	if err != nil {
		if ctx.Err() != nil {
			result.Status, result.Code = HookTimeout, HookCodeTimeout
		} else {
			result.Status, result.Code = HookUnavailable, HookCodeUnavailable
		}
		return result
	}
	_ = conn.Close()
	result.Status, result.Code = HookHealthy, HookCodeHealthy
	return result
}

func probeHealthURL(ctx context.Context, hook Hook, identity HookIdentity, client *http.Client, approved []string, result HookResult) HookResult {
	health, err := url.Parse(strings.TrimSpace(hook.HealthURL))
	if err != nil || health.User != nil || health.RawQuery != "" || health.Fragment != "" || endpointClass(health, approved) == EndpointInvalid {
		result.Status, result.Code = HookMalformed, HookCodeAuthority
		return result
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, health.String(), nil)
	if err != nil {
		result.Status, result.Code = HookMalformed, HookCodeMalformed
		return result
	}
	req.Header.Set("X-Herd-Provider", identity.Provider)
	req.Header.Set("X-Herd-Model", identity.Model)
	req.Header.Set("X-Herd-Effort", identity.Effort)
	probeClient := *client
	probeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirectRejected }
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
	var healthResponse struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &healthResponse); err != nil || (healthResponse.Status != "ok" && healthResponse.Status != "healthy") {
		result.Status, result.Code = HookMalformed, HookCodeHealthMalformed
		return result
	}
	result.Status, result.Code = HookHealthy, HookCodeHealthy
	return result
}

func stableHookName(name string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(name)))
	return fmt.Sprintf("hook:%x", sum[:8])
}

var errRedirectRejected = errors.New("hook redirect rejected")

func endpointClass(u *url.URL, approved []string) EndpointClass {
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return EndpointLoopback
	}
	authority := strings.ToLower(strings.TrimSpace(u.Host))
	for _, candidate := range approved {
		candidateURL, err := url.Parse("http://" + strings.TrimSpace(candidate))
		if err == nil && net.ParseIP(candidateURL.Hostname()) != nil && net.ParseIP(candidateURL.Hostname()).IsLoopback() && authority == strings.ToLower(strings.TrimSpace(candidate)) {
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
