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
