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

type TabInfo struct {
	ID    string
	Label string
	Pane  PaneInfo
}

type PaneInfo struct {
	ID    string
	TabID string
}

// Tab creates a new tab in the specified workspace and returns the tab + root pane.
func Tab(workspaceID, label string, noFocus bool) (*TabInfo, error) {
	args := []string{"tab", "create", "--workspace", workspaceID, "--label", label}
	if noFocus {
		args = append(args, "--no-focus")
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
		Pane: PaneInfo{
			ID:    resp.Result.RootPane.PaneID,
			TabID: resp.Result.RootPane.TabID,
		},
	}, nil
}

// AgentStart starts an agent in the specified pane.
// Newly created tabs may need a brief moment before the pane shell is ready;
// we sleep and retry once if herdr reports agent_pane_busy.
func AgentStart(name, kind string, paneID string) error {
	// small delay to let the pane shell initialize
	time.Sleep(500 * time.Millisecond)

	output, err := runHerdr("agent", "start", name, "--kind", kind, "--pane", paneID)
	if err != nil {
		// retry once on pane-busy
		if strings.Contains(string(output), "agent_pane_busy") {
			time.Sleep(1 * time.Second)
			output, err = runHerdr("agent", "start", name, "--kind", kind, "--pane", paneID)
		}
		if err != nil {
			return fmt.Errorf("herdr agent start: %s: %w", output, err)
		}
	}
	return nil
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

func runHerdr(args ...string) (string, error) {
	cmd := exec.Command(herdrCLI, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stderr.String()), err
	}
	return stdout.String(), nil
}
