package feedback

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// ErrWorkspaceUnknown is returned by ResolveWorkspace when no workspace can
// be identified by any tier of the cascade.
var ErrWorkspaceUnknown = errors.New("herd-feedback: no workspace matched")

var coordinatorPattern = regexp.MustCompile(`(?i)coordinator|orchestrator`)

// ErrCoordinatorUnresolved is returned when no live reply target exists.
var ErrCoordinatorUnresolved = errors.New("herd-feedback: no live coordinator or orchestrator")

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

func coordinatorCanReceive(a herdr.AgentEntry) bool {
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.PaneID) == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(a.Status), "idle") {
		return true
	}
	switch herdr.NormalizeTaskStatus(a.Status) {
	case "done", "unknown":
		return false
	default:
		return true
	}
}

func resolveCoordinatorTarget(agents []herdr.AgentEntry, workspace, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, a := range agents {
			if a.Workspace == workspace && strings.EqualFold(a.Name, requested) && coordinatorCanReceive(a) {
				return a.Name, nil
			}
		}
		return "", fmt.Errorf("%w: requested %q is not live in workspace %q", ErrCoordinatorUnresolved, requested, workspace)
	}
	type candidate struct {
		name string
		rank int
	}
	var candidates []candidate
	for _, a := range agents {
		if a.Workspace != workspace || !coordinatorCanReceive(a) || !coordinatorPattern.MatchString(a.Name) {
			continue
		}
		rank := 3
		switch strings.ToLower(a.Name) {
		case "forge-orchestrator":
			rank = 0
		case "orchestrator":
			rank = 1
		case "coordinator":
			rank = 2
		}
		candidates = append(candidates, candidate{name: a.Name, rank: rank})
	}
	if len(candidates) == 0 {
		return "", ErrCoordinatorUnresolved
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank < candidates[j].rank
		}
		return strings.ToLower(candidates[i].name) < strings.ToLower(candidates[j].name)
	})
	return candidates[0].name, nil
}

// CoordinatorTarget returns the live coordinator name, or empty when none
// exists. Run uses the error-returning resolver to fail closed with context.
func CoordinatorTarget(agents []herdr.AgentEntry, workspace string) string {
	got, _ := resolveCoordinatorTarget(agents, workspace, "")
	return got
}
