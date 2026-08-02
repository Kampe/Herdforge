package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type RouteShape string

const (
	RouteShapeChat      RouteShape = "chat"
	RouteShapeCode      RouteShape = "code"
	RouteShapeReview    RouteShape = "review"
	RouteShapePlanning  RouteShape = "planning"
	RouteShapeResearch  RouteShape = "research"
)

type RiskClass string

const (
	RiskR0Mechanical RiskClass = "R0"
	RiskR1Standard   RiskClass = "R1"
	RiskR2High       RiskClass = "R2"
	RiskR3Critical   RiskClass = "R3"
)

type ProviderConstraint string

const (
	ProviderAny        ProviderConstraint = "any"
	ProviderDeepSeek   ProviderConstraint = "deepseek"
	ProviderAnthropic  ProviderConstraint = "anthropic"
	ProviderGoogle     ProviderConstraint = "google"
	ProviderOpenAI     ProviderConstraint = "openai"
	ProviderXAI        ProviderConstraint = "xai"
	ProviderOllama     ProviderConstraint = "ollama"
)

type NetworkCapability string

const (
	NetworkOnline    NetworkCapability = "online"
	NetworkOffline   NetworkCapability = "offline"
	NetworkLimited   NetworkCapability = "limited"
)

type Config struct {
	Version        string          `yaml:"version"`
	Project        ProjectConfig   `yaml:"project"`
	TaskProvider   TaskProvider    `yaml:"task_provider"`
	Fleet          FleetConfig     `yaml:"fleet,omitempty"`
	Lanes          []LaneDef       `yaml:"lanes"`
	Verification   Verification    `yaml:"verification,omitempty"`
}

type FleetConfig struct {
	HerdrWorkspace string `yaml:"herdr_workspace,omitempty"`
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

type LaneDef struct {
	Name      string           `yaml:"name"`
	Role      string           `yaml:"role,omitempty"`
	AgentKind string           `yaml:"agent_kind"`
	Harness   string           `yaml:"harness,omitempty"`
	Prompt    string           `yaml:"prompt"`
	Worktree  string           `yaml:"worktree,omitempty"`
	Provider  string           `yaml:"provider,omitempty"`
	Model     string           `yaml:"model,omitempty"`
	// FallbackModels are tried in order when Model probes unavailable
	// (quota exhausted / no payment method). The first that probes healthy
	// launches the lane, so a spent surface fails over instead of silently
	// whiffing every build.
	FallbackModels []string    `yaml:"fallback_models,omitempty"`
	Route     *RouteShape      `yaml:"route,omitempty"`
	Risk      *RiskClass       `yaml:"risk,omitempty"`
	MaxInput  *int             `yaml:"max_input_tokens,omitempty"`
	MaxOutput *int             `yaml:"max_output_tokens,omitempty"`
	Network   *NetworkCapability `yaml:"network,omitempty"`
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
		if lane.AgentKind == "" {
			return fmt.Errorf("lanes[%d]: missing required field: agent_kind", i)
		}
		if lane.Model == "" {
			return fmt.Errorf("lanes[%d]: missing required field: model", i)
		}
		if lane.Route != nil {
			switch *lane.Route {
			case RouteShapeChat, RouteShapeCode, RouteShapeReview, RouteShapePlanning, RouteShapeResearch:
			default:
				return fmt.Errorf("lanes[%d]: invalid route shape %q", i, *lane.Route)
			}
		}
		if lane.Risk != nil {
			switch *lane.Risk {
			case RiskR0Mechanical, RiskR1Standard, RiskR2High, RiskR3Critical:
			default:
				return fmt.Errorf("lanes[%d]: invalid risk class %q", i, *lane.Risk)
			}
		}
		if lane.Network != nil {
			switch *lane.Network {
			case NetworkOnline, NetworkOffline, NetworkLimited:
			default:
				return fmt.Errorf("lanes[%d]: invalid network capability %q", i, *lane.Network)
			}
		}
	}
	return nil
}

var (
	DefaultConfigPath = filepath.Join(".herd", "herd.yaml")
	DefaultHerdDir    = ".herd"
)
