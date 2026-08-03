package main

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

func TestWorkerConfigDriftRejectsBeforeLaunch(t *testing.T) {
	lane := &config.LaneDef{Name: "mutant", Role: "worker", AgentKind: "opencode", Provider: "lazer", Model: "litellm/lazer/deepseek-v4-flash", Effort: "medium", TaskShape: "implementation"}
	err := validateLaneLaunchConfig(lane)
	if err == nil {
		t.Fatalf("drift must fail closed, got %v", err)
	}
}

func TestConfiguredRolePoliciesAreComplete(t *testing.T) {
	cases := []config.LaneDef{
		{Name: "worker", Role: "worker", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "forge", Role: "forge-smith", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "recovery-worker", Role: "recovery", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "reviewer", Role: "reviewer", AgentKind: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "qa"},
		{Name: "orchestrator", Role: "orchestrator", AgentKind: "claude", Provider: "claude", Model: "claude-opus-5", Effort: "medium", TaskShape: "coordinator"},
		{Name: "scout-planner", Role: "scout-planner", AgentKind: "claude", Provider: "claude", Model: "claude-opus-5", Effort: "medium", TaskShape: "architecture"},
		{Name: "verification", Role: "verification-gate", AgentKind: "opencode", Provider: "opencode", Model: "opencode/kimi-k3", Effort: "medium", TaskShape: "bounded"},
		{Name: "supervisor", Role: "review-supervisor", AgentKind: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "coordinator"},
		{Name: "harvest", Role: "harvest", AgentKind: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "bounded"},
		{Name: "sentinel", Role: "recovery-sentinel", AgentKind: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "bounded"},
	}
	for _, lane := range cases {
		if err := validateLaneLaunchConfig(&lane); err != nil {
			t.Errorf("%s: %v", lane.Role, err)
		}
	}
}
