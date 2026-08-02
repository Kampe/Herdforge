package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config represents the root .herd/herd.yaml configuration structure
type Config struct {
	Version        string          `yaml:"version"`
	Project        ProjectConfig   `yaml:"project"`
	TaskProvider   TaskProvider    `yaml:"task_provider"`
	ModelProviders []ModelProvider `yaml:"model_providers"`
	Roles          []RoleConfig    `yaml:"roles"`
	Verification   Verification    `yaml:"verification"`
}

type ProjectConfig struct {
	Name          string `yaml:"name"`
	DefaultBranch string `yaml:"default_branch"`
}

type TaskProvider struct {
	Type      string `yaml:"type"` // kaneo | github | linear
	ProjectID string `yaml:"project_id"`
	APIURL    string `yaml:"api_url"`
}

type ModelProvider struct {
	Name  string `yaml:"name"`
	Type  string `yaml:"type"` // anthropic | google | openai | ollama
	Model string `yaml:"model"`
}

type RoleConfig struct {
	Name             string `yaml:"name"`
	Provider         string `yaml:"provider"`
	FallbackProvider string `yaml:"fallback_provider,omitempty"`
	PromptPath       string `yaml:"prompt_path"`
}

type Verification struct {
	TestCommand      string `yaml:"test_command"`
	PreflightCommand string `yaml:"preflight_command,omitempty"`
}

// LoadConfig reads and parses a herd.yaml file from path
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid herd.yaml syntax: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

// Validate checks for required configuration fields
func (c *Config) Validate() error {
	if c.Version == "" {
		return fmt.Errorf("missing required field: version")
	}
	if c.Project.Name == "" {
		return fmt.Errorf("missing required field: project.name")
	}
	if c.TaskProvider.Type == "" {
		return fmt.Errorf("missing required field: task_provider.type")
	}
	return nil
}
