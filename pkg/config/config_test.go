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
