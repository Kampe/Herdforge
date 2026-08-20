// Package standing raises and reports declaratively configured standing
// control roles (FAC-91).
//
// herd standing loads standing lanes from configuration in roster order,
// validates prompt path, authority, role label, capabilities, and isolated
// worktree cwd, resolves workspace from live evidence (no hardcoded ID),
// and raises each role at most once while the agent name is held.
//
// Modes: raise (default), dry-run, status, shutdown. Ephemeral task workers
// are never selected or closed by this package.
package standing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/goalguard"
	"github.com/Kampe/Herdforge/pkg/kick"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// ForgePrefix is the live herdr agent name prefix shared with pkg/kick.
const ForgePrefix = kick.ForgePrefix

// Mode selects the standing operation.
type Mode int

const (
	// ModeRaise creates missing standing owners (default).
	ModeRaise Mode = iota
	// ModeDryRun plans raise without herdr side effects.
	ModeDryRun
	// ModeStatus reports live vs missing standing owners.
	ModeStatus
	// ModeShutdown closes only configured standing owners (not ephemeral workers).
	ModeShutdown
)

// Outcome is what happened to one standing role.
type Outcome string

const (
	OutcomeRaised      Outcome = "raised"
	OutcomeSkippedLive Outcome = "skipped_live"
	OutcomePreview     Outcome = "preview"
	OutcomeFailed      Outcome = "failed"
	OutcomeLive        Outcome = "live"
	OutcomeHeld        Outcome = "held"
	OutcomeMissing     Outcome = "missing"
	OutcomeUnraiseable Outcome = "unraiseable"
	OutcomeWouldClose  Outcome = "would_close"
	OutcomeClosed      Outcome = "closed"
	OutcomePreserved   Outcome = "preserved"
)

// Agent is the fleet subset standing needs from herdr agent list.
type Agent struct {
	Name      string
	Status    string
	PaneID    string
	TabID     string
	Workspace string
	Cwd       string
	LoopMode  LoopMode
}

// Tab is a created herdr tab.
type Tab struct {
	ID     string
	Label  string
	PaneID string
	Cwd    string
}

// Route is the admitted launch surface for one standing role.
// Decision is opaque so callers can pass *router.LaunchDecision without
// this package importing the router graph for pure validation tests.
type Route struct {
	Provider string
	Model    string
	Effort   string
	Harness  string
	Decision any
}

// Options controls one standing run. Production callers fill injectables
// with live herdr/config seams; tests inject fakes.
type Options struct {
	Mode     Mode
	Only     []string // lane names or forge-<lane>; empty = all standing
	Quiet    bool
	RepoRoot string

	ListAgents         func() ([]Agent, error)
	ResolveWorkspace   func(repoRoot string, cfg *config.Config) (string, error)
	PrepareWorktree    func(lane *config.LaneDef) error
	AdmitRoute         func(lane *config.LaneDef) (Route, error)
	CreateTab          func(workspace, label, cwd string) (Tab, error)
	StartAgent         func(tab Tab, agentName string, route Route, lane *config.LaneDef, repository string) error
	PromptAgent        func(agentName, promptText string) error
	CloseTab           func(tabID string) error
	RepositoryIdentity func(cfg *config.Config) string
	PromptReadable     func(path string) error
	AbsPath            func(path string) (string, error)
	HarnessPresent     func(harness string) bool

	// SetGoal installs the durable goal-guard.json a raised lane's Stop hook
	// checks. It runs in the lane's own worktree cwd (never the coordinator's)
	// so it resolves that lane's own state dir. Nil means no goal-guard wiring
	// (e.g. tests that don't exercise it); a non-nil error here does not fail
	// the raise, since a missing goal degrades to the Stop hook's quiet
	// no-goal path rather than blocking the agent.
	SetGoal func(cwd, lane, task, owner string) error
}

// RoleResult records one standing role's outcome.
type RoleResult struct {
	LaneName  string   `json:"lane"`
	AgentName string   `json:"agent"`
	Role      string   `json:"role"`
	CWD       string   `json:"cwd,omitempty"`
	Outcome   Outcome  `json:"outcome"`
	LoopMode  LoopMode `json:"loop_mode,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	TabID     string   `json:"tab_id,omitempty"`
	PaneID    string   `json:"pane_id,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Model     string   `json:"model,omitempty"`
}

// Result is the full standing run report.
type Result struct {
	Workspace   string       `json:"workspace"`
	Mode        string       `json:"mode"`
	Roles       []RoleResult `json:"roles"`
	Raised      int          `json:"raised"`
	Skipped     int          `json:"skipped"`
	Failed      int          `json:"failed"`
	Previewed   int          `json:"previewed"`
	Closed      int          `json:"closed"`
	Missing     int          `json:"missing"`
	Live        int          `json:"live"`
	Unraiseable int          `json:"unraiseable"`
}

// NameHeld reports whether an agent_status means the live name is held and
// a second raise must skip (shell parity: working|idle|starting|done|blocked).
func NameHeld(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "working", "idle", "starting", "done", "blocked":
		return true
	default:
		return false
	}
}

// AgentName returns the live herdr name for a configured lane.
func AgentName(laneName string) string {
	return ForgePrefix + strings.TrimSpace(laneName)
}

// AgentNameForRepository qualifies a standing lane with its stable repository
// identity. Herdr names are fleet-global, while lane names are intentionally
// shared by every repository. The short digest keeps the name readable and
// within Herdr's practical label limits without exposing the identity itself.
func AgentNameForRepository(laneName, repository string) string {
	laneName = strings.TrimSpace(laneName)
	repository = strings.TrimSpace(repository)
	if repository == "" {
		return AgentName(laneName)
	}
	sum := sha256.Sum256([]byte(repository))
	return fmt.Sprintf("%s%s-%s", ForgePrefix, laneName, hex.EncodeToString(sum[:])[:10])
}

// withContinuationGoal makes the standing contract explicit in the first
// packet delivered to a freshly raised lane. A standing lane must keep making
// useful progress when its current board card is absent or complete: inspect
// the repository and board, mint the next actionable ticket, claim it, and
// continue until an explicit stop condition is reported. The /goal marker is
// understood by the supported harnesses and is intentionally part of the
// prompt rather than an out-of-band shell command.
func withContinuationGoal(lane config.LaneDef, prompt string) string {
	goal := strings.TrimSpace(lane.GoalTemplate)
	if goal == "" {
		goal = fmt.Sprintf(`Continue as the standing %s lane. When the current assignment is complete or there is no ticket, inspect the repo and board, create the next actionable ticket with provenance, claim it, and work it in an isolated worktree. Report progress and blockers to the coordinator/review supervisor; stop only on an explicit stop, lease loss, or wind-down condition.`, strings.TrimSpace(lane.Role))
	} else {
		goal = strings.ReplaceAll(goal, "{{role}}", strings.TrimSpace(lane.Role))
		goal = strings.TrimSpace(strings.TrimPrefix(goal, "/goal"))
	}
	return strings.TrimSpace(prompt) + "\n\n" + RenderAuthorityEnvelope(AuthorityEnvelopeForLane(lane)) + "\n/goal " + goal
}

// AuthorityEnvelopeForLane builds the standing grant from repository
// configuration. The packet path is deliberately the exact configured path,
// so a lane can verify the grant from its own transcript and worktree.
func AuthorityEnvelopeForLane(lane config.LaneDef) goalguard.AuthorityEnvelope {
	return goalguard.AuthorityEnvelope{
		Grantor:          "coordinator",
		PacketPath:       filepath.Clean(lane.Prompt),
		BoundedAutonomy:  fmt.Sprintf("Improve the standing %s lane indefinitely by selecting and completing the next actionable work item; do not self-grant new authority.", strings.TrimSpace(lane.Role)),
		MutationLimits:   fmt.Sprintf("Only change files in the isolated worktree %s, using declared %s authority and capabilities.", filepath.Clean(lane.Worktree), lane.Authority),
		ForbiddenActions: []string{"push", "open or update a PR", "merge", "self-review", "change standing policy or authority"},
		StopConditions:   []string{"explicit coordinator stop", "lease loss", "wind-down", "completed goal with coordinator acknowledgement"},
	}
}

// RenderAuthorityEnvelope is the transcript-visible, human-verifiable grant.
func RenderAuthorityEnvelope(a goalguard.AuthorityEnvelope) string {
	return fmt.Sprintf("AUTHORITY ENVELOPE\n- grantor: %s\n- packet path: %s\n- bounded autonomy: %s\n- mutation limits: %s\n- forbidden actions: %s\n- stop conditions: %s\nAUTHORITY ENVELOPE END", a.Grantor, a.PacketPath, a.BoundedAutonomy, a.MutationLimits, strings.Join(a.ForbiddenActions, "; "), strings.Join(a.StopConditions, "; "))
}

func durableGoalTask(lane config.LaneDef) string {
	goal := strings.TrimSpace(lane.GoalTemplate)
	if goal == "" {
		goal = fmt.Sprintf("standing %s: continue until an explicit stop, lease loss, or wind-down condition", strings.TrimSpace(lane.Role))
	} else {
		goal = strings.ReplaceAll(goal, "{{role}}", strings.TrimSpace(lane.Role))
		goal = strings.TrimSpace(strings.TrimPrefix(goal, "/goal"))
	}
	return strings.TrimSpace(goal)
}

// StandingLanes returns standing control roles in config declaration order.
// Order is deterministic: the roster array order from herd.yaml, never sorted
// by name (kick.StandingIDs sorts agent ids for kick; raise follows config).
func StandingLanes(cfg *config.Config) []config.LaneDef {
	if cfg == nil {
		return nil
	}
	out := make([]config.LaneDef, 0, len(cfg.Lanes))
	for _, lane := range cfg.Lanes {
		if lane.Standing {
			out = append(out, lane)
		}
	}
	return out
}

// Select returns standing lanes filtered by Only. Empty Only means all.
// Each Only entry may be a lane name ("harvest") or live agent name
// ("forge-harvest"). Unknown selections fail closed. Result order follows
// the input lanes slice (config declaration order).
func Select(lanes []config.LaneDef, only []string) ([]config.LaneDef, error) {
	if len(only) == 0 {
		return append([]config.LaneDef(nil), lanes...), nil
	}
	wantLanes := map[string]struct{}{}
	for _, raw := range only {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, ForgePrefix) {
			wantLanes[strings.TrimPrefix(s, ForgePrefix)] = struct{}{}
		} else {
			wantLanes[s] = struct{}{}
		}
	}
	if len(wantLanes) == 0 {
		return nil, errors.New("standing selection is empty")
	}
	byName := make(map[string]config.LaneDef, len(lanes))
	for _, lane := range lanes {
		byName[lane.Name] = lane
	}
	for name := range wantLanes {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("unknown standing selection %q (not a configured standing lane)", name)
		}
	}
	out := make([]config.LaneDef, 0, len(wantLanes))
	for _, lane := range lanes {
		if _, ok := wantLanes[lane.Name]; ok {
			out = append(out, lane)
		}
	}
	return out, nil
}

// ValidateLane checks standing launch preconditions without side effects.
// Missing prompt, worktree, authority, capabilities, role, or a shared-root
// cwd blocks launch. Capability *probe* enforcement remains FAC-139; this
// gate requires the declarative fields FAC-127 registered on the lane.
func ValidateLane(lane config.LaneDef, repoRoot string, promptReadable func(string) error, absPath func(string) (string, error)) (cwd string, err error) {
	name := strings.TrimSpace(lane.Name)
	if name == "" {
		return "", errors.New("standing lane missing name")
	}
	if strings.TrimSpace(lane.Role) == "" {
		return "", fmt.Errorf("standing lane %q missing role label", name)
	}
	if strings.TrimSpace(lane.Prompt) == "" {
		return "", fmt.Errorf("standing lane %q missing prompt path", name)
	}
	if promptReadable == nil {
		promptReadable = defaultPromptReadable
	}
	if err := promptReadable(lane.Prompt); err != nil {
		return "", fmt.Errorf("standing lane %q prompt %q: %w", name, lane.Prompt, err)
	}
	if strings.TrimSpace(lane.Worktree) == "" {
		return "", fmt.Errorf("standing lane %q requires an isolated worktree", name)
	}
	if lane.Authority == "" {
		return "", fmt.Errorf("standing lane %q missing authority", name)
	}
	switch lane.Authority {
	case config.AuthorityRead, config.AuthorityWrite:
	default:
		return "", fmt.Errorf("standing lane %q invalid authority %q", name, lane.Authority)
	}
	if len(lane.Capabilities) == 0 {
		return "", fmt.Errorf("standing lane %q missing capabilities (declarative route tools)", name)
	}
	for _, cap := range lane.Capabilities {
		if !validCapability(cap) {
			return "", fmt.Errorf("standing lane %q unknown capability %q", name, cap)
		}
	}
	for _, inc := range lane.IncompatibleWith {
		if strings.EqualFold(strings.TrimSpace(inc), strings.TrimSpace(lane.Role)) {
			return "", fmt.Errorf("standing lane %q incompatible_with includes its own role %q", name, lane.Role)
		}
	}
	if lane.Risk != nil {
		switch *lane.Risk {
		case config.RiskR0Mechanical, config.RiskR1Standard, config.RiskR2High, config.RiskR3Critical:
		default:
			return "", fmt.Errorf("standing lane %q invalid risk ceiling %q", name, *lane.Risk)
		}
	}

	if absPath == nil {
		absPath = filepath.Abs
	}
	if strings.TrimSpace(repoRoot) == "" {
		repoRoot = "."
	}
	absRoot, err := absPath(repoRoot)
	if err != nil {
		return "", fmt.Errorf("standing lane %q resolve repo root: %w", name, err)
	}
	// Worktree paths in herd.yaml are repo-relative (e.g. .worktrees/harvest).
	wt := lane.Worktree
	if !filepath.IsAbs(wt) {
		wt = filepath.Join(absRoot, wt)
	}
	absCwd, err := absPath(wt)
	if err != nil {
		return "", fmt.Errorf("standing lane %q resolve worktree: %w", name, err)
	}
	if err := worktree.RejectSharedRoot(absRoot, absCwd); err != nil {
		return "", fmt.Errorf("standing lane %q refuses shared-root session: %w", name, err)
	}
	// Write-capable standing owners get the same shared-root refusal with an
	// explicit authority mention for operators reading the failure.
	if lane.Authority == config.AuthorityWrite {
		if err := worktree.RejectSharedRoot(absRoot, absCwd); err != nil {
			return "", fmt.Errorf("standing lane %q write authority cannot share repository root: %w", name, err)
		}
	}
	return absCwd, nil
}

func validCapability(c config.Capability) bool {
	switch c {
	case config.CapabilityNetwork, config.CapabilityGitWrite, config.CapabilityFSWrite, config.CapabilityBoardWrite, config.CapabilityShellExec:
		return true
	default:
		return false
	}
}

func defaultPromptReadable(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return errors.New("path is a directory")
	}
	return nil
}

// indexAgents maps only authorized agents to name -> Agent for O(1) live
// lookups. Agent names are fleet-global, so name alone is not an identity:
// the live record must belong to the resolved workspace and its cwd must be
// inside the repository being operated.
func indexAgents(agents []Agent, workspace, repoRoot string) map[string][]Agent {
	idx := make(map[string][]Agent, len(agents))
	for _, a := range agents {
		if a.Name == "" || !authorizedAgent(a, workspace, repoRoot) {
			continue
		}
		idx[a.Name] = append(idx[a.Name], a)
	}
	return idx
}

// standingAgent returns the qualified owner first and adopts the legacy
// unqualified owner only when no qualified record exists. The fallback keeps
// lanes raised before repository-qualified naming from being reported missing
// or raised a second time during the naming transition.
// standingAgent resolves a lane to its live agent. Within one workspace two
// agents can share a name, so the lane's OWN configured cwd is the
// disambiguator — not containment in repoRoot, which would reject lanes whose
// worktrees legitimately sit outside the repository (FAC-503).
func standingAgent(live map[string][]Agent, laneName, repository, laneCWD string) (string, Agent, bool) {
	for _, name := range []string{AgentNameForRepository(laneName, repository), AgentName(laneName)} {
		candidates := live[name]
		if len(candidates) == 0 {
			continue
		}
		if want := strings.TrimSpace(laneCWD); want != "" {
			for _, a := range candidates {
				if samePath(a.Cwd, want) {
					return name, a, true
				}
			}
		}
		if len(candidates) == 1 {
			return name, candidates[0], true
		}
		// Ambiguous and no configured cwd matched: refuse rather than let
		// list order decide which tab's status is authoritative.
		return name, Agent{}, false
	}
	return AgentNameForRepository(laneName, repository), Agent{}, false
}

// samePath compares two paths after cleaning and resolving symlinks.
func samePath(a, b string) bool {
	ca, err := filepath.Abs(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	cb, err := filepath.Abs(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	if r, err := filepath.EvalSymlinks(ca); err == nil {
		ca = r
	}
	if r, err := filepath.EvalSymlinks(cb); err == nil {
		cb = r
	}
	return filepath.Clean(ca) == filepath.Clean(cb)
}

// authorizedAgent decides whether a live agent may satisfy THIS repository's
// lane lookup. WORKSPACE is the authority: it is what separates two repos
// sharing one control plane, and it alone correctly rejects a foreign
// workspace's identically-named lane.
//
// FAC-503: repoRoot containment is deliberately NOT a condition. A standing
// lane's worktree legitimately lives outside the repository directory —
// sibling per-lane checkouts are the normal topology — so requiring
// pathWithin(repoRoot, cwd) reported healthy lanes as missing and a raise
// against that state would duplicate them. Rejecting a real lane is worse
// than the cross-repo confusion this check was reaching for, which workspace
// scoping already prevents.
func authorizedAgent(agent Agent, workspace, repoRoot string) bool {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || strings.TrimSpace(agent.Workspace) != workspace {
		return false
	}
	return strings.TrimSpace(agent.Cwd) != ""
}

func pathWithin(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	// Resolve existing symlinks so a worktree cannot escape through an alias.
	if resolved, resolveErr := filepath.EvalSymlinks(rootAbs); resolveErr == nil {
		rootAbs = resolved
	}
	if resolved, resolveErr := filepath.EvalSymlinks(candidateAbs); resolveErr == nil {
		candidateAbs = resolved
	}
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(candidateAbs))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ""
}

// Run executes standing raise/status/shutdown/dry-run against cfg.
//
// Raise and dry-run never query the live fleet inventory until a lane has
// passed AdmitRoute (launch policy). That keeps policy failures free of
// herdr agent-list dependency — a wedged/unhealthy herdr cannot mask
// config_worker_tuple_mismatch (or any other AdmitRoute error).
func Run(cfg *config.Config, opts Options) (*Result, error) {
	if cfg == nil {
		return nil, errors.New("standing: config is required")
	}
	repoRoot := opts.RepoRoot
	if repoRoot == "" {
		repoRoot = "."
	}
	lanes, err := Select(StandingLanes(cfg), opts.Only)
	if err != nil {
		return nil, err
	}
	if len(lanes) == 0 {
		return nil, errors.New("standing: no standing control roles configured")
	}

	modeName := modeString(opts.Mode)
	result := &Result{Mode: modeName, Roles: make([]RoleResult, 0, len(lanes))}
	repository := ""
	if opts.RepositoryIdentity != nil {
		repository = opts.RepositoryIdentity(cfg)
	}

	switch opts.Mode {
	case ModeStatus, ModeShutdown:
		// Status/shutdown are inventory operations: the fleet list is the
		// subject of the command, so it is required up front.
		if opts.ListAgents == nil {
			return nil, errors.New("standing: agent list is required")
		}
		agents, err := opts.ListAgents()
		if err != nil {
			return nil, fmt.Errorf("standing: list agents: %w", err)
		}
		if opts.ResolveWorkspace == nil {
			return nil, errors.New("standing: workspace resolver is required")
		}
		ws, wsErr := opts.ResolveWorkspace(repoRoot, cfg)
		if wsErr != nil {
			return nil, fmt.Errorf("standing: workspace: %w", wsErr)
		}
		if strings.TrimSpace(ws) == "" {
			return nil, errors.New("standing: workspace resolution returned empty id")
		}
		result.Workspace = strings.TrimSpace(ws)
		live := indexAgents(agents, result.Workspace, repoRoot)
		if opts.Mode == ModeStatus {
			return runStatus(result, lanes, live, repoRoot, repository, opts)
		}
		return runShutdown(result, lanes, live, repoRoot, repository, opts)
	case ModeDryRun, ModeRaise:
		// No herdr inventory or workspace resolution until policy admits.
		return runRaise(result, cfg, lanes, repoRoot, repository, opts)
	default:
		return nil, fmt.Errorf("standing: unknown mode %d", opts.Mode)
	}
}

func modeString(m Mode) string {
	switch m {
	case ModeDryRun:
		return "dry-run"
	case ModeStatus:
		return "status"
	case ModeShutdown:
		return "shutdown"
	default:
		return "raise"
	}
}

func runStatus(result *Result, lanes []config.LaneDef, live map[string][]Agent, repoRoot, repository string, opts Options) (*Result, error) {
	var failures []error
	for i := range lanes {
		lane := lanes[i]
		agentName := AgentNameForRepository(lane.Name, repository)
		cwd, verr := ValidateLane(lane, repoRoot, opts.PromptReadable, opts.AbsPath)
		rr := RoleResult{LaneName: lane.Name, AgentName: agentName, Role: lane.Role, CWD: cwd}
		if verr != nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = verr.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", lane.Name, verr))
			result.Roles = append(result.Roles, rr)
			continue
		}
		if actualName, a, ok := standingAgent(live, lane.Name, repository, rr.CWD); ok && NameHeld(a.Status) {
			rr.AgentName = actualName
			rr.LoopMode = a.LoopMode
			if rr.LoopMode == "" {
				rr.LoopMode = LoopRunning
			}
			rr.Outcome = OutcomeLive
			if rr.LoopMode == LoopHeld || rr.LoopMode == LoopOneShot {
				rr.Outcome = OutcomeHeld
			}
			rr.Reason = "status=" + a.Status
			rr.TabID = a.TabID
			rr.PaneID = a.PaneID
			if a.Cwd != "" {
				rr.CWD = a.Cwd
			}
			result.Live++
		} else if opts.HarnessPresent != nil && strings.TrimSpace(lane.Harness) != "" && !opts.HarnessPresent(lane.Harness) {
			rr.Outcome = OutcomeUnraiseable
			rr.Reason = fmt.Sprintf("harness %q binary not found on this host — cannot be raised here", lane.Harness)
			result.Unraiseable++
		} else {
			rr.Outcome = OutcomeMissing
			rr.Reason = "not live — needs raising"
			result.Missing++
		}
		result.Roles = append(result.Roles, rr)
	}
	return result, errors.Join(failures...)
}

func runShutdown(result *Result, lanes []config.LaneDef, live map[string][]Agent, repoRoot, repository string, opts Options) (*Result, error) {
	// Exact shutdown: only configured standing agent names. Ephemeral task
	// workers (task-*, non-standing forge-*) are never selected.
	standingNames := map[string]struct{}{}
	for _, lane := range lanes {
		standingNames[AgentNameForRepository(lane.Name, repository)] = struct{}{}
	}
	var failures []error
	for _, lane := range lanes {
		agentName := AgentNameForRepository(lane.Name, repository)
		rr := RoleResult{LaneName: lane.Name, AgentName: agentName, Role: lane.Role}
		actualName, a, ok := standingAgent(live, lane.Name, repository, "")
		if !ok || a.TabID == "" {
			rr.Outcome = OutcomeMissing
			rr.Reason = "not live"
			result.Missing++
			result.Roles = append(result.Roles, rr)
			continue
		}
		rr.AgentName = actualName
		rr.TabID = a.TabID
		rr.PaneID = a.PaneID
		// Active standing agents are preserved (same safety as herd stop)
		// unless this is a pure close of settled sessions.
		if isActive(a.Status) {
			rr.Outcome = OutcomePreserved
			rr.Reason = "active standing owner preserved; not destroyed"
			result.Skipped++
			result.Roles = append(result.Roles, rr)
			continue
		}
		if opts.Mode == ModeShutdown && opts.CloseTab == nil {
			// Should not happen for execute path; treat as would-close plan.
			rr.Outcome = OutcomeWouldClose
			rr.Reason = "settled standing owner"
			result.Closed++
			result.Roles = append(result.Roles, rr)
			continue
		}
		// Dry-run of shutdown is expressed as ModeDryRun with a separate flag
		// in the CLI; when CloseTab is nil we only plan.
		if opts.CloseTab == nil {
			rr.Outcome = OutcomeWouldClose
			rr.Reason = "settled standing owner"
			result.Closed++
			result.Roles = append(result.Roles, rr)
			continue
		}
		if err := opts.CloseTab(a.TabID); err != nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = err.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s close: %w", agentName, err))
			result.Roles = append(result.Roles, rr)
			continue
		}
		if opts.ListAgents == nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = "standing owner close lacks absence verification"
			result.Failed++
			failures = append(failures, fmt.Errorf("%s close: absence verification unavailable", agentName))
			result.Roles = append(result.Roles, rr)
			continue
		}
		remaining, listErr := opts.ListAgents()
		if listErr != nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = "absence readback: " + listErr.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s close absence readback: %w", agentName, listErr))
			result.Roles = append(result.Roles, rr)
			continue
		}
		if _, stillLive := indexAgents(remaining, result.Workspace, repoRoot)[actualName]; stillLive {
			rr.Outcome = OutcomeFailed
			rr.Reason = "standing owner still present after close"
			result.Failed++
			failures = append(failures, fmt.Errorf("%s close: agent remains present", agentName))
			result.Roles = append(result.Roles, rr)
			continue
		}
		rr.Outcome = OutcomeClosed
		rr.Reason = "standing owner closed"
		result.Closed++
		result.Roles = append(result.Roles, rr)
	}
	_ = standingNames
	return result, errors.Join(failures...)
}

func isActive(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "working", "starting":
		return true
	default:
		return false
	}
}

func runRaise(result *Result, cfg *config.Config, lanes []config.LaneDef, repoRoot, repository string, opts Options) (*Result, error) {
	dry := opts.Mode == ModeDryRun
	var failures []error

	// Lazy fleet inventory: loaded only after the first successful AdmitRoute
	// so a policy-only failure never pays for (or is masked by) herdr agent list.
	var live map[string][]Agent
	liveLoaded := false
	loadLive := func() error {
		if liveLoaded {
			return nil
		}
		if opts.ListAgents == nil {
			return errors.New("standing: agent list is required")
		}
		agents, err := opts.ListAgents()
		if err != nil {
			return fmt.Errorf("standing: list agents: %w", err)
		}
		if opts.ResolveWorkspace == nil {
			return errors.New("standing: workspace resolver is required")
		}
		ws, wsErr := opts.ResolveWorkspace(repoRoot, cfg)
		if wsErr != nil {
			return fmt.Errorf("standing: workspace: %w", wsErr)
		}
		if strings.TrimSpace(ws) == "" {
			return errors.New("standing: workspace resolution returned empty id")
		}
		result.Workspace = strings.TrimSpace(ws)
		live = indexAgents(agents, result.Workspace, repoRoot)
		liveLoaded = true
		return nil
	}

	for i := range lanes {
		lane := &lanes[i]
		agentName := AgentNameForRepository(lane.Name, repository)
		rr := RoleResult{LaneName: lane.Name, AgentName: agentName, Role: lane.Role}

		// Route admission (role/shape/provider policy + live route) runs
		// before inventory, path validation, and every herdr side effect so
		// policy failures surface with their canonical errors.
		if opts.AdmitRoute == nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = "route admitter is required"
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: route admitter is required", lane.Name))
			result.Roles = append(result.Roles, rr)
			continue
		}
		route, routeErr := opts.AdmitRoute(lane)
		if routeErr != nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = "route blocked: " + routeErr.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", lane.Name, routeErr))
			result.Roles = append(result.Roles, rr)
			continue
		}
		if strings.TrimSpace(route.Provider) == "" || strings.TrimSpace(route.Model) == "" {
			rr.Outcome = OutcomeFailed
			rr.Reason = "route missing provider/model"
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: route missing provider/model", lane.Name))
			result.Roles = append(result.Roles, rr)
			continue
		}
		rr.Provider = route.Provider
		rr.Model = route.Model

		// Inventory is only needed after policy admits (idempotent skip-if-live).
		if err := loadLive(); err != nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = err.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", lane.Name, err))
			result.Roles = append(result.Roles, rr)
			continue
		}
		if actualName, a, ok := standingAgent(live, lane.Name, repository, rr.CWD); ok && NameHeld(a.Status) {
			rr.AgentName = actualName
			if strings.EqualFold(strings.TrimSpace(a.Status), "done") && opts.CloseTab != nil {
				if err := opts.CloseTab(a.TabID); err != nil {
					rr.Outcome = OutcomeFailed
					rr.Reason = "retired owner close: " + err.Error()
					result.Failed++
					failures = append(failures, fmt.Errorf("%s: %w", lane.Name, err))
					result.Roles = append(result.Roles, rr)
					continue
				}
				remaining, listErr := opts.ListAgents()
				if listErr != nil {
					rr.Outcome = OutcomeFailed
					rr.Reason = "retired owner absence readback: " + listErr.Error()
					result.Failed++
					failures = append(failures, fmt.Errorf("%s: retired owner absence readback: %w", lane.Name, listErr))
					result.Roles = append(result.Roles, rr)
					continue
				}
				if _, stillLive := indexAgents(remaining, result.Workspace, repoRoot)[agentName]; stillLive {
					rr.Outcome = OutcomeFailed
					rr.Reason = "retired owner still present after close"
					result.Failed++
					failures = append(failures, fmt.Errorf("%s: retired owner remains present", lane.Name))
					result.Roles = append(result.Roles, rr)
					continue
				}
				delete(live, actualName)
			} else {
				rr.Outcome = OutcomeSkippedLive
				rr.Reason = "already live status=" + a.Status
				rr.TabID = a.TabID
				rr.PaneID = a.PaneID
				if a.Cwd != "" {
					rr.CWD = a.Cwd
				}
				if opts.SetGoal != nil {
					// Re-raising is also a policy refresh. Set replaces the atomic
					// goal file, so a later corrective instruction cannot be shadowed
					// by the previous durable wording.
					goalCWD := rr.CWD
					if goalCWD == "" {
						goalCWD, _ = ValidateLane(*lane, repoRoot, opts.PromptReadable, opts.AbsPath)
					}
					if goalCWD != "" {
						_ = opts.SetGoal(goalCWD, lane.Name, durableGoalTask(*lane), "coordinator")
					}
				}
				result.Skipped++
				result.Roles = append(result.Roles, rr)
				continue
			}
		}

		cwd, verr := ValidateLane(*lane, repoRoot, opts.PromptReadable, opts.AbsPath)
		if verr != nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = verr.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", lane.Name, verr))
			result.Roles = append(result.Roles, rr)
			continue
		}
		rr.CWD = cwd

		if dry {
			rr.Outcome = OutcomePreview
			rr.Reason = fmt.Sprintf("would start %s/%s cwd=%s", route.Provider, route.Model, cwd)
			result.Previewed++
			result.Roles = append(result.Roles, rr)
			continue
		}

		if repository == "" {
			rr.Outcome = OutcomeFailed
			rr.Reason = "repository identity unavailable"
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: repository identity unavailable", lane.Name))
			result.Roles = append(result.Roles, rr)
			continue
		}

		if opts.PrepareWorktree != nil {
			if err := opts.PrepareWorktree(lane); err != nil {
				rr.Outcome = OutcomeFailed
				rr.Reason = "worktree: " + err.Error()
				result.Failed++
				failures = append(failures, fmt.Errorf("%s: %w", lane.Name, err))
				result.Roles = append(result.Roles, rr)
				continue
			}
		}

		if opts.SetGoal != nil {
			if err := opts.SetGoal(cwd, lane.Name, durableGoalTask(*lane), "coordinator"); err != nil {
				rr.Outcome = OutcomeFailed
				rr.Reason = "authority goal: " + err.Error()
				result.Failed++
				failures = append(failures, fmt.Errorf("%s: %w", lane.Name, err))
				result.Roles = append(result.Roles, rr)
				continue
			}
		}

		// Re-validate cwd after prepare: still not shared root, still isolated.
		if _, err := ValidateLane(*lane, repoRoot, opts.PromptReadable, opts.AbsPath); err != nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = err.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", lane.Name, err))
			result.Roles = append(result.Roles, rr)
			continue
		}

		if opts.CreateTab == nil || opts.StartAgent == nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = "herdr raise seams are required"
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: herdr raise seams are required", lane.Name))
			result.Roles = append(result.Roles, rr)
			continue
		}

		ws := result.Workspace
		if strings.TrimSpace(ws) == "" {
			if opts.ResolveWorkspace == nil {
				rr.Outcome = OutcomeFailed
				rr.Reason = "workspace resolver is required"
				result.Failed++
				failures = append(failures, fmt.Errorf("%s: workspace resolver is required", lane.Name))
				result.Roles = append(result.Roles, rr)
				continue
			}
			resolved, wsErr := opts.ResolveWorkspace(repoRoot, cfg)
			if wsErr != nil || strings.TrimSpace(resolved) == "" {
				reason := "workspace unresolved"
				if wsErr != nil {
					reason = wsErr.Error()
				}
				rr.Outcome = OutcomeFailed
				rr.Reason = reason
				result.Failed++
				failures = append(failures, fmt.Errorf("%s: %s", lane.Name, reason))
				result.Roles = append(result.Roles, rr)
				continue
			}
			ws = resolved
			result.Workspace = ws
		}

		tab, tabErr := opts.CreateTab(ws, agentName, cwd)
		if tabErr != nil {
			rr.Outcome = OutcomeFailed
			rr.Reason = "create tab: " + tabErr.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", lane.Name, tabErr))
			result.Roles = append(result.Roles, rr)
			continue
		}
		rr.TabID = tab.ID
		rr.PaneID = tab.PaneID

		if err := opts.StartAgent(tab, agentName, route, lane, repository); err != nil {
			// Partial launch: tab without a running agent must not orphan.
			if opts.CloseTab != nil && tab.ID != "" {
				if closeErr := opts.CloseTab(tab.ID); closeErr != nil {
					err = errors.Join(err, fmt.Errorf("reconcile orphan tab %s: %w", tab.ID, closeErr))
				}
			}
			rr.Outcome = OutcomeFailed
			rr.Reason = "start agent: " + err.Error()
			rr.TabID = ""
			rr.PaneID = ""
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", lane.Name, err))
			result.Roles = append(result.Roles, rr)
			continue
		}

		promptBytes, readErr := os.ReadFile(lane.Prompt)
		if readErr != nil {
			// Agent is live with a held name; closing would destroy a raised
			// owner. Report failure; status will show live for re-prompt.
			rr.Outcome = OutcomeFailed
			rr.Reason = "read prompt after start: " + readErr.Error()
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: %w", lane.Name, readErr))
			result.Roles = append(result.Roles, rr)
			continue
		}
		promptText := withContinuationGoal(*lane, string(promptBytes))
		if promptText == "" {
			rr.Outcome = OutcomeFailed
			rr.Reason = "prompt file is empty"
			result.Failed++
			failures = append(failures, fmt.Errorf("%s: prompt file is empty", lane.Name))
			result.Roles = append(result.Roles, rr)
			continue
		}
		if opts.PromptAgent != nil {
			if err := opts.PromptAgent(agentName, promptText); err != nil {
				rr.Outcome = OutcomeFailed
				rr.Reason = "prompt: " + err.Error()
				result.Failed++
				failures = append(failures, fmt.Errorf("%s: %w", lane.Name, err))
				result.Roles = append(result.Roles, rr)
				continue
			}
		}

		rr.Outcome = OutcomeRaised
		rr.Reason = fmt.Sprintf("%s/%s running", route.Provider, route.Model)
		result.Raised++
		// Reflect into live index so a second selection in the same run
		// cannot double-raise the same name (repeated raise safety).
		live[agentName] = []Agent{{Name: agentName, Status: "starting", TabID: tab.ID, PaneID: tab.PaneID, Cwd: cwd, Workspace: result.Workspace}}
		result.Roles = append(result.Roles, rr)
	}

	return result, errors.Join(failures...)
}

// Summary returns a one-line human report.
func Summary(r *Result) string {
	if r == nil {
		return "herd-standing: empty result"
	}
	switch r.Mode {
	case "status":
		return fmt.Sprintf("herd-standing: status workspace=%s live=%d missing=%d unraiseable=%d failed=%d",
			r.Workspace, r.Live, r.Missing, r.Unraiseable, r.Failed)
	case "shutdown":
		return fmt.Sprintf("herd-standing: shutdown workspace=%s closed=%d preserved=%d missing=%d failed=%d",
			r.Workspace, r.Closed, r.Skipped, r.Missing, r.Failed)
	case "dry-run":
		return fmt.Sprintf("herd-standing: DRY done workspace=%s previewed=%d skipped=%d failed=%d (nothing raised)",
			r.Workspace, r.Previewed, r.Skipped, r.Failed)
	default:
		return fmt.Sprintf("herd-standing: done workspace=%s started=%d skipped=%d failed=%d",
			r.Workspace, r.Raised, r.Skipped, r.Failed)
	}
}
