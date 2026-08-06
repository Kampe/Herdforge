// Package toolpolicy is the compiled tool-server boundary for Herdforge
// processes.  It is deliberately independent of any harness implementation:
// a launch either carries the effective policy or it is not launchable.
package toolpolicy

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	CodeReviewGraph = "code-review-graph"
	// CodexDisableCodeReviewGraph is a complete disabled stdio server value.
	// A partial `.enabled=false` value is rejected by Codex when the inherited
	// config has no server definition ("invalid transport").
	CodexDisableCodeReviewGraph = `mcp_servers.code-review-graph={command="false",enabled=false}`
)

var ErrMissingPolicy = errors.New("tool policy is missing compiled effective configuration")

type Role string

const (
	RoleWorker     Role = "worker"
	RoleReviewer   Role = "reviewer"
	RoleVerifier   Role = "verification-gate"
	RoleRecovery   Role = "recovery"
	RoleResume     Role = "resume"
	RoleForgeSmith Role = "forge-smith"
	RoleStanding   Role = "standing"
)

// EffectiveConfig is the only configuration accepted by a Herdforge Codex
// launch. CRG's CLI remains available; only its inherited MCP child is off.
type EffectiveConfig struct {
	MCPServers map[string]bool `json:"mcp_servers"`
	CLI        map[string]bool `json:"cli"`
}

func CodexConfig() EffectiveConfig {
	return EffectiveConfig{MCPServers: map[string]bool{CodeReviewGraph: false}, CLI: map[string]bool{CodeReviewGraph: true}}
}

func (c EffectiveConfig) Valid() bool {
	mcpEnabled, mcpPresent := c.MCPServers[CodeReviewGraph]
	cliEnabled, cliPresent := c.CLI[CodeReviewGraph]
	return c.MCPServers != nil && mcpPresent && !mcpEnabled && c.CLI != nil && cliPresent && cliEnabled
}

// CompileCodexArgs adds an explicit override, never relying on inherited
// ~/.codex/config.toml. The executable and CLI are intentionally untouched.
func CompileCodexArgs(argv []string) ([]string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) != "codex" {
		return nil, fmt.Errorf("compile codex policy: codex executable is required")
	}
	out := make([]string, 0, len(argv))
	insertAt := -1
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			for _, trailing := range argv[i+1:] {
				if targetsCodeReviewGraph(trailing) {
					return nil, fmt.Errorf("compile codex policy: code-review-graph policy appears after --")
				}
			}
			if insertAt < 0 {
				insertAt = len(out)
			}
			out = append(out, argv[i:]...)
			break
		}
		if arg == "-c" || arg == "--config" {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("compile codex policy: %s requires a value", arg)
			}
			value := argv[i+1]
			if targetsCodeReviewGraph(value) {
				insertAt = len(out)
				i++
				continue
			}
			out = append(out, arg, value)
			i++
			continue
		}
		if value, ok := inlineConfigValue(arg); ok {
			if value == "" {
				return nil, fmt.Errorf("compile codex policy: inline config requires a value")
			}
			if targetsCodeReviewGraph(value) {
				insertAt = len(out)
				continue
			}
		}
		out = append(out, arg)
	}
	if insertAt < 0 {
		insertAt = len(out)
	}
	return insertCodexPolicy(out, insertAt), nil
}

func inlineConfigValue(arg string) (string, bool) {
	for _, prefix := range []string{"--config=", "-c="} {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix), true
		}
	}
	return "", false
}

func insertCodexPolicy(argv []string, at int) []string {
	out := make([]string, 0, len(argv)+2)
	out = append(out, argv[:at]...)
	out = append(out, "-c", CodexDisableCodeReviewGraph)
	return append(out, argv[at:]...)
}

func targetsCodeReviewGraph(value string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(CodeReviewGraph))
}

func Require(role Role, provider string, argv []string) ([]string, EffectiveConfig, error) {
	if strings.EqualFold(provider, "codex") {
		compiled, err := CompileCodexArgs(argv)
		if err != nil {
			return nil, EffectiveConfig{}, err
		}
		// Worker compilers may add the explicit inherited-MCP override. Every
		// control/resume role must instead present the already-compiled argv;
		// silently correcting it here would let callers discard the authority.
		if role != RoleWorker && role != RoleForgeSmith && role != RoleRecovery && !sameArgs(compiled, argv) {
			return nil, EffectiveConfig{}, fmt.Errorf("control-role codex argv is not compiled")
		}
		return compiled, CodexConfig(), nil
	}
	if strings.TrimSpace(string(role)) == "" || len(argv) == 0 {
		return nil, EffectiveConfig{}, ErrMissingPolicy
	}
	return append([]string(nil), argv...), EffectiveConfig{MCPServers: map[string]bool{}, CLI: map[string]bool{}}, nil
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

// Authorization binds an exceptional stateful server to every identity that
// can make it safe. Empty fields are never wildcards.
type Authorization struct {
	Repository   string
	Role         string
	Server       string
	Transport    string
	OwnerSession string
	ExpiresAt    time.Time
}

func (a Authorization) Valid(now time.Time) bool {
	return a.Repository != "" && a.Role != "" && a.Server != "" && a.Transport != "" && a.OwnerSession != "" && !a.ExpiresAt.IsZero() && now.Before(a.ExpiresAt)
}

func (a Authorization) Matches(b Authorization, now time.Time) bool {
	return a.Valid(now) && b.Valid(now) && a == b
}
