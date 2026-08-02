package herdr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const herdrCLI = "herdr"

// runHerdr is overridable for crash-point / unit tests (FAC-121).
var runHerdr = runHerdrReal

type TabInfo struct {
	ID    string
	Label string
	Pane  PaneInfo
	Cwd   string // process cwd requested at tab create (empty for legacy Tab)
}

type PaneInfo struct {
	ID    string
	TabID string
}

// TabCreateOptions is the fail-closed tab launch contract (FAC-121).
// Workspace and Cwd are required; unknown workspace must not fall back.
type TabCreateOptions struct {
	Workspace string
	Label     string
	Cwd       string
	NoFocus   bool
	Env       []string // optional KEY=VALUE pairs
}

// Tab creates a new tab in the specified workspace and returns the tab + root pane.
// Legacy convenience without cwd — prefer TabCreate for write-capable agents.
func Tab(workspaceID, label string, noFocus bool) (*TabInfo, error) {
	return TabCreate(TabCreateOptions{
		Workspace: workspaceID,
		Label:     label,
		NoFocus:   noFocus,
	})
}

// TabCreate creates a herdr tab with explicit workspace and optional cwd.
// When Cwd is set it is passed as --cwd so the pane process starts there
// (prompt "cd" is not isolation). Empty Workspace fails closed.
func TabCreate(opts TabCreateOptions) (*TabInfo, error) {
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, fmt.Errorf("herdr tab create: workspace is required (no hardcoded fallback)")
	}
	args := []string{"tab", "create", "--workspace", opts.Workspace, "--label", opts.Label}
	if opts.Cwd != "" {
		args = append(args, "--cwd", opts.Cwd)
	}
	if opts.NoFocus {
		args = append(args, "--no-focus")
	}
	for _, e := range opts.Env {
		if e != "" {
			args = append(args, "--env", e)
		}
	}
	output, err := runHerdr(args...)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: %w", err)
	}

	var resp struct {
		Result struct {
			Tab struct {
				TabID string `json:"tab_id"`
				Label string `json:"label"`
			} `json:"tab"`
			RootPane struct {
				PaneID string `json:"pane_id"`
				TabID  string `json:"tab_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parsing tab create output: %s: %w", output, err)
	}

	return &TabInfo{
		ID:    resp.Result.Tab.TabID,
		Label: resp.Result.Tab.Label,
		Cwd:   opts.Cwd,
		Pane: PaneInfo{
			ID:    resp.Result.RootPane.PaneID,
			TabID: resp.Result.RootPane.TabID,
		},
	}, nil
}

// TabCreateForTask is the FAC-121 launch entry: requires workspace and cwd.
// Rejects empty cwd so shared-root / unknown-directory starts cannot slip through.
func TabCreateForTask(workspaceID, label, cwd string, noFocus bool) (*TabInfo, error) {
	if strings.TrimSpace(cwd) == "" {
		return nil, fmt.Errorf("herdr tab create: cwd is required for task agents")
	}
	return TabCreate(TabCreateOptions{
		Workspace: workspaceID,
		Label:     label,
		Cwd:       cwd,
		NoFocus:   noFocus,
	})
}

// AgentStart starts an agent in the specified pane. Extra agentArgs are
// passed through to the agent executable after `--` (e.g. --model X) — a
// lane's configured model MUST reach the launch argv or the agent silently
// runs on the harness default (observed: worker lane configured for
// deepseek-v4-flash launched on the opencode default instead).
// Newly created tabs may need a brief moment before the pane shell is ready;
// we sleep and retry once if herdr reports agent_pane_busy.
func AgentStart(name, kind string, paneID string, agentArgs ...string) error {
	// small delay to let the pane shell initialize
	time.Sleep(500 * time.Millisecond)

	args := []string{"agent", "start", name, "--kind", kind, "--pane", paneID}
	if len(agentArgs) > 0 {
		args = append(args, "--")
		args = append(args, agentArgs...)
	}

	output, err := runHerdr(args...)
	// A freshly created tab's shell can take several seconds to become an
	// available target on a loaded host (observed: dispatch launch failing
	// with agent_pane_busy under swap pressure while the 1.5s retry gave up).
	// Back off up to ~12s before failing.
	for attempt := 0; err != nil && strings.Contains(string(output), "agent_pane_busy") && attempt < 6; attempt++ {
		time.Sleep(2 * time.Second)
		output, err = runHerdr(args...)
	}
	if err != nil {
		return fmt.Errorf("herdr agent start: %s: %w", output, err)
	}
	return nil
}

// LaneAgentArgs builds the launch args a lane's config demands: the
// configured model, when set, always reaches the agent argv.
func LaneAgentArgs(model string) []string {
	if model == "" {
		return nil
	}
	return []string{"--model", model}
}

// AgentPrompt sends a prompt to a running agent. If wait is true, blocks for response.
func AgentPrompt(target, text string, wait bool) (string, error) {
	args := []string{"agent", "prompt", target, text}
	if wait {
		args = append(args, "--wait")
	}
	output, err := runHerdr(args...)
	if err != nil {
		return "", fmt.Errorf("herdr agent prompt: %s: %w", output, err)
	}
	return strings.TrimSpace(output), nil
}

// IsAvailable checks whether the herdr CLI is reachable.
func IsAvailable() bool {
	_, err := exec.LookPath(herdrCLI)
	return err == nil
}

// AgentList returns all agents managed by herdr.
func AgentList() ([]AgentEntry, error) {
	output, err := runHerdr("agent", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	var resp struct {
		Result struct {
			Agents []AgentEntry `json:"agents"`
			Type   string       `json:"type"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parsing agent list: %s: %w", output, err)
	}
	return resp.Result.Agents, nil
}

// AgentEntry represents a single herdr-managed agent.
type AgentEntry struct {
	Name      string `json:"name,omitempty"`
	Kind      string `json:"agent,omitempty"`
	Status    string `json:"agent_status,omitempty"`
	PaneID    string `json:"pane_id,omitempty"`
	TabID     string `json:"tab_id,omitempty"`
	Workspace string `json:"workspace_id,omitempty"`
}

// ResolveAgentTab finds a standing agent by name and returns its tab label.
// Returns an error if no agent with that name exists.
func ResolveAgentTab(name string) (string, error) {
	agents, err := AgentList()
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Name == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("no standing agent named '%s' found", name)
}

func runHerdrReal(args ...string) (string, error) {
	cmd := exec.Command(herdrCLI, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stderr.String()), err
	}
	return stdout.String(), nil
}
