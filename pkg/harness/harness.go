package harness

import (
	"fmt"
	"os/exec"
	"strings"

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
