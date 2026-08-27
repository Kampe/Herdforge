package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
)

// WorkspaceEntry is one herdr workspace.
type WorkspaceEntry struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Name        string `json:"name,omitempty"`
	Focused     bool   `json:"focused"`
	// Cwd is the workspace's working directory, when herdr reports one.
	// Absent from every fixture seen so far in this repo; consumers that
	// match on it (e.g. pkg/feedback's workspace cascade) must treat an
	// empty value as "no cwd match available", never as a match.
	Cwd string `json:"cwd,omitempty"`
}

// WorkspaceDrift is a read-only finding from the live agent/workspace audit.
// It intentionally contains no remediation operation: moving or closing a
// stranded tab requires an operator or coordinator decision.
type WorkspaceDrift struct {
	Agent             string
	Workspace         string
	ExpectedWorkspace string
	ForegroundCwd     string
}

// AuditWorkspaceDrift identifies agents whose foreground cwd belongs to a
// repository registered for a different Herdr workspace. Repositories with a
// local fleet binding are authoritative even when the cwd is a task
// worktree. For repositories without a binding, a workspace label matching
// the cwd basename is sufficient evidence of a healthy placement; unknown
// layouts are ignored to avoid false positives.
func AuditWorkspaceDrift(agents []AgentEntry, workspaces []WorkspaceEntry) []WorkspaceDrift {
	findings := make([]WorkspaceDrift, 0)
	for _, agent := range agents {
		workspace := strings.TrimSpace(agent.Workspace)
		cwd := strings.TrimSpace(agent.ForegroundCwd)
		if workspace == "" || cwd == "" {
			continue
		}
		expected, known := registeredWorkspaceForCwd(cwd)
		if !known {
			expected, known = workspaceForCwdLabel(cwd, workspaces)
		}
		if !known || workspace == expected {
			continue
		}
		findings = append(findings, WorkspaceDrift{
			Agent:             driftIdentity(agent),
			Workspace:         workspace,
			ExpectedWorkspace: expected,
			ForegroundCwd:     cwd,
		})
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Agent != findings[j].Agent {
			return findings[i].Agent < findings[j].Agent
		}
		return findings[i].ForegroundCwd < findings[j].ForegroundCwd
	})
	return findings
}

func registeredWorkspaceForCwd(cwd string) (string, bool) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", false
	}
	for dir := abs; ; dir = filepath.Dir(dir) {
		configPath := config.PathFor(dir)
		if _, statErr := os.Stat(configPath); statErr == nil {
			cfg, loadErr := config.LoadConfig(configPath)
			if loadErr != nil {
				return "", false
			}
			workspace := strings.TrimSpace(cfg.Fleet.HerdrWorkspace)
			return workspace, workspace != ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", false
}

func workspaceForCwdLabel(cwd string, workspaces []WorkspaceEntry) (string, bool) {
	name := filepath.Base(filepath.Clean(cwd))
	for _, workspace := range workspaces {
		label := workspace.Label
		if strings.TrimSpace(label) == "" {
			label = workspace.Name
		}
		if workspace.WorkspaceID != "" && strings.EqualFold(strings.TrimSpace(label), name) {
			return workspace.WorkspaceID, true
		}
	}
	return "", false
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

// workspaceInList reports every live workspace id and whether want is among
// them. Extracted so the staleness rule is testable without a live herdr.
func workspaceInList(entries []WorkspaceEntry, want string) ([]string, bool) {
	live := make([]string, 0, len(entries))
	found := false
	for _, e := range entries {
		live = append(live, e.WorkspaceID)
		if e.WorkspaceID == want {
			found = true
		}
	}
	return live, found
}

// RequireWorkspace resolves a herdr workspace or returns an error.
// Never falls back to a hardcoded workspace ID (FAC-121). Order:
//  1. HERD_WORKSPACE env (must be non-empty)
//  2. label match against repo directory name
//  3. focused workspace from the live list
//  4. error — unknown workspace is a hard failure
//
// Config fleet.herdr_workspace is intentionally not consulted here: fail-closed
// dispatch must not silently adopt a soft config override without env/list proof.
func RequireWorkspace(repoRoot string) (string, error) {
	envWS := strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
	registeredWS, configErr := registeredWorkspace(repoRoot)
	if configErr != nil {
		return "", configErr
	}
	if registeredWS != "" {
		if envWS != "" && envWS != registeredWS {
			return "", fmt.Errorf("herdr workspace mismatch for repo %q: HERD_WORKSPACE=%q, registered workspace=%q; refusing cross-workspace mutation", filepath.Base(repoRoot), envWS, registeredWS)
		}
		// FAC-648: a REGISTERED workspace is exactly as capable of being stale as
		// an exported one, and until now only the export was checked.
		//
		// Twenty-five lines below, the env path calls a stale export "silent
		// poison" and validates it against the live list before honouring it. This
		// path returned the config value with no liveness check at all, so a host
		// profile naming a workspace that no longer exists routed every dispatch
		// at a dead id. Measured live on the review host: the profile named w3,
		// herdr had only w1 and w4, and every remote review launch died with
		// workspace_not_found while the host looked merely idle.
		//
		// A list we cannot read is not proof of absence, so a failed WorkspaceList
		// still honours the registered value -- refusing there would break every
		// command whenever herdr is down. Only a readable list that does NOT
		// contain the id is proof, and that fails closed and names both sides.
		if entries, listErr := WorkspaceList(); listErr == nil && len(entries) > 0 {
			live, found := workspaceInList(entries, registeredWS)
			if !found {
				return "", fmt.Errorf("registered herdr workspace %q for repo %q does not exist; live workspaces are %v. "+
					"A host profile naming a dead workspace routes every dispatch into the void and reads as an idle fleet; "+
					"update fleet.herdr_workspace in this host's config",
					registeredWS, filepath.Base(repoRoot), live)
			}
		}
		return registeredWS, nil
	}

	abs, err := filepath.Abs(repoRoot)
	if err != nil {
		abs = repoRoot
	}
	repoName := filepath.Base(abs)

	entries, err := WorkspaceList()
	if err != nil {
		// No live list to check against: an explicit env value is all we have.
		if envWS != "" {
			return envWS, nil
		}
		return "", fmt.Errorf("herdr workspace resolution failed: %w", err)
	}

	// An explicit HERD_WORKSPACE wins unconditionally — chainseer
	// herd_resolve_workspace returns it without checking, and that escape hatch
	// has to keep working (a workspace can legitimately be named before it
	// exists). But a STALE export is silent poison: a long-lived shell profile
	// outliving the workspace it names routes the whole fleet at a dead ID,
	// every agent list comes back empty, and the fleet reads as idle when it is
	// really being addressed into the void. Honour it, but say so loudly.
	if envWS != "" {
		for _, e := range entries {
			if e.WorkspaceID == envWS {
				if id, ok := PickWorkspaceStrict(entries, repoName); ok && id != envWS {
					return "", fmt.Errorf("herdr workspace mismatch for repo %q: HERD_WORKSPACE=%q, registered workspace=%q; refusing cross-workspace mutation", repoName, envWS, id)
				}
				return envWS, nil
			}
		}
		hint := ""
		if id, ok := PickWorkspaceStrict(entries, repoName); ok {
			if id != envWS {
				return "", fmt.Errorf("herdr workspace mismatch for repo %q: HERD_WORKSPACE=%q, registered workspace=%q; refusing cross-workspace mutation", repoName, envWS, id)
			}
			hint = fmt.Sprintf("; this repo labels to %s", id)
		}
		fmt.Fprintf(os.Stderr, "herdr: WARN HERD_WORKSPACE=%s is not in the live workspace list%s — the fleet will look empty (unset the stale export or correct it)\n", envWS, hint)
		return envWS, nil
	}
	id, ok := PickWorkspaceStrict(entries, repoName)
	if !ok {
		return "", fmt.Errorf("herdr workspace unknown for repo %q: set HERD_WORKSPACE or label a workspace; refusing hardcoded fallback", repoName)
	}
	return id, nil
}

// RequireCleanupWorkspace binds cleanup to every workspace identity that can
// influence the process. Cleanup is destructive, so an explicit runtime
// target, repository registration, and Herdr's selected workspace must agree
// whenever they are present. This prevents an inherited workspace from one
// checkout from turning a repo-local cleanup into a cross-repository close.
func RequireCleanupWorkspace(repoRoot string) (string, error) {
	runtimeWS := strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
	herdrWS := strings.TrimSpace(os.Getenv("HERDR_WORKSPACE_ID"))
	registeredWS, err := registeredWorkspace(repoRoot)
	if err != nil {
		return "", err
	}

	identities := []struct {
		name  string
		value string
	}{
		{"HERD_WORKSPACE", runtimeWS},
		{"registered workspace", registeredWS},
		{"HERDR_WORKSPACE_ID", herdrWS},
	}
	selectedName, selected := "", ""
	for _, identity := range identities {
		if identity.value == "" {
			continue
		}
		if selected == "" {
			selectedName, selected = identity.name, identity.value
			continue
		}
		if identity.value != selected {
			return "", fmt.Errorf("herd cleanup workspace mismatch for repo %q: %s=%q, registered workspace=%q, HERDR_WORKSPACE_ID=%q; refusing cross-workspace mutation", filepath.Base(repoRoot), selectedName, selected, registeredWS, herdrWS)
		}
	}
	if selected != "" {
		return selected, nil
	}
	return RequireWorkspace(repoRoot)
}

// registeredWorkspace reads the repository-local Herdr binding. An explicit
// environment value is only an override when it names this repository's
// registered workspace; otherwise it is ambient state from another checkout.
// A missing config or an empty binding preserves the legacy live-list
// resolution for repositories that predate the registration field.
func registeredWorkspace(repoRoot string) (string, error) {
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return "", fmt.Errorf("herdr workspace: resolve repository root: %w", err)
	}
	cfg, err := config.LoadConfig(config.PathFor(root))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		// Worktree callers may provide a root without a Herdforge config. Keep
		// resolution compatible, but never hide a malformed existing config.
		if _, statErr := os.Stat(config.PathFor(root)); statErr == nil {
			return "", fmt.Errorf("herdr workspace: load repository binding: %w", err)
		}
		return "", nil
	}
	return strings.TrimSpace(cfg.Fleet.HerdrWorkspace), nil
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

// driftIdentity returns something an operator can act on.
//
// FAC-712: this reported agent.Name, which is EMPTY for an unnamed pane -- a
// plain terminal rather than a named agent. The herd-smith lane reported this
// line on every beat for hours:
//
//	DRIFT agent= workspace=w20 expected=wB foreground_cwd=.../chainseer
//
// The drift was real and the finding was unactionable, which is exactly why it
// survived beat after beat: nothing in that line locates the pane, and an
// operator hunting for an agent by empty name is hunting for something that
// does not exist. A report that names a problem and no identity cannot be
// closed by anyone.
//
// An unnamed pane is still locatable by pane or tab id, and saying "unnamed"
// out loud stops the search for a named agent before it starts.
func driftIdentity(agent AgentEntry) string {
	if n := strings.TrimSpace(agent.Name); n != "" {
		return n
	}
	if p := strings.TrimSpace(agent.PaneID); p != "" {
		return "unnamed pane " + p
	}
	if t := strings.TrimSpace(agent.TabID); t != "" {
		return "unnamed tab " + t
	}
	// Never empty: a blank field reads as a rendering bug and sends the reader
	// looking in the wrong place.
	return "unidentified pane (no name, pane id or tab id)"
}
