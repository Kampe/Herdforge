package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version      string        `yaml:"version"`
	Project      ProjectConfig `yaml:"project"`
	TaskProvider TaskProvider  `yaml:"task_provider"`
	Lanes        []LaneConfig  `yaml:"lanes"`
	Verification Verification  `yaml:"verification"`
}

type ProjectConfig struct {
	Name          string `yaml:"name"`
	DefaultBranch string `yaml:"default_branch"`
	RepoURL       string `yaml:"repo_url,omitempty"`
}

type TaskProvider struct {
	Type        string `yaml:"type"`
	ProjectID   string `yaml:"project_id"`
	WorkspaceID string `yaml:"workspace_id,omitempty"`
	APIURL      string `yaml:"api_url,omitempty"`
	APIKeyEnv   string `yaml:"api_key_env,omitempty"`
	UseCLI      bool   `yaml:"use_cli,omitempty"`
}

type LaneConfig struct {
	Name      string `yaml:"name"`
	Role      string `yaml:"role"`
	AgentKind string `yaml:"agent_kind"`
	Harness   string `yaml:"harness,omitempty"`
	Prompt    string `yaml:"prompt"`
	Worktree  string `yaml:"worktree,omitempty"`
	Provider  string `yaml:"provider,omitempty"`
	Model     string `yaml:"model,omitempty"`
}

type Verification struct {
	TestCommand      string `yaml:"test_command"`
	PreflightCommand string `yaml:"preflight_command,omitempty"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}
	return ParseConfig(data)
}

func ParseConfig(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid herd.yaml syntax: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}
	return &cfg, nil
}

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
	for i, lane := range c.Lanes {
		if lane.Name == "" {
			return fmt.Errorf("lanes[%d]: missing required field: name", i)
		}
		if lane.Role == "" {
			return fmt.Errorf("lanes[%d]: missing required field: role", i)
		}
		if lane.AgentKind == "" {
			return fmt.Errorf("lanes[%d]: missing required field: agent_kind", i)
		}
	}
	return nil
}
