package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfig writes content to a temp herd.yaml and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return cfgPath
}

const rosterHeader = `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
`

func TestLoadConfig_PromptRequired(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    # missing prompt
`)
	if _, err := LoadConfig(cfgPath); err == nil {
		t.Fatal("expected validation error for missing prompt path")
	}
}

func TestLoadConfig_InvalidAuthority(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
    authority: "execute"
`)
	if _, err := LoadConfig(cfgPath); err == nil {
		t.Fatal("expected validation error for invalid authority")
	}
}

func TestLoadConfig_ValidAuthority(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
    authority: "write"
`)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if cfg.Lanes[0].Authority != AuthorityWrite {
		t.Errorf("expected authority=write, got %q", cfg.Lanes[0].Authority)
	}
}

func TestLoadConfig_UnknownCapability(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
    capabilities: ["telekinesis"]
`)
	if _, err := LoadConfig(cfgPath); err == nil {
		t.Fatal("expected validation error for unknown capability")
	}
}

func TestLoadConfig_ValidCapabilities(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
    capabilities: ["git-write", "network"]
`)
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
	if len(cfg.Lanes[0].Capabilities) != 2 {
		t.Fatalf("expected 2 capabilities, got %v", cfg.Lanes[0].Capabilities)
	}
}

func TestLoadConfig_IncompatibleWithUnknownRole(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "worker"
    role: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
    incompatible_with: ["reviewer"]
`)
	if _, err := LoadConfig(cfgPath); err == nil {
		t.Fatal("expected validation error for incompatible_with referencing a role no lane declares")
	}
}

func TestLoadConfig_IncompatibleWithKnownRole(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "worker"
    role: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
    incompatible_with: ["reviewer"]
  - name: "assayer"
    role: "reviewer"
    agent_kind: "claude"
    model: "claude-sonnet-5"
    prompt: ".herd/prompts/reviewer.md"
`)
	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}
}

func TestLoadConfig_DuplicateStandingOwner(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "harvest-a"
    role: "harvest"
    agent_kind: "claude"
    model: "claude-sonnet-5"
    prompt: ".herd/prompts/harvest.md"
    standing: true
  - name: "harvest-b"
    role: "harvest"
    agent_kind: "claude"
    model: "claude-sonnet-5"
    prompt: ".herd/prompts/harvest.md"
    standing: true
`)
	if _, err := LoadConfig(cfgPath); err == nil {
		t.Fatal("expected validation error for duplicate standing owner of role harvest")
	}
}

func TestLoadConfig_StandingDifferentRolesAllowed(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "orchestrator"
    role: "orchestrator"
    agent_kind: "claude"
    model: "claude-opus-5"
    prompt: ".herd/prompts/orchestrator.md"
    standing: true
  - name: "harvest"
    role: "harvest"
    agent_kind: "claude"
    model: "claude-sonnet-5"
    prompt: ".herd/prompts/harvest.md"
    standing: true
`)
	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatalf("expected valid config with two distinct standing roles, got error: %v", err)
	}
}

func TestLoadConfig_OnlyOneStandingOwnerAllowed(t *testing.T) {
	cfgPath := writeConfig(t, rosterHeader+`
  - name: "harvest-a"
    role: "harvest"
    agent_kind: "claude"
    model: "claude-sonnet-5"
    prompt: ".herd/prompts/harvest.md"
    standing: true
  - name: "harvest-b"
    role: "harvest"
    agent_kind: "claude"
    model: "claude-sonnet-5"
    prompt: ".herd/prompts/harvest.md"
    standing: false
`)
	if _, err := LoadConfig(cfgPath); err != nil {
		t.Fatalf("expected valid config: only one lane claims the standing role, got error: %v", err)
	}
}
