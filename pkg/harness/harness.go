package harness

import (
	"fmt"
	"os/exec"
	"strings"

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
}

// GetHarnessConfig maps harness identifiers to CLI invocation conventions
func GetHarnessConfig(harness string) *HarnessConfig {
	switch strings.ToLower(harness) {
	case "claude":
		return &HarnessConfig{Type: HarnessClaude, BinaryName: "claude", PromptFlag: "-p", NonInteractive: true}
	case "codex":
		return &HarnessConfig{Type: HarnessCodex, BinaryName: "codex", PromptFlag: "--prompt", NonInteractive: true}
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
	if h.PromptFlag != "" {
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
