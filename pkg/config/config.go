package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type RouteShape string

const (
	RouteShapeChat     RouteShape = "chat"
	RouteShapeCode     RouteShape = "code"
	RouteShapeReview   RouteShape = "review"
	RouteShapePlanning RouteShape = "planning"
	RouteShapeResearch RouteShape = "research"
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
	ProviderAny       ProviderConstraint = "any"
	ProviderDeepSeek  ProviderConstraint = "deepseek"
	ProviderAnthropic ProviderConstraint = "anthropic"
	ProviderGoogle    ProviderConstraint = "google"
	ProviderOpenAI    ProviderConstraint = "openai"
	ProviderXAI       ProviderConstraint = "xai"
	ProviderOllama    ProviderConstraint = "ollama"
)

type NetworkCapability string

const (
	NetworkOnline  NetworkCapability = "online"
	NetworkOffline NetworkCapability = "offline"
	NetworkLimited NetworkCapability = "limited"
)

// Authority is a lane's repository write authority. Roles that commit,
// merge, or otherwise mutate tracked files declare "write"; every other
// role (routing, review, verification, observation) declares "read".
type Authority string

const (
	AuthorityRead  Authority = "read"
	AuthorityWrite Authority = "write"
)

// Capability names a tool/network requirement a lane needs to launch.
// This is the vocabulary "herd status" and capability probing check a
// lane's launch surface against before granting a spawn.
type Capability string

const (
	CapabilityNetwork    Capability = "network"
	CapabilityGitWrite   Capability = "git-write"
	CapabilityFSWrite    Capability = "fs-write"
	CapabilityBoardWrite Capability = "board-write"
	CapabilityShellExec  Capability = "shell-exec"
)

func validCapability(c Capability) bool {
	switch c {
	case CapabilityNetwork, CapabilityGitWrite, CapabilityFSWrite, CapabilityBoardWrite, CapabilityShellExec:
		return true
	default:
		return false
	}
}

type Config struct {
	Version      string        `yaml:"version"`
	Project      ProjectConfig `yaml:"project"`
	TaskProvider TaskProvider  `yaml:"task_provider"`
	Fleet        FleetConfig   `yaml:"fleet,omitempty"`
	Lanes        []LaneDef     `yaml:"lanes"`
	Verification Verification  `yaml:"verification,omitempty"`
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
	Name      string `yaml:"name"`
	Role      string `yaml:"role,omitempty"`
	AgentKind string `yaml:"agent_kind"`
	Harness   string `yaml:"harness,omitempty"`
	Prompt    string `yaml:"prompt"`
	Worktree  string `yaml:"worktree,omitempty"`
	Provider  string `yaml:"provider,omitempty"`
	Model     string `yaml:"model,omitempty"`
	// FallbackModels are tried in order when Model probes unavailable
	// (quota exhausted / no payment method). The first that probes healthy
	// launches the lane, so a spent surface fails over instead of silently
	// whiffing every build.
	FallbackModels []string           `yaml:"fallback_models,omitempty"`
	Route          *RouteShape        `yaml:"route,omitempty"`
	Risk           *RiskClass         `yaml:"risk,omitempty"`
	MaxInput       *int               `yaml:"max_input_tokens,omitempty"`
	MaxOutput      *int               `yaml:"max_output_tokens,omitempty"`
	Network        *NetworkCapability `yaml:"network,omitempty"`
	// Standing marks this lane as a control-plane role that `herd standing`
	// raises and keeps alive. Task roles (worker/reviewer/verification-gate,
	// …) leave this false/omitted and are launched ephemerally per dispatch.
	Standing bool `yaml:"standing,omitempty"`
	// Authority is this lane's repository write authority (read|write).
	// Optional for backward compatibility; validated when present.
	Authority Authority `yaml:"authority,omitempty"`
	// Capabilities are the tool/network requirements this lane's launch
	// surface must satisfy (probe-gated); values must come from the known
	// Capability vocabulary.
	Capabilities []Capability `yaml:"capabilities,omitempty"`
	// IncompatibleWith lists role labels this lane's role must never be
	// launched as/alongside for the same task (e.g. an author role listing
	// the reviewer role). Each value must match a role declared by some
	// lane in this same roster.
	IncompatibleWith []string `yaml:"incompatible_with,omitempty"`
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
	roles := make(map[string]bool, len(c.Lanes))
	for _, lane := range c.Lanes {
		if lane.Role != "" {
			roles[lane.Role] = true
		}
	}
	standingOwners := make(map[string]string, len(c.Lanes))

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
		if lane.Prompt == "" {
			return fmt.Errorf("lanes[%d]: missing required field: prompt", i)
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
		if lane.Authority != "" {
			switch lane.Authority {
			case AuthorityRead, AuthorityWrite:
			default:
				return fmt.Errorf("lanes[%d]: invalid authority %q", i, lane.Authority)
			}
		}
		for _, capability := range lane.Capabilities {
			if !validCapability(capability) {
				return fmt.Errorf("lanes[%d]: unknown capability %q", i, capability)
			}
		}
		for _, incompatible := range lane.IncompatibleWith {
			if !roles[incompatible] {
				return fmt.Errorf("lanes[%d]: incompatible_with references unknown role %q", i, incompatible)
			}
		}
		if lane.Standing && lane.Role != "" {
			if owner, ok := standingOwners[lane.Role]; ok {
				return fmt.Errorf("lanes[%d]: duplicate standing owner for role %q (already owned by lane %q)", i, lane.Role, owner)
			}
			standingOwners[lane.Role] = lane.Name
		}
	}
	return nil
}

var (
	DefaultConfigPath = filepath.Join(".herd", "herd.yaml")
	DefaultHerdDir    = ".herd"
)
