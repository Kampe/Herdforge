package feedback

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// ErrWorkspaceUnknown is returned by ResolveWorkspace when no workspace can
// be identified by any tier of the cascade.
var ErrWorkspaceUnknown = errors.New("herd-feedback: no workspace matched")

var coordinatorPattern = regexp.MustCompile(`(?i)coordinator|orchestrator`)

// ResolveWorkspace ports herd_resolve_workspace: an explicit override
// (HERD_WORKSPACE) wins unconditionally with no herdr call at all; otherwise
// the workspace list is searched for an exact case-insensitive label match
// (never a substring), then a workspace whose cwd equals or is nested under
// root; else, when exactly one workspace exists, that one. label overrides
// the repo directory basename used for the label tier (HERD_WORKSPACE_LABEL).
func ResolveWorkspace(root, label, override string, list func() ([]herdr.WorkspaceEntry, error)) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	entries, err := list()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	want := strings.TrimSpace(label)
	if want == "" {
		want = filepath.Base(abs)
	}
	for _, e := range entries {
		if e.WorkspaceID != "" && strings.EqualFold(e.Label, want) {
			return e.WorkspaceID, nil
		}
	}
	for _, e := range entries {
		if e.WorkspaceID == "" || e.Cwd == "" {
			continue
		}
		if e.Cwd == abs || strings.HasPrefix(e.Cwd, abs+string(filepath.Separator)) {
			return e.WorkspaceID, nil
		}
	}
	if len(entries) == 1 && entries[0].WorkspaceID != "" {
		return entries[0].WorkspaceID, nil
	}
	return "", ErrWorkspaceUnknown
}

// CoordinatorTarget ports herd_coordinator_target: the name of the first
// workspace agent whose name matches coordinator|orchestrator
// (case-insensitive). Empty when no agent matches. There is no pane-id
// fallback: the match requires a non-empty name, so an agent could never
// reach this point with one — the original jq `.name // .pane_id` is
// unreachable for the same reason and is not ported.
func CoordinatorTarget(agents []herdr.AgentEntry, workspace string) string {
	for _, a := range agents {
		if workspace != "" && a.Workspace != workspace {
			continue
		}
		if coordinatorPattern.MatchString(a.Name) {
			return a.Name
		}
	}
	return ""
}
