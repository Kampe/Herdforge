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

const CodeReviewGraph = "code-review-graph"

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
	return c.MCPServers != nil && c.MCPServers[CodeReviewGraph] == false && c.CLI != nil && c.CLI[CodeReviewGraph]
}

// CompileCodexArgs adds an explicit override, never relying on inherited
// ~/.codex/config.toml. The executable and CLI are intentionally untouched.
func CompileCodexArgs(argv []string) ([]string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) != "codex" {
		return nil, fmt.Errorf("compile codex policy: codex executable is required")
	}
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-c" && argv[i+1] == "mcp_servers.code-review-graph.enabled=false" {
			return append([]string(nil), argv...), nil
		}
	}
	out := append([]string(nil), argv...)
	out = append(out, "-c", "mcp_servers.code-review-graph.enabled=false")
	return out, nil
}

func Require(role Role, provider string, argv []string) ([]string, EffectiveConfig, error) {
	if strings.EqualFold(provider, "codex") {
		compiled, err := CompileCodexArgs(argv)
		if err != nil {
			return nil, EffectiveConfig{}, err
		}
		return compiled, CodexConfig(), nil
	}
	if strings.TrimSpace(string(role)) == "" || len(argv) == 0 {
		return nil, EffectiveConfig{}, ErrMissingPolicy
	}
	return append([]string(nil), argv...), EffectiveConfig{MCPServers: map[string]bool{}, CLI: map[string]bool{}}, nil
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
