package agentpolicy

import (
	"fmt"
	"strings"
)

// Nested-agent CLI features that must be absent from every fleet-launched
// Claude or Codex surface. These are compile-time controls: the tools must
// not be exposed, not merely discouraged in prompt text.
const (
	CodexFeatureMultiAgent   = "multi_agent"
	CodexFeatureMultiAgentV2 = "multi_agent_v2"
	ClaudeToolAgent          = "Agent"
	ClaudeToolTask           = "Task"
	ClaudeToolSearch         = "ToolSearch"
)

// CompileCodexArgs injects the fleet nested-agent denials. Existing
// --disable multi_agent / multi_agent_v2 flags are replaced so a caller
// cannot leave a contradictory enable behind.
func CompileCodexArgs(argv []string) ([]string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) != "codex" {
		return nil, fmt.Errorf("compile codex nested-agent policy: codex executable is required")
	}
	out := make([]string, 0, len(argv)+4)
	out = append(out, argv[0])
	out = append(out, "--disable", CodexFeatureMultiAgent, "--disable", CodexFeatureMultiAgentV2)
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--disable" {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("compile codex nested-agent policy: --disable requires a value")
			}
			feature := strings.TrimSpace(argv[i+1])
			i++
			if feature == CodexFeatureMultiAgent || feature == CodexFeatureMultiAgentV2 {
				continue
			}
			out = append(out, "--disable", feature)
			continue
		}
		if feature, ok := inlineDisableFeature(arg); ok {
			if feature == CodexFeatureMultiAgent || feature == CodexFeatureMultiAgentV2 {
				continue
			}
			out = append(out, arg)
			continue
		}
		// Reject an explicit features.<name>=true for the forbidden pair.
		if value, isConfig := inlineConfigValue(arg); isConfig {
			if enablesForbiddenCodexFeature(value) {
				return nil, fmt.Errorf("compile codex nested-agent policy: refuses to enable %s", value)
			}
		}
		if arg == "-c" || arg == "--config" {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("compile codex nested-agent policy: %s requires a value", arg)
			}
			value := argv[i+1]
			if enablesForbiddenCodexFeature(value) {
				return nil, fmt.Errorf("compile codex nested-agent policy: refuses to enable %s", value)
			}
			out = append(out, arg, value)
			i++
			continue
		}
		out = append(out, arg)
	}
	return out, nil
}

func inlineDisableFeature(arg string) (string, bool) {
	const prefix = "--disable="
	if strings.HasPrefix(arg, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(arg, prefix)), true
	}
	return "", false
}

func inlineConfigValue(arg string) (string, bool) {
	for _, prefix := range []string{"--config=", "-c="} {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix), true
		}
	}
	return "", false
}

func enablesForbiddenCodexFeature(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, feature := range []string{CodexFeatureMultiAgent, CodexFeatureMultiAgentV2} {
		// features.multi_agent=true or features.multi_agent_v2.enabled=true
		if strings.Contains(lower, "features."+feature) && strings.Contains(lower, "true") {
			return true
		}
	}
	return false
}

// CompileClaudeArgs injects the fleet nested-agent denials for Claude Code.
// Agent/Task/ToolSearch must be on the disallowed list and an empty strict
// MCP config prevents inherited collaboration servers.
func CompileClaudeArgs(argv []string) ([]string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) != "claude" {
		return nil, fmt.Errorf("compile claude nested-agent policy: claude executable is required")
	}
	// Strip any prior denial flags so recompilation is idempotent, then
	// re-emit the closed set immediately after the binary.
	stripped := make([]string, 0, len(argv))
	stripped = append(stripped, argv[0])
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		switch arg {
		case "--mcp-config", "--disallowed-tools", "--disallowedTools":
			// Consume the flag and its values until the next option or end.
			i++
			for i < len(argv) && !strings.HasPrefix(argv[i], "-") {
				i++
			}
			i--
			continue
		case "--strict-mcp-config", "--disable-slash-commands":
			continue
		}
		if strings.HasPrefix(arg, "--mcp-config=") || strings.HasPrefix(arg, "--disallowed-tools=") || strings.HasPrefix(arg, "--disallowedTools=") {
			continue
		}
		stripped = append(stripped, arg)
	}
	out := make([]string, 0, len(stripped)+8)
	out = append(out, stripped[0])
	out = append(out,
		"--mcp-config", "{}",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--disallowed-tools", ClaudeToolAgent, ClaudeToolTask, ClaudeToolSearch,
	)
	out = append(out, stripped[1:]...)
	return out, nil
}

// nestedDenyCompiled lists providers with a verified vendor flag contract that
// RequireNestedDeny asserts on every launch argv.
var nestedDenyCompiled = map[string]bool{"codex": true, "claude": true}

// nestedDenyUnavailable lists providers the fleet routes today for which no
// vendor nested-agent denial flag has been verified against that vendor's CLI.
// Their argv is admitted UNCHANGED.
//
// Membership is a declaration that a provider's nested-agent posture is
// UNGUARDED, not that it is safe. Do not add a provider here to silence a
// launch refusal, and do not invent a vendor flag to move one to
// nestedDenyCompiled: the flag must exist in that CLI's real interface.
var nestedDenyUnavailable = map[string]bool{
	"agy": true, "grok": true, "kimi": true,
	"opencode": true, "ollama": true, "lazer": true,
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// NestedDenyCompiled reports whether provider's launch argv carries a denial
// that RequireNestedDeny actually asserts.
func NestedDenyCompiled(provider string) bool {
	return nestedDenyCompiled[normalizeProvider(provider)]
}

// NestedDenyUnavailable reports whether provider launches with no verified
// vendor denial flag, so RequireNestedDeny admits its argv unchanged.
func NestedDenyUnavailable(provider string) bool {
	return nestedDenyUnavailable[normalizeProvider(provider)]
}

// RequireNestedDeny fails closed when a codex/claude argv does not already
// carry the compiled nested-agent denials.
//
// A provider that is neither compiled nor explicitly declared unguarded is
// REFUSED. Adding a provider to the router without deciding its nested-agent
// posture used to launch it with no denial and no signal; the decision is now
// forced at the boundary rather than defaulted to allow.
//
// Passing this check is not containment — see the package doc. It proves the
// launched argv carried the flag, not that the vendor honoured it.
func RequireNestedDeny(provider string, argv []string) error {
	switch normalizeProvider(provider) {
	case "codex":
		compiled, err := CompileCodexArgs(argv)
		if err != nil {
			return err
		}
		if !sameArgs(compiled, argv) {
			return fmt.Errorf("agentpolicy: codex argv is not compiled for nested-agent denial")
		}
		return nil
	case "claude":
		compiled, err := CompileClaudeArgs(argv)
		if err != nil {
			return err
		}
		if !sameArgs(compiled, argv) {
			return fmt.Errorf("agentpolicy: claude argv is not compiled for nested-agent denial")
		}
		return nil
	default:
		if nestedDenyUnavailable[normalizeProvider(provider)] {
			return nil
		}
		return fmt.Errorf("agentpolicy: provider %q has no reviewed nested-agent posture", provider)
	}
}

func sameArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
