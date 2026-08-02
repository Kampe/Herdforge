package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceEntry is one herdr workspace.
type WorkspaceEntry struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
}

// WorkspaceList returns all herdr workspaces.
func WorkspaceList() ([]WorkspaceEntry, error) {
	output, err := runHerdr("workspace", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr workspace list: %w", err)
	}
	var resp struct {
		Result struct {
			Workspaces []WorkspaceEntry `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parsing workspace list: %w", err)
	}
	return resp.Result.Workspaces, nil
}

// ResolveWorkspace ports bin/herd-ws: HERD_WORKSPACE env wins, else the
// workspace whose label matches the repo directory name (case-insensitive),
// else the focused workspace. The old behavior — a hardcoded "wF" at every
// Tab call site — broke every deployment that wasn't this exact machine.
func ResolveWorkspace(repoRoot string) string {
	if ws := os.Getenv("HERD_WORKSPACE"); ws != "" {
		return ws
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		abs = repoRoot
	}
	repoName := filepath.Base(abs)

	entries, err := WorkspaceList()
	if err != nil {
		return "wF" // ponytail: legacy fallback so a headless herdr outage degrades to old behavior
	}
	return PickWorkspace(entries, repoName)
}

// PickWorkspace is the pure selection: label match beats focused beats first.
func PickWorkspace(entries []WorkspaceEntry, repoName string) string {
	var focused, first string
	for _, e := range entries {
		if e.WorkspaceID == "" {
			continue
		}
		if strings.EqualFold(e.Label, repoName) {
			return e.WorkspaceID
		}
		if e.Focused && focused == "" {
			focused = e.WorkspaceID
		}
		if first == "" {
			first = e.WorkspaceID
		}
	}
	if focused != "" {
		return focused
	}
	if first != "" {
		return first
	}
	return "wF"
}
