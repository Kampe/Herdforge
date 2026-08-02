package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
  - name: "worker"
    role: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
verification:
  test_command: "go test ./..."
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("expected valid config, got error: %v", err)
	}

	if cfg.Project.Name != "test-project" {
		t.Errorf("expected project name 'test-project', got '%s'", cfg.Project.Name)
	}
	if len(cfg.Lanes) != 1 {
		t.Fatalf("expected 1 lane, got %d", len(cfg.Lanes))
	}
	if cfg.Lanes[0].Name != "worker" {
		t.Errorf("expected lane name 'worker', got '%s'", cfg.Lanes[0].Name)
	}
	if cfg.Lanes[0].AgentKind != "opencode" {
		t.Errorf("expected agent_kind 'opencode', got '%s'", cfg.Lanes[0].AgentKind)
	}
}

func TestLoadConfig_Invalid(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
# missing project.name
task_provider:
  type: "kaneo"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected validation error, got nil")
	}
}

func TestLoadConfig_NoLanes(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("expected valid config with zero lanes, got error: %v", err)
	}
	if len(cfg.Lanes) != 0 {
		t.Errorf("expected 0 lanes, got %d", len(cfg.Lanes))
	}
}

func TestLoadConfig_LaneRequiresAgentKind(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
  - name: "bad-lane"
    role: "worker"
    model: "deepseek-v4-flash"
    # missing agent_kind
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected validation error for missing agent_kind, got nil")
	}
}

func TestLoadConfig_InvalidRouteShape(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
  - name: "bad-route"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    route: "nonexistent"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected validation error for invalid route shape")
	}
}

func TestLoadConfig_InvalidRiskClass(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
  - name: "bad-risk"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    risk: "R5"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected validation error for invalid risk class")
	}
}

func TestLoadConfig_InvalidNetworkCapability(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
  - name: "bad-network"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    network: "airgap"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected validation error for invalid network capability")
	}
}

func TestLoadConfig_ValidRouteShape(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	route := RouteShapeCode
	risk := RiskR2High
	network := NetworkLimited

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
  - name: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
    provider: "deepseek"
    route: "code"
    risk: "R2"
    network: "limited"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("expected valid config with route/risk/network, got error: %v", err)
	}
	if cfg.Lanes[0].Route == nil || *cfg.Lanes[0].Route != route {
		t.Errorf("expected route=%s, got %v", route, cfg.Lanes[0].Route)
	}
	if cfg.Lanes[0].Risk == nil || *cfg.Lanes[0].Risk != risk {
		t.Errorf("expected risk=%s, got %v", risk, cfg.Lanes[0].Risk)
	}
	if cfg.Lanes[0].Network == nil || *cfg.Lanes[0].Network != network {
		t.Errorf("expected network=%s, got %v", network, cfg.Lanes[0].Network)
	}
}

func TestLoadConfig_FleetHerdrWorkspace(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
fleet:
  herdr_workspace: "wF"
lanes:
  - name: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("expected valid config with fleet.herdr_workspace, got error: %v", err)
	}
	if cfg.Fleet.HerdrWorkspace != "wF" {
		t.Errorf("expected fleet.herdr_workspace='wF', got %q", cfg.Fleet.HerdrWorkspace)
	}
}

func TestLoadConfig_FleetHerdrWorkspaceDefaultEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
  - name: "worker"
    agent_kind: "opencode"
    model: "deepseek-v4-flash"
    prompt: ".herd/prompts/worker.md"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("expected valid config without fleet, got error: %v", err)
	}
	if cfg.Fleet.HerdrWorkspace != "" {
		t.Errorf("expected empty fleet.herdr_workspace by default, got %q", cfg.Fleet.HerdrWorkspace)
	}
}

func TestLoadConfig_ModelRequired(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")

	content := `
version: "1"
project:
  name: "test-project"
  default_branch: "main"
task_provider:
  type: "kaneo"
  project_id: "test-id"
lanes:
  - name: "no-model"
    agent_kind: "opencode"
    # missing model
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatalf("expected validation error for missing model")
	}
}
