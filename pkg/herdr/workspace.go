package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
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

// resolveWorkspace is the pure selection kernel (FAC-141).
// Precedence: envVal > configVal > label-match > focused > first.
// Empty env+config falls through to PickWorkspace (legacy "wF" when empty).
func resolveWorkspace(envVal, configVal string, entries []WorkspaceEntry, repoName string) string {
	if envVal != "" {
		return envVal
	}
	if configVal != "" {
		return configVal
	}
	return PickWorkspace(entries, repoName)
}

// ResolveWorkspace ports bin/herd-ws: HERD_WORKSPACE env wins, else the
// workspace whose label matches the repo directory name (case-insensitive),
// else the focused workspace. Legacy callers still receive "wF" when nothing
// matches — Prefer RequireWorkspace for FAC-121 fail-closed dispatch.
// Delegates to ResolveWorkspaceWithConfig with a nil config.
func ResolveWorkspace(repoRoot string) string {
	return ResolveWorkspaceWithConfig(repoRoot, nil)
}

// ResolveWorkspaceWithConfig extends ResolveWorkspace with config awareness.
// Precedence: HERD_WORKSPACE env > config.Fleet.HerdrWorkspace > label-match > focused > first.
func ResolveWorkspaceWithConfig(repoRoot string, cfg *config.Config) string {
	envVal := os.Getenv("HERD_WORKSPACE")
	var configVal string
	if cfg != nil {
		configVal = cfg.Fleet.HerdrWorkspace
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		abs = repoRoot
	}
	repoName := filepath.Base(abs)
	entries, err := WorkspaceList()
	if err != nil {
		entries = nil
	}
	return resolveWorkspace(envVal, configVal, entries, repoName)
}

// RequireWorkspace resolves a herdr workspace or returns an error.
// Never falls back to a hardcoded workspace ID (FAC-121). Order:
//  1. HERD_WORKSPACE env (must be non-empty)
//  2. label match against repo directory name
//  3. focused workspace from the live list
//  4. error — unknown workspace is a hard failure
// Config fleet.herdr_workspace is intentionally not consulted here: fail-closed
// dispatch must not silently adopt a soft config override without env/list proof.
func RequireWorkspace(repoRoot string) (string, error) {
	if ws := strings.TrimSpace(os.Getenv("HERD_WORKSPACE")); ws != "" {
		return ws, nil
	}
	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		abs = repoRoot
	}
	repoName := filepath.Base(abs)

	entries, err := WorkspaceList()
	if err != nil {
		return "", fmt.Errorf("herdr workspace resolution failed: %w", err)
	}
	id, ok := PickWorkspaceStrict(entries, repoName)
	if !ok {
		return "", fmt.Errorf("herdr workspace unknown for repo %q: set HERD_WORKSPACE or label a workspace; refusing hardcoded fallback", repoName)
	}
	return id, nil
}

// PickWorkspace is the pure selection: label match beats focused beats first.
// Empty list returns the legacy "wF" for ResolveWorkspace compatibility.
func PickWorkspace(entries []WorkspaceEntry, repoName string) string {
	id, ok := PickWorkspaceStrict(entries, repoName)
	if !ok {
		return "wF"
	}
	return id
}

// PickWorkspaceStrict is the fail-closed pure selector (FAC-121).
// Returns ok=false when no workspace can be identified — never invents "wF".
func PickWorkspaceStrict(entries []WorkspaceEntry, repoName string) (string, bool) {
	var focused, first string
	for _, e := range entries {
		if e.WorkspaceID == "" {
			continue
		}
		if strings.EqualFold(e.Label, repoName) {
			return e.WorkspaceID, true
		}
		if e.Focused && focused == "" {
			focused = e.WorkspaceID
		}
		if first == "" {
			first = e.WorkspaceID
		}
	}
	if focused != "" {
		return focused, true
	}
	if first != "" {
		return first, true
	}
	return "", false
}
