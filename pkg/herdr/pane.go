package herdr

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PaneEntry is one pane from `herdr pane list`.
type PaneEntry struct {
	PaneID      string `json:"pane_id,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	Workspace   string `json:"workspace_id,omitempty"`
	Name        string `json:"name,omitempty"`
	AgentStatus string `json:"agent_status,omitempty"`
}

// PaneList returns every pane herdr knows about.
func PaneList() ([]PaneEntry, error) {
	output, err := runHerdr("pane", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr pane list: %w", err)
	}
	var resp struct {
		Result struct {
			Panes []PaneEntry `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parsing pane list output: %s: %w", output, err)
	}
	return resp.Result.Panes, nil
}

// PaneProcessInfo exposes the pane's foreground processes to callers outside
// this package (stall detection needs the agent's real cwd).
func PaneProcessInfo(paneID string) ([]PaneProcess, error) { return paneProcesses(paneID) }

// PaneRead returns recent unwrapped pane output. Unwrapped matters for stall
// detection: reflowed text changes with terminal width, which would make a
// frozen pane fingerprint differently after a resize.
func PaneRead(paneID string, lines int) (string, error) {
	if strings.TrimSpace(paneID) == "" {
		return "", fmt.Errorf("herdr pane read: pane id is required")
	}
	if lines <= 0 {
		lines = 80
	}
	out, err := runHerdr("pane", "read", paneID,
		"--source", "recent-unwrapped", "--lines", fmt.Sprint(lines))
	if err != nil {
		return "", fmt.Errorf("herdr pane read %s: %w", paneID, err)
	}
	// The payload may be a JSON envelope or raw text depending on herdr
	// version; treat an unparseable body as raw rather than failing, since a
	// missing tail only degrades detection instead of breaking it.
	var resp struct {
		Result struct {
			Text  string   `json:"text"`
			Lines []string `json:"lines"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(out), &resp) == nil {
		if resp.Result.Text != "" {
			return resp.Result.Text, nil
		}
		if len(resp.Result.Lines) > 0 {
			return strings.Join(resp.Result.Lines, "\n"), nil
		}
	}
	return out, nil
}

// PaneClose closes one pane. Callers must have already established that the
// pane holds no agent: this does not re-check.
func PaneClose(paneID string) error {
	if strings.TrimSpace(paneID) == "" {
		return fmt.Errorf("herdr pane close: pane id is required")
	}
	if _, err := runHerdr("pane", "close", paneID); err != nil {
		return fmt.Errorf("herdr pane close %s: %w", paneID, err)
	}
	return nil
}

// PaneMoveToNewTab moves a pane into its own full tab. One agent per tab is the
// fleet invariant: a split pane makes an agent's output unreadable and its pane
// ID ambiguous to every delivery path.
func PaneMoveToNewTab(paneID, label string) error {
	if strings.TrimSpace(paneID) == "" {
		return fmt.Errorf("herdr pane move: pane id is required")
	}
	args := []string{"pane", "move", paneID, "--new-tab", "--no-focus"}
	if label = strings.TrimSpace(label); label != "" {
		args = append(args, "--label", EnsureHerdforgeLabel(label))
	}
	if _, err := runHerdr(args...); err != nil {
		return fmt.Errorf("herdr pane move %s: %w", paneID, err)
	}
	return nil
}
