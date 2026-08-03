package herdr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
)

const (
	herdrCLI       = "herdr"
	herdforgeLabel = "Herdforge · "
)

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
// Labels are auto-prefixed with "Herdforge · " when missing (FAC-141).
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
// Labels lacking the "Herdforge · " prefix are auto-prefixed (FAC-141).
func TabCreate(opts TabCreateOptions) (*TabInfo, error) {
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, fmt.Errorf("herdr tab create: workspace is required (no hardcoded fallback)")
	}
	opts.Label = EnsureHerdforgeLabel(opts.Label)
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
	// Raw starts are the incident path. There is no trustworthy role, shape,
	// provider, effort, or decision provenance in this API, so it can never
	// create a process. The failed receipt records the attempted raw request.
	return launch.Validate(launch.Request{}, nil)
}

// AgentStartWithDecision is the direct Herdr adapter. Validation happens
// before the process API is invoked, including for recovery/rescue callers.
func AgentStartWithDecision(name, kind, paneID string, req launch.Request) error {
	req.Name, req.PaneID = name, paneID
	if err := launch.Validate(req, nil); err != nil {
		return err
	}
	if req.Decision == nil || strings.TrimSpace(kind) != strings.TrimSpace(req.Decision.Provider) {
		return launch.RecordRejected(req, nil, fmt.Sprintf("herdr kind %q does not match decision provider", kind))
	}
	if req.Decision == nil || len(req.Decision.Argv) == 0 {
		return fmt.Errorf("launch decision argv is empty")
	}
	if err := agentStartProcess(name, kind, paneID, req.Decision.Argv[1:]...); err != nil {
		_ = launch.RecordRejected(req, nil, err.Error())
		return err
	}
	if err := launch.RecordStarted(req, nil); err != nil {
		cleanupErr := compensateStartedProcess(name)
		if cleanupErr != nil {
			return fmt.Errorf("launch receipt failed: %w; compensating unaccounted process failed: %v", err, cleanupErr)
		}
		return fmt.Errorf("launch receipt failed: %w; process stopped", err)
	}
	return nil
}

func compensateStartedProcess(name string) error {
	agents, err := AgentList()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.Name != name {
			continue
		}
		if a.TabID == "" {
			return fmt.Errorf("cannot compensate launch %q: missing tab id", name)
		}
		if err := TabClose(a.TabID); err != nil {
			return err
		}
		remaining, err := AgentList()
		if err != nil {
			return fmt.Errorf("verify compensated launch %q: %w", name, err)
		}
		for _, live := range remaining {
			if live.Name == name {
				return fmt.Errorf("compensated launch %q remains present", name)
			}
		}
		return nil
	}
	return fmt.Errorf("cannot compensate launch %q: %w", name, ErrAgentNotFound)
}

func agentStartProcess(name, kind, paneID string, agentArgs ...string) error {
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

var (
	ErrAgentNotFound         = errors.New("herdr agent not found")
	ErrAgentIdentityMismatch = errors.New("herdr agent identity mismatch")
)

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
	return "", fmt.Errorf("no standing agent named '%s' found: %w", name, ErrAgentNotFound)
}

// ResolveAgentTabWithDecision is the standing/resume trust boundary. A newly
// computed route never proves an existing process: herdr must report the same
// durable role, task identity, lease, provider, model, effort, and shape.
func ResolveAgentTabWithDecision(name string, req launch.Request, leaseGeneration int64) (string, error) {
	if err := launch.Validate(req, nil); err != nil {
		return "", err
	}
	if req.Decision == nil {
		return "", fmt.Errorf("resume requires a routed decision")
	}
	agents, err := AgentList()
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Name != name {
			continue
		}
		req.Name, req.PaneID, req.LeaseGeneration = name, a.PaneID, leaseGeneration
		ok, err := launch.HasStarted(req)
		if err != nil {
			return "", fmt.Errorf("resume lifecycle lookup: %w", err)
		}
		if !ok {
			return "", fmt.Errorf("standing agent %q has no matching durable launch identity: %w", name, ErrAgentIdentityMismatch)
		}
		return name, nil
	}
	return "", fmt.Errorf("no standing agent named '%s' found: %w", name, ErrAgentNotFound)
}

// EnsureHerdforgeLabel prefixes the label with "Herdforge · " if it does not
// already start with that prefix. HasPrefix (not Contains) is required so a
// mid-string match such as "review of Herdforge · thing" still gets prefixed.
func EnsureHerdforgeLabel(label string) string {
	if strings.HasPrefix(label, herdforgeLabel) {
		return label
	}
	return herdforgeLabel + label
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
