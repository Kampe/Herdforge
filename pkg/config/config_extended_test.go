package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_ReadError(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/herd.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent config path")
	}
}

func TestValidate_MissingVersion(t *testing.T) {
	cfg := &Config{}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing version")
	}
}

func TestValidate_MissingProjectName(t *testing.T) {
	cfg := &Config{Version: "1"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing project.name")
	}
}

func TestValidate_MissingTaskProviderType(t *testing.T) {
	cfg := &Config{Version: "1", Project: ProjectConfig{Name: "test"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing task_provider.type")
	}
}

func TestValidate_LinearRequiresNonBlankProjectID(t *testing.T) {
	for _, projectID := range []string{"", " \t "} {
		cfg := &Config{
			Version:      "1",
			Project:      ProjectConfig{Name: "test"},
			TaskProvider: TaskProvider{Type: "linear", ProjectID: projectID},
		}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("project_id=%q: expected validation error", projectID)
		}
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "herd.yaml")
	os.WriteFile(cfgPath, []byte("invalid: yaml: broken: ["), 0644)

	_, err := LoadConfig(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}
