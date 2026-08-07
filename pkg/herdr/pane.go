package herdr

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PaneEntry is one pane from `herdr pane list`.
// Fields beyond the original identity set match live herdr 0.7.x rows used
// by rescue diagnosis (focus, cwd, geometry scroll hints).
type PaneEntry struct {
	PaneID        string `json:"pane_id,omitempty"`
	TabID         string `json:"tab_id,omitempty"`
	Workspace     string `json:"workspace_id,omitempty"`
	Name          string `json:"name,omitempty"`
	Agent         string `json:"agent,omitempty"`
	AgentStatus   string `json:"agent_status,omitempty"`
	Focused       bool   `json:"focused"`
	Cwd           string `json:"cwd,omitempty"`
	ForegroundCwd string `json:"foreground_cwd,omitempty"`
	TerminalID    string `json:"terminal_id,omitempty"`
	TerminalTitle string `json:"terminal_title,omitempty"`
}

// PaneRect is a layout rectangle from `herdr pane layout`.
type PaneRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// LayoutPane is one pane inside a PaneLayout.
type LayoutPane struct {
	PaneID  string   `json:"pane_id,omitempty"`
	Focused bool     `json:"focused"`
	Rect    PaneRect `json:"rect"`
}

// PaneLayout is the structured geometry for one tab from `herdr pane layout`.
type PaneLayout struct {
	TabID         string       `json:"tab_id,omitempty"`
	Workspace     string       `json:"workspace_id,omitempty"`
	FocusedPaneID string       `json:"focused_pane_id,omitempty"`
	Zoomed        bool         `json:"zoomed"`
	Area          PaneRect     `json:"area"`
	Panes         []LayoutPane `json:"panes"`
	// SplitCount is the number of split nodes herdr reports. Non-zero means
	// the tab is not a single full-pane agent surface.
	SplitCount int `json:"-"`
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
	if resp.Result.Panes == nil {
		return nil, fmt.Errorf("herdr pane list returned no panes inventory")
	}
	return resp.Result.Panes, nil
}

// GetPaneLayout returns geometry for the tab containing paneID.
func GetPaneLayout(paneID string) (PaneLayout, error) {
	if strings.TrimSpace(paneID) == "" {
		return PaneLayout{}, fmt.Errorf("herdr pane layout: pane id is required")
	}
	output, err := runHerdr("pane", "layout", "--pane", paneID)
	if err != nil {
		return PaneLayout{}, fmt.Errorf("herdr pane layout %s: %w", paneID, err)
	}
	var resp struct {
		Result struct {
			Layout struct {
				PaneLayout
				Splits []json.RawMessage `json:"splits"`
			} `json:"layout"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return PaneLayout{}, fmt.Errorf("parsing pane layout: %s: %w", output, err)
	}
	layout := resp.Result.Layout.PaneLayout
	layout.SplitCount = len(resp.Result.Layout.Splits)
	return layout, nil
}

// RectFor returns the layout rect for paneID inside this layout, if present.
func (l PaneLayout) RectFor(paneID string) (PaneRect, bool) {
	for _, p := range l.Panes {
		if p.PaneID == paneID {
			return p.Rect, true
		}
	}
	return PaneRect{}, false
}

// PaneProcessInfo exposes the pane's foreground processes to callers outside
// this package (stall detection needs the agent's real cwd).
//
// Argv is whatever herdr reported and may be empty. Callers that need argv
// should use PaneProcessArgv rather than paying for the OS read here: stall
// detection reads Cwd on a hot path and has no use for it.
func PaneProcessInfo(paneID string) ([]PaneProcess, error) { return paneProcesses(paneID) }

// PaneProcessArgv is PaneProcessInfo with argv filled in from the OS for any
// process herdr reported without one.
//
// The second return is the per-PID read failures, not a fatal error: a process
// that exited between the inventory and the read is ordinary, and the
// surviving entries are still good. Callers that treat argv as evidence must
// decide for themselves what a gap means — silently returning argv-less
// processes is what makes a reader believe it saw everything.
func PaneProcessArgv(paneID string) ([]PaneProcess, []error) {
	processes, err := paneProcesses(paneID)
	if err != nil {
		return nil, []error{err}
	}
	return hydratePaneProcesses(processes)
}

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
