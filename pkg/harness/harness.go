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
	Name           string
	URL            string
	HealthURL      string
	Requirement    HookRequirement
	Timeout        time.Duration
	kind           hookKind
	executable     string
	runtimeKey     string
	behaviorDigest string
}

// HookPolicy is trusted Herdforge metadata for one discovered handler. The
// digest is the canonical discovered handler identity; behavior is never
// copied into this record.
type HookPolicy struct {
	HandlerDigest string          `json:"handler_digest"`
	Requirement   HookRequirement `json:"requirement"`
	HealthURL     string          `json:"health_url"`
}

type HookPolicyInventory struct {
	Provider       string                     `json:"provider"`
	PolicyRevision string                     `json:"policy_revision"`
	Handlers       []HookPolicyInventoryEntry `json:"handlers"`
}

type HookPolicyInventoryEntry struct {
	HandlerDigest string          `json:"handler_digest"`
	Requirement   HookRequirement `json:"requirement"`
	NeedsHealth   bool            `json:"needs_health"`
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
	State                  DiscoveryState
	Hooks                  []Hook
	ApprovedAuthorities    []string
	Policies               []HookPolicy
	PolicyRequired         bool
	PolicyRevision         string
	ExpectedPolicyRevision string
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
			Hooks               []Hook       `json:"hooks"`
			ApprovedAuthorities []string     `json:"approved_local_authorities"`
			Policies            []HookPolicy `json:"policies"`
			Revision            string       `json:"revision"`
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
	revision := policyRevision(entry.Policies)
	if entry.Revision != "" && entry.Revision != revision {
		return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
	}
	return HookDiscoveryResult{State: state, Hooks: entry.Hooks, ApprovedAuthorities: entry.ApprovedAuthorities, Policies: entry.Policies, PolicyRequired: len(entry.Policies) > 0, PolicyRevision: revision, ExpectedPolicyRevision: entry.Revision}, nil
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
	var override HookDiscoveryResult
	hasOverride := false
	if _, err := os.Stat(overridePath); err == nil {
		result, err := (FileDiscovery{Path: overridePath}).Discover(provider)
		if err != nil {
			return HookDiscoveryResult{State: DiscoveryFailed}, err
		}
		hasOverride = result.State != DiscoveryNotDiscovered
		override = result
	} else if !os.IsNotExist(err) || strings.TrimSpace(d.OverridePath) != "" || strings.TrimSpace(os.Getenv("HERD_HARNESS_HOOKS_FILE")) != "" {
		return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
	}
	if strings.EqualFold(strings.TrimSpace(provider), "claude") {
		result, err := d.Claude.Discover(provider)
		if err != nil || result.State != DiscoveryHooks || !hasOverride {
			return result, err
		}
		result.Policies = override.Policies
		result.PolicyRevision = override.PolicyRevision
		result.ExpectedPolicyRevision = override.PolicyRevision
		return result, nil
	}
	if hasOverride {
		return override, nil
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "grok", "kimi", "agy", "antigravity", "pi", "opencode":
		return HookDiscoveryResult{State: DiscoveryNoHooks}, nil
	default:
		return HookDiscoveryResult{State: DiscoveryNotDiscovered}, nil
	}
}

type ClaudeDiscovery struct {
	Paths                  []string
	Policies               []HookPolicy
	PolicyRevision         string
	ExpectedPolicyRevision string
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
	merged := make([]Hook, 0)
	byRuntime := make(map[string]Hook)
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
		for _, hook := range hooks {
			key := hook.runtimeKey
			if key == "" {
				key = hook.Name
			}
			if existing, exists := byRuntime[key]; exists {
				if existing.behaviorDigest != hook.behaviorDigest {
					return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
				}
				continue
			}
			byRuntime[key] = hook
			merged = append(merged, hook)
		}
	}
	if !found || len(merged) == 0 {
		return HookDiscoveryResult{State: DiscoveryNoHooks}, nil
	}
	result := append([]Hook(nil), merged...)
	revision := d.PolicyRevision
	if revision == "" {
		revision = policyRevision(d.Policies)
	}
	if d.ExpectedPolicyRevision != "" && d.ExpectedPolicyRevision != revision {
		return HookDiscoveryResult{State: DiscoveryFailed}, fmt.Errorf("hook discovery failed")
	}
	return HookDiscoveryResult{State: DiscoveryHooks, Hooks: result, Policies: d.Policies, PolicyRequired: true, PolicyRevision: revision, ExpectedPolicyRevision: d.ExpectedPolicyRevision}, nil
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
					behavior := claudeCommandBehaviorDigest(fields)
					hook := Hook{Name: claudeHookIdentityWithMaterial(event, entry.Matcher, "", "", index, behavior), Requirement: claudeHookRequirement(event), Timeout: defaultHookTimeout, kind: hookCommand, executable: commandExecutable(command), runtimeKey: claudeRuntimeKey("command", event, entry.Matcher, command), behaviorDigest: behavior}
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
					behavior := claudeCommandBehaviorDigest(fields)
					result = append(result, Hook{Name: claudeHookIdentityWithMaterial(event, entry.Matcher, "", "", index, behavior), Requirement: claudeHookRequirement(event), Timeout: defaultHookTimeout, kind: hookPassive, runtimeKey: claudeRuntimeKey(kind, event, entry.Matcher, string(rawItem)), behaviorDigest: behavior})
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
					behavior := claudeHTTPBehaviorDigest(item.URL, item.Headers, item.AllowedEnvVars, item.If, item.StatusMessage, item.Once, item.Timeout)
					hook := Hook{Name: claudeHookIdentityWithMaterial(event, entry.Matcher, item.Name, item.URL, index, behavior), URL: item.URL, Requirement: claudeHookRequirement(event), Timeout: defaultHookTimeout, kind: hookHTTP, runtimeKey: claudeRuntimeKey("http", event, entry.Matcher, item.URL), behaviorDigest: behavior}
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

func claudeRuntimeKey(kind, event, matcher, value string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + event + "\x00" + matcher + "\x00" + value))
	return fmt.Sprintf("runtime:%x", sum[:])
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
	return fmt.Sprintf("claude:%s:%x", claudeEventClass(event), sum[:])
}

func claudeCommandBehaviorDigest(fields map[string]json.RawMessage) string {
	canonical, _ := json.Marshal(fields)
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("command-behavior:%x", sum[:])
}

func claudeHTTPBehaviorDigest(endpoint string, headers map[string]string, allowedEnv []string, condition, statusMessage string, once bool, timeout json.RawMessage) string {
	material, _ := json.Marshal(struct {
		Endpoint   string            `json:"endpoint"`
		Headers    map[string]string `json:"headers"`
		AllowedEnv []string          `json:"allowed_env"`
		Condition  string            `json:"if"`
		Status     string            `json:"status"`
		Once       bool              `json:"once"`
		Timeout    json.RawMessage   `json:"timeout"`
	}{endpoint, headers, append([]string(nil), allowedEnv...), condition, statusMessage, once, timeout})
	sum := sha256.Sum256(material)
	return fmt.Sprintf("http-behavior:%x", sum[:])
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
	case "pretooluse", "permissionrequest", "userpromptsubmit", "posttooluse", "posttoolusefailure", "subagentstart", "stop", "subagentstop", "taskcompleted":
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
	HookStructural  HookStatus = "structural"
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
	Provider       string
	Model          string
	Effort         string
	PolicyRevision string
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
	HookCodePolicyMissing      HookCode = "hook.policy_missing"
	HookCodePolicyStale        HookCode = "hook.policy_stale"
	HookCodePolicyDuplicate    HookCode = "hook.policy_duplicate"
	HookCodePolicyMismatch     HookCode = "hook.policy_mismatch"
	HookCodeNoHealth           HookCode = "hook.no_independent_health"
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
		HookCodeUnknownRequirement, HookCodeTimeoutLimit, HookCodePolicyMissing,
		HookCodePolicyStale, HookCodePolicyDuplicate, HookCodePolicyMismatch:
		return true
	default:
		return false
	}
}

func policyRevision(policies []HookPolicy) string {
	ordered := append([]HookPolicy(nil), policies...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].HandlerDigest < ordered[j].HandlerDigest })
	canonical, _ := json.Marshal(ordered)
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("sha256:%x", sum[:])
}

// HookPolicyRevision returns the full immutable revision for an authored
// policy set. It is safe to print and store; it contains no handler content.
func HookPolicyRevision(policies []HookPolicy) string { return policyRevision(policies) }

// DiscoverHookPolicyInventory exposes only bounded canonical digests and the
// immutable policy revision needed to author trusted metadata.
func DiscoverHookPolicyInventory(discovery HookDiscovery, provider string) (HookPolicyInventory, error) {
	result, err := discovery.Discover(provider)
	if err != nil {
		return HookPolicyInventory{}, err
	}
	entries := make([]HookPolicyInventoryEntry, 0, len(result.Hooks))
	for _, hook := range result.Hooks {
		entries = append(entries, HookPolicyInventoryEntry{HandlerDigest: hook.Name, Requirement: hook.Requirement, NeedsHealth: hook.Requirement == HookRequired})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].HandlerDigest < entries[j].HandlerDigest })
	return HookPolicyInventory{Provider: provider, PolicyRevision: result.PolicyRevision, Handlers: entries}, nil
}

// DiscoverHookPolicyInventoryJSON is the compiled read-only operator surface.
// It emits only full handler digests, requirement classifications, and the
// immutable policy revision; it never emits command text or endpoint data.
func DiscoverHookPolicyInventoryJSON(discovery HookDiscovery, provider string) ([]byte, error) {
	inventory, err := DiscoverHookPolicyInventory(discovery, provider)
	if err != nil {
		return nil, err
	}
	return json.Marshal(inventory)
}

func (d DefaultDiscovery) PolicyInventory(provider string) (HookPolicyInventory, error) {
	return DiscoverHookPolicyInventory(d, provider)
}

// ApplyHookPolicies binds trusted metadata to canonical discovered handlers.
// It rejects drift before any network probe. Only discoveries marked as
// policy-required should call this function.
func ApplyHookPolicies(hooks []Hook, policies []HookPolicy, revision string) ([]Hook, HookCode, string) {
	if len(policies) == 0 {
		return nil, HookCodePolicyMissing, firstHookDigest(hooks)
	}
	if strings.TrimSpace(revision) == "" || revision != policyRevision(policies) {
		return nil, HookCodePolicyStale, firstHookDigest(hooks)
	}
	byDigest := make(map[string]HookPolicy, len(policies))
	for _, policy := range policies {
		if strings.TrimSpace(policy.HandlerDigest) == "" {
			return nil, HookCodePolicyMismatch, policy.HandlerDigest
		}
		if _, exists := byDigest[policy.HandlerDigest]; exists {
			return nil, HookCodePolicyDuplicate, policy.HandlerDigest
		}
		byDigest[policy.HandlerDigest] = policy
	}
	bound := append([]Hook(nil), hooks...)
	matched := make(map[string]struct{}, len(hooks))
	for i := range bound {
		hook := &bound[i]
		policy, exists := byDigest[hook.Name]
		if !exists {
			if hook.Requirement == HookRequired || hook.Requirement == HookRequirement("unknown") {
				return nil, HookCodePolicyMissing, hook.Name
			}
			continue
		}
		matched[hook.Name] = struct{}{}
		if policy.Requirement != hook.Requirement || (policy.Requirement != HookRequired && policy.Requirement != HookOptional) {
			if policy.Requirement != HookRequired && policy.Requirement != HookOptional {
				return nil, HookCodePolicyMismatch, hook.Name
			}
		}
		// The exact current digest and immutable policy revision make the
		// trusted policy, rather than the event default, authoritative. This
		// permits an explicitly classified telemetry hook on a critical event.
		hook.Requirement = policy.Requirement
		if strings.TrimSpace(policy.HealthURL) == "" {
			if hook.Requirement == HookRequired {
				return nil, HookCodeNoHealth, hook.Name
			}
			continue
		}
		if !validHealthAuthority(policy.HealthURL) {
			return nil, HookCodeAuthority, hook.Name
		}
		hook.HealthURL = policy.HealthURL
	}
	for digest := range byDigest {
		if _, exists := matched[digest]; !exists {
			return nil, HookCodePolicyMismatch, digest
		}
	}
	return bound, HookCodeHealthy, ""
}

func firstHookDigest(hooks []Hook) string {
	if len(hooks) == 0 {
		return ""
	}
	return hooks[0].Name
}

func validHealthAuthority(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return endpointClass(u, nil) == EndpointLoopback
}

func checkHook(parent context.Context, hook Hook, identity HookIdentity, client *http.Client, approved []string, seen map[string]struct{}) HookResult {
	result := HookResult{Name: hookResultName(hook.Name), Requirement: hook.Requirement}
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
	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	if timeout > maxHookTimeout {
		timeout = maxHookTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if hook.kind == hookCommand {
		if hook.executable == "" {
			result.Status, result.Code = HookMalformed, HookCodeMalformed
			return result
		}
		executable := expandHomeExecutable(hook.executable)
		if executable == "" {
			result.Status, result.Code = HookMalformed, HookCodeMalformed
			return result
		}
		if _, err := exec.LookPath(executable); err != nil {
			result.Status, result.Code = HookUnavailable, HookCodeUnavailable
			result.EndpointClass = EndpointCommand
			return result
		}
		if strings.TrimSpace(hook.HealthURL) == "" {
			result.Status, result.Code, result.EndpointClass = HookStructural, HookCodeNoHealth, EndpointCommand
			return result
		}
		return probeHealthURL(ctx, hook, identity, client, approved, result)
	}
	if hook.kind == hookPassive {
		if strings.TrimSpace(hook.HealthURL) == "" {
			result.Status, result.Code, result.EndpointClass = HookStructural, HookCodeNoHealth, EndpointCommand
			return result
		}
		return probeHealthURL(ctx, hook, identity, client, approved, result)
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
	if strings.TrimSpace(hook.HealthURL) != "" {
		return probeHealthURL(ctx, hook, identity, client, approved, result)
	}
	return probeReachability(ctx, u, result)
}

func hookResultName(name string) string {
	if strings.HasPrefix(name, "claude:") && len(name) >= len("claude:")+64 {
		return name
	}
	return stableHookName(name)
}

func expandHomeExecutable(raw string) string {
	if !strings.HasPrefix(raw, "$HOME/") {
		return raw
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Clean(filepath.Join(home, strings.TrimPrefix(raw, "$HOME/")))
	relative, err := filepath.Rel(home, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ""
	}
	return path
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
	result.EndpointClass = endpointClass(health, approved)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, health.String(), nil)
	if err != nil {
		result.Status, result.Code = HookMalformed, HookCodeMalformed
		return result
	}
	req.Header.Set("X-Herd-Provider", identity.Provider)
	req.Header.Set("X-Herd-Model", identity.Model)
	req.Header.Set("X-Herd-Effort", identity.Effort)
	req.Header.Set("X-Herd-Hook-Digest", hook.Name)
	req.Header.Set("X-Herd-Policy-Revision", identity.PolicyRevision)
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
		Status         string `json:"status"`
		HookDigest     string `json:"hook_digest"`
		PolicyRevision string `json:"policy_revision"`
	}
	if err := json.Unmarshal(body, &healthResponse); err != nil || (healthResponse.Status != "ok" && healthResponse.Status != "healthy") || (identity.PolicyRevision != "" && (healthResponse.HookDigest != hook.Name || healthResponse.PolicyRevision != identity.PolicyRevision)) {
		result.Status, result.Code = HookMalformed, HookCodeHealthMalformed
		return result
	}
	result.Status, result.Code = HookHealthy, HookCodeHealthy
	return result
}

func stableHookName(name string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(name)))
	return fmt.Sprintf("hook:%x", sum[:])
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
