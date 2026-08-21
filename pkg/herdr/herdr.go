package herdr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

const (
	herdrCLI       = "herdr"
	herdforgeLabel = "Herdforge · "
	// herdforgeLabelPipe is a second canonical prefix already live on the
	// fleet from an older labeling path (FAC-199 acceptance explicitly
	// accepts either form). Recognized as already-correct; never written by
	// this package — new/repaired labels always use herdforgeLabel.
	herdforgeLabelPipe = "Herdforge | "

	// BinaryEnv overrides the herdr executable (tests inject a
	// protocol-faithful fake).
	BinaryEnv = "HERD_HERDR_BIN"
	// NoLiveEnv is the hermeticity guard: when set, reaching the REAL
	// herdr CLI is a hard error. Test processes set it so a production
	// fallback can never touch the operator's live fleet (FAC-145).
	NoLiveEnv = "HERD_NO_LIVE_HERDR"
)

func localDirectMode() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("HERD_MODE")))
	return mode == "local" || mode == "dev" || mode == "development"
}

// binaryPath resolves the herdr executable, honouring the test override.
func binaryPath() (string, error) {
	if override := strings.TrimSpace(os.Getenv(BinaryEnv)); override != "" {
		return override, nil
	}
	if os.Getenv(NoLiveEnv) != "" {
		return "", fmt.Errorf("refusing to reach the LIVE herdr fleet: %s is set and no %s override was provided (FAC-145 hermeticity guard)", NoLiveEnv, BinaryEnv)
	}
	return herdrCLI, nil
}

// runHerdr is overridable for crash-point / unit tests (FAC-121).
var runHerdr = runHerdrReal

// SetRunHerdrForTest replaces the CLI runner. Restore with the returned func.
func SetRunHerdrForTest(f func(args ...string) (string, error)) func() {
	old := runHerdr
	if f == nil {
		runHerdr = runHerdrReal
	} else {
		runHerdr = f
	}
	return func() { runHerdr = old }
}

var (
	ownerBindTimeout      = 20 * time.Second
	ownerBindPollInterval = 100 * time.Millisecond
)

// SetOwnerBindTimingForTest overrides bounded owner readiness polling.
func SetOwnerBindTimingForTest(timeout, poll time.Duration) func() {
	oldTimeout, oldPoll := ownerBindTimeout, ownerBindPollInterval
	ownerBindTimeout, ownerBindPollInterval = timeout, poll
	return func() { ownerBindTimeout, ownerBindPollInterval = oldTimeout, oldPoll }
}

// ToolChildLifecycle is the narrow process-tree seam used by every real
// launch. Tests install a FakeTree-backed implementation; production uses the
// exact-PID SystemTree and durable JSONL sink.
type ToolChildLifecycle interface {
	Bind(owner toolchild.Identity) error
	Invalidate(reason string) error
	VerifyTerminal() error
	Begin() error
	Reconcile(event string) error
}

var (
	toolChildMu           sync.Mutex
	toolChildByPane       = map[string]ToolChildLifecycle{}
	toolChildByTab        = map[string]ToolChildLifecycle{}
	newToolChildLifecycle = defaultToolChildLifecycle
)

func defaultToolChildLifecycle(req launch.Request, name, paneID string) (ToolChildLifecycle, error) {
	p := os.Getenv("HERD_TOOLCHILD_RECEIPTS")
	if p == "" {
		var err error
		p, err = toolchild.StableReceiptPath(req.Repository)
		if err != nil {
			return nil, err
		}
	}
	role := ""
	if req.Decision != nil {
		role = string(req.Decision.Role)
	}
	owner := toolchild.Identity{SessionGeneration: req.SessionGeneration, LaunchID: launch.DecisionDigest(req.Decision), Role: role, Lane: name}
	return toolchild.NewLifecycle(owner, toolchild.SystemTree{}, &toolchild.JSONLSink{Path: p}), nil
}

// SetToolChildLifecycleFactory is intentionally test-facing injection. A nil
// factory restores the production adapter.
func SetToolChildLifecycleFactory(f func(launch.Request, string, string) (ToolChildLifecycle, error)) func() {
	toolChildMu.Lock()
	old := newToolChildLifecycle
	if f == nil {
		newToolChildLifecycle = defaultToolChildLifecycle
	} else {
		newToolChildLifecycle = f
	}
	toolChildMu.Unlock()
	return func() { toolChildMu.Lock(); newToolChildLifecycle = old; toolChildMu.Unlock() }
}

// PrepareToolChildLifecycle reserves a durable session generation and publishes
// the provisional tool-child lifecycle. req must be non-nil; SessionGeneration
// is written back so callers (dispatch bindConfinement) MAC-bind the same
// generation the agent actually runs under.
func PrepareToolChildLifecycle(tabID, paneID string, req *launch.Request, name string) error {
	if req == nil {
		return fmt.Errorf("tool-child lifecycle requires launch request")
	}
	if tabID == "" || paneID == "" {
		return fmt.Errorf("tool-child lifecycle requires tab and pane")
	}
	if req.Decision == nil || req.TaskRef == "" || req.Repository == "" || req.Lane == "" || (req.Scope != "lane" && req.LeaseGeneration <= 0) {
		return fmt.Errorf("complete launch identity is required before lifecycle preparation")
	}
	if err := launch.Validate(*req, nil); err != nil {
		return err
	}
	if req.SessionGeneration <= 0 {
		var err error
		req.SessionGeneration, err = toolchild.NextSessionGeneration(req.Repository)
		if err != nil {
			return fmt.Errorf("durable Herdr session generation: %w", err)
		}
	}
	if req.Decision.Harness != router.PiHarness && !localDirectMode() {
		return fmt.Errorf("tool-child lifecycle requires Pi harness")
	}
	if req.Decision.Harness != router.PiHarness && localDirectMode() {
		// Native local lanes are owned by Herdr's vendor process directly; the
		// hosted Pi session ledger is not a prerequisite for a single-user pane.
		return nil
	}
	if req.Decision.HarnessSession != "" {
		return fmt.Errorf("tool-child lifecycle requires an unbound Pi session decision")
	}
	sessionPath, err := createPiLaunchSession(*req)
	if err != nil {
		return err
	}
	bound, err := router.BindHarnessSession(req.Decision, sessionPath)
	if err != nil {
		_ = os.Remove(sessionPath)
		return err
	}
	*req.Decision = *bound
	if err := launch.Validate(*req, nil); err != nil {
		_ = os.Remove(sessionPath)
		return err
	}
	toolChildMu.Lock()
	// A reserved identity is occupied even while provisional or test-injected;
	// replacing it would orphan durable authority and race a concurrent launch.
	if toolChildByTab[tabID] != nil || toolChildByPane[paneID] != nil {
		toolChildMu.Unlock()
		return fmt.Errorf("%w: tab %s/pane %s", toolchild.ErrLifecycleCollision, tabID, paneID)
	}
	f := newToolChildLifecycle
	lc, err := f(*req, name, paneID)
	if err != nil {
		toolChildMu.Unlock()
		return err
	}
	if lc == nil {
		toolChildMu.Unlock()
		return fmt.Errorf("tool-child lifecycle factory returned nil")
	}
	toolChildByPane[paneID] = lc
	toolChildByTab[tabID] = lc
	toolChildMu.Unlock()
	if concrete, ok := lc.(*toolchild.Lifecycle); ok {
		concrete.SetContext(toolchild.Identity{TabID: tabID, PaneID: paneID, Name: name, SessionGeneration: req.SessionGeneration, LaunchID: launch.DecisionDigest(req.Decision), Repository: req.Repository, Lane: req.Lane, Role: string(req.Decision.Role), TaskRef: req.TaskRef, Provider: req.Decision.Harness, ArgvDigest: launch.DecisionDigest(req.Decision), Argv: append([]string(nil), req.Decision.HarnessArgv...)})
		if err := concrete.Provision(); err != nil {
			// Provisioning has not published a valid lifecycle receipt. Close and
			// verify only the exact prepared tab, then release the process-local
			// reservation; never manufacture a tombstone for absent authority.
			if cleanupErr := cleanupUnpublishedReservation(tabID, paneID); cleanupErr != nil {
				return errors.Join(err, cleanupErr)
			}
			return err
		}
	}
	return nil
}

func cleanupUnpublishedReservation(tabID, paneID string) error {
	if err := tabCloseRaw(tabID); err != nil {
		return fmt.Errorf("unpublished reservation tab close: %w", err)
	}
	if err := verifyHerdrTerminal(tabID, paneID); err != nil {
		return fmt.Errorf("unpublished reservation terminal readback: %w", err)
	}
	dropToolChild(tabID, paneID)
	return nil
}

func isBoundConcreteLifecycle(lc ToolChildLifecycle) bool {
	concrete, ok := lc.(*toolchild.Lifecycle)
	return ok && concrete.Bound()
}

// StartPreparedAgent is the required direct-entrypoint adapter. It closes
// the gap between tab creation and exact Herdr-reported owner binding.
func StartPreparedAgent(tabID, name, kind, paneID string, req launch.Request) error {
	if err := PrepareToolChildLifecycle(tabID, paneID, &req, name); err != nil {
		if errors.Is(err, toolchild.ErrLifecycleCollision) {
			return err
		}
		if lc := lifecycleForTab(tabID); lc != nil {
			if rollbackErr := rollbackToolChild(tabID, paneID, lc, "prepare-failed"); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return err
		}
		if closeErr := tabCloseRaw(tabID); closeErr != nil {
			return errors.Join(err, closeErr)
		}
		if readbackErr := verifyHerdrTerminal(tabID, paneID); readbackErr != nil {
			return errors.Join(err, readbackErr)
		}
		return err
	}
	err := AgentStartWithDecision(name, kind, paneID, req)
	if err == nil {
		return nil
	}
	if lc := lifecycleForTab(tabID); lc != nil {
		if rollbackErr := rollbackToolChild(tabID, paneID, lc, "prepared-start-failed"); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
	}
	return err
}

func lifecycleForPane(paneID string) ToolChildLifecycle {
	toolChildMu.Lock()
	defer toolChildMu.Unlock()
	return toolChildByPane[paneID]
}
func lifecycleForTab(tabID string) ToolChildLifecycle {
	toolChildMu.Lock()
	defer toolChildMu.Unlock()
	return toolChildByTab[tabID]
}

func dropToolChild(tabID, paneID string) {
	toolChildMu.Lock()
	delete(toolChildByTab, tabID)
	delete(toolChildByPane, paneID)
	toolChildMu.Unlock()
}
func tabForPane(paneID string) string {
	toolChildMu.Lock()
	defer toolChildMu.Unlock()
	for tab, lc := range toolChildByTab {
		if toolChildByPane[paneID] == lc {
			return tab
		}
	}
	return ""
}
func rollbackToolChild(tabID, paneID string, lc ToolChildLifecycle, reason string) error {
	bound := false
	if concrete, ok := lc.(*toolchild.Lifecycle); ok {
		bound = concrete.Bound()
	}
	if probe, ok := lc.(interface{ Bound() bool }); ok {
		bound = probe.Bound()
	}
	if bound {
		if err := lc.Reconcile("failed-launch"); err != nil {
			return fmt.Errorf("child reconciliation failed; authority retained: %w", err)
		}
		if err := compensateStartedProcessExact("", paneID); err != nil {
			return err
		}
	} else if err := tabCloseRaw(tabID); err != nil {
		return err
	}
	if err := verifyHerdrTerminal(tabID, paneID); err != nil {
		return fmt.Errorf("terminal Herdr readback failed; authority retained: %w", err)
	}
	if err := lc.Invalidate(reason); err != nil {
		return fmt.Errorf("tombstone failed; authority retained: %w", err)
	}
	if err := lc.VerifyTerminal(); err != nil {
		return fmt.Errorf("tombstone readback failed; authority retained: %w", err)
	}
	dropToolChild(tabID, paneID)
	return nil
}

func verifyHerdrTerminal(tabID, paneID string) error {
	tabs, err := tabList()
	if err != nil {
		return err
	}
	for _, id := range tabs {
		if id == tabID {
			return fmt.Errorf("tab %s remains live", tabID)
		}
	}
	agents, err := AgentList()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.TabID == tabID || a.PaneID == paneID {
			return fmt.Errorf("agent/tab/pane remains live")
		}
	}
	processes, err := paneProcesses(paneID)
	if err != nil {
		if errors.Is(err, ErrPaneNotFound) {
			return nil
		}
		return err
	}
	if len(processes) != 0 {
		return fmt.Errorf("foreground process remains live")
	}
	return nil
}

// tabListEntry is one row of `herdr tab list`, id/label plus the workspace
// it actually lives in — required so label repair can refuse to cross a
// workspace boundary (FAC-199).
type tabListEntry struct {
	ID        string
	Label     string
	Workspace string
}

func tabListDetailed() ([]tabListEntry, error) {
	output, err := runHerdr("tab", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr tab list: %s: %w", output, err)
	}
	var envelope struct {
		Result struct {
			Tabs []struct {
				ID        string `json:"id"`
				TabID     string `json:"tab_id"`
				Label     string `json:"label"`
				Workspace string `json:"workspace_id"`
			} `json:"tabs"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		return nil, err
	}
	if envelope.Result.Tabs == nil {
		return nil, fmt.Errorf("herdr tab list returned no tabs inventory")
	}
	entries := make([]tabListEntry, 0, len(envelope.Result.Tabs))
	for _, t := range envelope.Result.Tabs {
		id := t.TabID
		if id == "" {
			id = t.ID
		}
		if id != "" {
			entries = append(entries, tabListEntry{ID: id, Label: t.Label, Workspace: t.Workspace})
		}
	}
	return entries, nil
}

func tabList() ([]string, error) {
	entries, err := tabListDetailed()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	return ids, nil
}

// tabLookup returns the live tab list row for tabID.
func tabLookup(tabID string) (tabListEntry, error) {
	entries, err := tabListDetailed()
	if err != nil {
		return tabListEntry{}, err
	}
	for _, e := range entries {
		if e.ID == tabID {
			return e, nil
		}
	}
	return tabListEntry{}, fmt.Errorf("herdr tab list: tab %s not found", tabID)
}

// TabLabel returns the live label of tabID as reported by `herdr tab list`.
func TabLabel(tabID string) (string, error) {
	e, err := tabLookup(tabID)
	if err != nil {
		return "", err
	}
	return e.Label, nil
}

// TabRename renames tabID in place. Unlike TabClose+TabCreate, this touches
// only the tab's label — pane, session, process, and worktree identity are
// untouched, which is what live label reconciliation requires (FAC-199).
// The Herdforge prefix is enforced here too (not just at TabCreate) so this
// primitive can never be used to write a raw label back onto a live tab.
func TabRename(tabID, label string) error {
	if strings.TrimSpace(tabID) == "" {
		return fmt.Errorf("herdr tab rename: tab id is required")
	}
	label = EnsureHerdforgeLabel(label)
	out, err := runHerdr("tab", "rename", tabID, label)
	if err != nil {
		return fmt.Errorf("herdr tab rename %s: %s: %w", tabID, out, err)
	}
	return nil
}

// ReconcileHerdforgeLabel renames tabID in place when its live label does
// not carry the Herdforge prefix, and leaves it untouched otherwise. It
// never closes or restarts the tab (FAC-199: resumed/recovered tabs must be
// relabeled without disturbing pane/session/process/worktree identity).
//
// wantWorkspace fails the repair closed when the tab's live workspace_id
// does not match: label repair must never cross a workspace boundary, so a
// stale/incorrect tab id (or a tab that lives in some other, non-Herdforge
// workspace) is refused rather than silently relabeled.
//
// After issuing the rename, the label is read back from a fresh `tab list`
// call and compared against what was requested — a zero exit status from
// `herdr tab rename` is not accepted as proof by itself.
//
// This is the tab-only primitive. Callers that hold a live AgentEntry
// (resume and the fleet sweep both do) should use ReconcileAgentLabel
// instead, which additionally proves pane/session/cwd identity survived the
// rename untouched.
func ReconcileHerdforgeLabel(tabID, wantWorkspace string) (label string, renamed bool, err error) {
	if strings.TrimSpace(wantWorkspace) == "" {
		return "", false, fmt.Errorf("herdr label reconcile: workspace is required (refusing unscoped relabel)")
	}
	before, err := tabLookup(tabID)
	if err != nil {
		return "", false, err
	}
	if before.Workspace != wantWorkspace {
		return "", false, fmt.Errorf("herdr label reconcile: tab %s is in workspace %q, not %q; refusing cross-workspace relabel", tabID, before.Workspace, wantWorkspace)
	}
	want := EnsureHerdforgeLabel(before.Label)
	if want == before.Label {
		return before.Label, false, nil
	}
	if err := TabRename(tabID, want); err != nil {
		return "", false, err
	}
	after, err := tabLookup(tabID)
	if err != nil {
		return "", false, fmt.Errorf("herdr label reconcile: post-rename readback failed: %w", err)
	}
	if after.Label != want {
		return "", false, fmt.Errorf("herdr label reconcile: rename reported success but live label is %q, want %q", after.Label, want)
	}
	if after.Workspace != wantWorkspace {
		return "", false, fmt.Errorf("herdr label reconcile: tab %s workspace changed during rename (%q -> %q)", tabID, before.Workspace, after.Workspace)
	}
	return want, true, nil
}

// ReconcileAgentLabel is the identity-verified reconciliation entrypoint
// (FAC-199): given a live AgentEntry snapshot (the caller's already-current
// view from AgentList), it repairs the label in place via
// ReconcileHerdforgeLabel and then — only when a rename actually happened —
// re-reads AgentList and proves pane, session, workspace, and cwd are
// exactly what they were before the rename. This is the full identity
// surface herdr's live inventory actually exposes; task/lease/generation
// identity is verified earlier by the launch-lifecycle checks that already
// run before resume ever reaches this call (recoverStandingLifecycle,
// launch.HasStarted) — herdr's own `agent list`/`tab list` carry no
// task/generation fields to re-check here.
func ReconcileAgentLabel(before AgentEntry) (label string, renamed bool, err error) {
	if strings.TrimSpace(before.TabID) == "" {
		return "", false, fmt.Errorf("herdr label reconcile: agent tab id is required")
	}
	label, renamed, err = ReconcileHerdforgeLabel(before.TabID, before.Workspace)
	if err != nil || !renamed {
		return label, renamed, err
	}
	agents, aerr := AgentList()
	if aerr != nil {
		return "", false, fmt.Errorf("herdr label reconcile: post-rename identity readback failed: %w", aerr)
	}
	for _, a := range agents {
		if a.TabID != before.TabID {
			continue
		}
		if a.PaneID != before.PaneID || a.Session.Value != before.Session.Value || a.Cwd != before.Cwd || a.Workspace != before.Workspace {
			return "", false, fmt.Errorf("herdr label reconcile: tab %s identity drifted during rename (pane/session/cwd/workspace changed)", before.TabID)
		}
		return label, renamed, nil
	}
	return "", false, fmt.Errorf("herdr label reconcile: tab %s vanished from agent inventory during rename", before.TabID)
}

// ReconcileWorkspaceLabels sweeps every live, Herdforge-owned agent's tab in
// workspace and repairs any drifted label in place (FAC-199's bounded
// reconciliation path). Ownership is determined by AgentEntry.Name — same
// doctrine as SelectCleanupCandidates ("unnamed panes are the operator's,
// never ours"). A tab is only ever touched when it belongs to a *named*
// live agent; a raw, non-empty tab label alone (an operator's numbered
// terminal, "Terminal", etc.) is never sufficient, since those commonly
// have non-empty labels too. Bounded: one pass over the live agent
// inventory, no retries, no process/pane mutation. Returns the tab ids that
// were actually renamed.
func ReconcileWorkspaceLabels(workspace string) ([]string, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("herdr label sweep: workspace is required")
	}
	agents, err := AgentList()
	if err != nil {
		return nil, err
	}
	var renamed []string
	var errs []error
	for _, a := range agents {
		if a.Name == "" || a.Workspace != workspace || a.TabID == "" {
			continue
		}
		if _, changed, err := ReconcileAgentLabel(a); err != nil {
			errs = append(errs, fmt.Errorf("tab %s: %w", a.TabID, err))
		} else if changed {
			renamed = append(renamed, a.TabID)
		}
	}
	if len(errs) > 0 {
		return renamed, errors.Join(errs...)
	}
	return renamed, nil
}

func paneProcessInventorySummary(processes []PaneProcess) string {
	parts := make([]string, 0, len(processes))
	for _, p := range processes {
		parts = append(parts, fmt.Sprintf("pid=%d name=%q argv=%q", p.PID, p.Name, p.Argv))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// AgentReadyForToolChildBind is the caf589a2 inventory gate for owner bind:
// both a tab and an agent_session.value must be present before the caller
// leaves the poll loop.
//
// b81bf2e once relaxed this to tab-only because no non-claude harness reported
// a session id. caf589a2 superseded that: Session.Value is now the Pi session
// FILE PATH that herd itself allocates, and bindToolChildLifecycle feeds it
// straight into routedProcessCandidates -> verifyPiSessionRoute. An empty path
// there is ErrPiSessionRouteMismatch (a hard return out of the poll loop), not
// ErrPiSessionNotReady (a retryable wait). Relaxing this gate converts the
// normal startup window — pi visible in the pane inventory before
// agent_session.value lands — into an intermittent launch abort on the fleet's
// only harness. Session is provenance for identity purposes; it is a
// precondition for routing.
func AgentReadyForToolChildBind(a AgentEntry) bool {
	return strings.TrimSpace(a.TabID) != "" && strings.TrimSpace(a.Session.Value) != ""
}

func bindToolChildLifecycle(paneID, name string, req launch.Request) error {
	lc := lifecycleForPane(paneID)
	if lc == nil {
		return nil
	}
	sessionGeneration := req.SessionGeneration
	if sessionGeneration <= 0 {
		if concrete, ok := lc.(*toolchild.Lifecycle); ok {
			sessionGeneration = concrete.Inventory.Owner.SessionGeneration
		}
	}
	if sessionGeneration <= 0 {
		return fmt.Errorf("Herdr session generation is unavailable")
	}

	deadline := time.Now().Add(ownerBindTimeout)
	sawAgent := false
	lastWaitReason := "matching agent not listed"
	for {
		agents, err := AgentList()
		if err != nil {
			return fmt.Errorf("tool-child owner lookup: %w", err)
		}
		matchesAgent := make([]AgentEntry, 0, 1)
		for _, a := range agents {
			if a.Name == name && a.PaneID == paneID {
				matchesAgent = append(matchesAgent, a)
			}
		}
		if len(matchesAgent) > 1 {
			return fmt.Errorf("tool-child owner agent identity is ambiguous for %s/%s", name, paneID)
		}
		if len(matchesAgent) == 1 {
			a := matchesAgent[0]
			sawAgent = true
			if a.Kind != req.Decision.Harness {
				return fmt.Errorf("tool-child owner harness mismatch: got %s want %s", a.Kind, req.Decision.Harness)
			}
			// Tab AND session must be present: the session value is the Pi
			// session path routedProcessCandidates attests below (caf589a2).
			if !AgentReadyForToolChildBind(a) {
				lastWaitReason = fmt.Sprintf("agent incomplete kind=%q status=%q tab_present=%t session_present=%t", a.Kind, a.Status, a.TabID != "", a.Session.Value != "")
			} else {
				if a.TabID != tabForPane(paneID) {
					return fmt.Errorf("prepared tab identity drift: got %s", a.TabID)
				}
				processes, err := paneProcesses(paneID)
				if err != nil {
					if !errors.Is(err, ErrPaneNotFound) {
						return err
					}
					lastWaitReason = "prepared pane process inventory is not yet available"
				} else {
					hydrated, argvErrors := hydratePaneProcesses(processes)
					processes = hydrated
					if len(argvErrors) > 0 {
						lastWaitReason = fmt.Sprintf("process argv unavailable: session=%q %v; observed=%s", a.Session.Value, errors.Join(argvErrors...), paneProcessInventorySummary(processes))
					} else {
						processMatches, wrappers, routeWait, routeErr := routedProcessCandidates(req.Decision.Harness, req.Decision.HarnessArgv, a.Session.Value, processes)
						if routeErr != nil {
							return routeErr
						}
						if routeWait != "" {
							lastWaitReason = fmt.Sprintf("%s; session=%q observed=%s", routeWait, a.Session.Value, paneProcessInventorySummary(processes))
						} else if len(processMatches) == 0 || (strings.EqualFold(req.Decision.Harness, "codex") && len(wrappers) == 0) {
							lastWaitReason = fmt.Sprintf("process candidates exact=%d wrappers=%d total=%d session=%q observed=%s", len(processMatches), len(wrappers), len(processes), a.Session.Value, paneProcessInventorySummary(processes))
						} else {
							if len(processMatches) != 1 {
								return fmt.Errorf("pane %s has %d exact routed agent processes", paneID, len(processMatches))
							}
							p := processMatches[0]
							if p.PID <= 0 || len(p.Argv) == 0 {
								return fmt.Errorf("pane process identity is incomplete")
							}
							token, err := readPIDStartToken(p.PID)
							if err != nil {
								return err
							}
							parent, err := readPIDParent(p.PID)
							if err != nil {
								return err
							}
							if strings.EqualFold(req.Decision.Harness, "codex") {
								if len(wrappers) != 1 || parent != wrappers[0].PID {
									return fmt.Errorf("codex native process requires exactly one node wrapper parent")
								}
							} else if len(wrappers) > 1 {
								return fmt.Errorf("harness process has ambiguous node wrappers")
							}
							if req.Repository == "" || req.Lane == "" {
								return fmt.Errorf("repository and lane binding are required")
							}
							return lc.Bind(toolchild.Identity{PID: p.PID, ParentPID: parent, StartToken: token, SessionGeneration: sessionGeneration, LaunchID: launch.DecisionDigest(req.Decision), Repository: req.Repository, Role: string(req.Decision.Role), Lane: req.Lane, TaskRef: req.TaskRef, Name: name, SessionID: a.Session.Value, PaneID: paneID, TabID: a.TabID, Provider: req.Decision.Harness, ArgvDigest: launch.DecisionDigest(req.Decision), Argv: append([]string(nil), req.Decision.HarnessArgv...)})
						}
					}
				}
			}
		} else {
			lastWaitReason = "matching agent not listed"
		}
		if !time.Now().Before(deadline) {
			if sawAgent {
				return fmt.Errorf("tool-child owner identity did not become ready for %s/%s: %s", name, paneID, lastWaitReason)
			}
			return fmt.Errorf("tool-child owner identity unavailable for %s/%s: %s", name, paneID, lastWaitReason)
		}
		time.Sleep(ownerBindPollInterval)
	}
}

// bindRecoveredToolChildLifecycle reconstructs the exact owner after a
// coordinator restart. The provisional receipt is the only source of launch
// intent; live Herdr inventory supplies the tab/session and pane process-info
// supplies the candidate PID. No process-local map or broad process match is
// used.
func bindRecoveredToolChildLifecycle(lc *toolchild.Lifecycle) error {
	if lc == nil || lc.Bound() {
		return nil
	}
	owner := lc.Inventory.Owner
	if owner.TabID == "" || owner.PaneID == "" || owner.Provider != router.PiHarness || len(owner.Argv) != 7 || owner.ArgvDigest == "" || owner.Repository == "" {
		return fmt.Errorf("recovery provisional Pi identity is incomplete")
	}
	agents, err := AgentList()
	if err != nil {
		return fmt.Errorf("recovery agent inventory: %w", err)
	}
	var match *AgentEntry
	for i := range agents {
		a := &agents[i]
		if a.TabID != owner.TabID || a.PaneID != owner.PaneID || a.Kind != owner.Provider || a.Name != owner.Name || a.Session.Value == "" {
			continue
		}
		if owner.SessionID != "" && a.Session.Value != owner.SessionID {
			continue
		}
		if match != nil {
			return fmt.Errorf("recovery agent identity is ambiguous")
		}
		match = a
	}
	if match == nil {
		return fmt.Errorf("recovery agent identity unavailable")
	}
	processes, err := paneProcesses(owner.PaneID)
	if err != nil {
		return err
	}
	hydrated, argvErrors := hydratePaneProcesses(processes)
	if len(argvErrors) > 0 {
		return fmt.Errorf("recovery pane process argv unavailable: %w", errors.Join(argvErrors...))
	}
	processes = hydrated
	native, wrappers, routeWait, routeErr := routedProcessCandidates(owner.Provider, owner.Argv, match.Session.Value, processes)
	if routeErr != nil {
		return fmt.Errorf("recovery Pi session route attestation: %w", routeErr)
	}
	if routeWait != "" {
		return fmt.Errorf("recovery Pi session route not ready: %s", routeWait)
	}
	if len(native) != 1 {
		return fmt.Errorf("recovery routed owner candidates=%d", len(native))
	}
	p := native[0]
	if len(wrappers) > 1 {
		return fmt.Errorf("recovery harness wrapper ancestry is ambiguous")
	}
	token, err := readPIDStartToken(p.PID)
	if err != nil {
		return err
	}
	parent, err := readPIDParent(p.PID)
	if err != nil {
		return err
	}
	if owner.SessionID != "" && owner.SessionID != match.Session.Value {
		return fmt.Errorf("recovery session drift")
	}
	owner.PID, owner.ParentPID, owner.StartToken = p.PID, parent, token
	owner.SessionID = match.Session.Value
	return lc.Bind(owner)
}

func ReconcileToolChild(tabID, event string) error {
	lc, err := loadToolChildLifecycle(tabID)
	if err != nil {
		return fmt.Errorf("tool-child cleanup blocked: %w", err)
	}
	if concrete, ok := lc.(*toolchild.Lifecycle); ok && !concrete.Bound() {
		if err := bindRecoveredToolChildLifecycle(concrete); err != nil {
			return fmt.Errorf("tool-child pre-bind recovery blocked: %w", err)
		}
	}
	return lc.Reconcile(event)
}

func loadToolChildLifecycle(tabID string) (ToolChildLifecycle, error) {
	toolChildMu.Lock()
	lc := toolChildByTab[tabID]
	toolChildMu.Unlock()
	if lc == nil {
		path := os.Getenv("HERD_TOOLCHILD_RECEIPTS")
		var err error
		if path != "" {
			lc, err = toolchild.LoadLifecycle(path, tabID, toolchild.SystemTree{}, &toolchild.JSONLSink{Path: path})
		} else {
			lc, err = toolchild.DiscoverLifecycle(tabID, toolchild.SystemTree{}, &toolchild.JSONLSink{})
		}
		if err != nil {
			return nil, err
		}
		toolChildMu.Lock()
		toolChildByTab[tabID] = lc
		if concrete, ok := lc.(*toolchild.Lifecycle); ok {
			toolChildByPane[concrete.Inventory.Owner.PaneID] = lc
		}
		toolChildMu.Unlock()
	}
	return lc, nil
}

func recoverStandingLifecycle(agent AgentEntry, req launch.Request) (int64, error) {
	if req.Decision == nil || req.Decision.Harness != router.PiHarness || req.Decision.HarnessSession == "" || agent.TabID == "" || agent.PaneID == "" || agent.Session.Value == "" {
		return 0, fmt.Errorf("standing lifecycle authority is incomplete: %w", ErrAgentIdentityMismatch)
	}
	if err := verifyPiSessionRoute(agent.Session.Value, req.Decision.HarnessArgv); err != nil {
		return 0, fmt.Errorf("standing Pi session route mismatch: %w: %w", err, ErrAgentIdentityMismatch)
	}
	lc, err := loadToolChildLifecycle(agent.TabID)
	if err != nil {
		return 0, fmt.Errorf("standing lifecycle recovery: %w", err)
	}
	concrete, ok := lc.(*toolchild.Lifecycle)
	if !ok || !concrete.Bound() {
		return 0, fmt.Errorf("standing lifecycle is not bound: %w", ErrAgentIdentityMismatch)
	}
	owner := concrete.Inventory.Owner
	expectedDigest := launch.DecisionDigest(req.Decision)
	if owner.TabID != agent.TabID || owner.PaneID != agent.PaneID || agent.Kind != req.Decision.Harness || owner.Repository != req.Repository || owner.TaskRef != req.TaskRef || owner.Lane != req.Lane || owner.Provider != req.Decision.Harness || owner.Role != string(req.Decision.Role) || owner.LaunchID != expectedDigest || owner.ArgvDigest != expectedDigest || !equalArgs(owner.Argv, req.Decision.HarnessArgv) || owner.SessionID != agent.Session.Value || owner.SessionID != req.Decision.HarnessSession || agent.Session.Value != req.Decision.HarnessSession || owner.SessionGeneration <= 0 || concrete.RecoveredPhase >= 4 {
		return 0, fmt.Errorf("standing lifecycle identity mismatch or terminal authority: %w", ErrAgentIdentityMismatch)
	}
	if req.SessionGeneration != 0 && req.SessionGeneration != owner.SessionGeneration {
		return 0, fmt.Errorf("standing lifecycle generation mismatch: %w", ErrAgentIdentityMismatch)
	}
	return owner.SessionGeneration, nil
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func reconcileRecoveredToolChild(tabID, event string, generation int64) error {
	lc := lifecycleForTab(tabID)
	concrete, ok := lc.(*toolchild.Lifecycle)
	if !ok || concrete.Inventory.Owner.SessionGeneration != generation {
		return fmt.Errorf("standing lifecycle generation changed before reconciliation: %w", ErrAgentIdentityMismatch)
	}
	return ReconcileToolChild(tabID, event)
}

type TabInfo struct {
	ID         string
	Label      string
	Generation string
	Pane       PaneInfo
	Cwd        string // process cwd requested at tab create (empty for legacy Tab)
}

type PaneInfo struct {
	ID    string
	TabID string
	// TerminalID is herdr's pane INCARNATION token (e.g.
	// "term_658791cc707d56e"). tab_id/pane_id name a slot that a
	// replacement agent can occupy; terminal_id is the only field on
	// herdr's PaneInfo that distinguishes the process we launched from a
	// later one that took the same slot. It is `required` on the herdr
	// PaneInfo/AgentInfo schema, so an empty value means we are not
	// talking to a herdr that can prove incarnation (FAC-145).
	TerminalID string
}

// TabRecord is the exact Herdr tab-list socket read model. It intentionally
// contains no task/generation/role fields; those belong to a separate durable
// launch-receipt binding authority.
type TabRecord struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	// Generation is optional because older Herdr versions do not expose
	// immutable tab generations in tab-list responses. Newer versions may use
	// either generation or tab_generation; TabList normalizes both to this
	// field so reconciliation consumes the live identity surface.
	Generation    string `json:"generation,omitempty"`
	TabGeneration string `json:"tab_generation,omitempty"`
	Number        int    `json:"number"`
	PaneCount     int    `json:"pane_count"`
	Focused       bool   `json:"focused"`
	AgentStatus   string `json:"agent_status"`
}

// TabList reads durable tab metadata. It is read-only and deliberately does
// not fall back to labels, panes, or process guesses when fields are absent.
func TabList(workspace string) ([]TabRecord, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("herdr tab list: workspace is required")
	}
	out, err := runHerdr("tab", "list", "--workspace", workspace)
	if err != nil {
		return nil, fmt.Errorf("herdr tab list: %w", err)
	}
	var resp struct {
		Result struct {
			Tabs []TabRecord `json:"tabs"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("parsing tab list: %w", err)
	}
	for i := range resp.Result.Tabs {
		if strings.TrimSpace(resp.Result.Tabs[i].Generation) == "" {
			resp.Result.Tabs[i].Generation = strings.TrimSpace(resp.Result.Tabs[i].TabGeneration)
		}
	}
	return resp.Result.Tabs, nil
}

// TabCreateOptions is the fail-closed tab launch contract (FAC-121).
// Workspace and Cwd are required; unknown workspace must not fall back.
type TabCreateOptions struct {
	Workspace string
	Label     string
	Cwd       string
	NoFocus   bool
	Env       []string // optional KEY=VALUE pairs
	// HostedUID when >0 requests the herdr *daemon* host the tab shell as this
	// kernel UID (FAC-172 BuilderUID). Requires structured capability negotiation.
	// Running the herdr CLI under setuid is NOT used and is not isolation.
	HostedUID int
}

// Tab creates a new tab in the specified workspace and returns the tab + root pane.
// Legacy convenience without cwd — prefer TabCreate for write-capable agents.
// Labels are auto-prefixed with "Herdforge · " when missing (FAC-141).
func Tab(workspaceID, label string, noFocus bool) (*TabInfo, error) {
	return TabCreate(TabCreateOptions{
		Workspace: workspaceID,
		Label:     label,
		NoFocus:   noFocus,
	})
}

// TabCreate creates a herdr tab with explicit workspace and optional cwd.
// When Cwd is set it is passed as --cwd so the pane process starts there
// (prompt "cd" is not isolation). Empty Workspace fails closed.
// Labels lacking the "Herdforge · " prefix are auto-prefixed (FAC-141).
func TabCreate(opts TabCreateOptions) (*TabInfo, error) {
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, fmt.Errorf("herdr tab create: workspace is required (no hardcoded fallback)")
	}
	opts.Label = EnsureHerdforgeLabel(opts.Label)
	args := []string{"tab", "create", "--workspace", opts.Workspace, "--label", opts.Label}
	if opts.Cwd != "" {
		args = append(args, "--cwd", opts.Cwd)
	}
	if opts.NoFocus {
		args = append(args, "--no-focus")
	}
	for _, e := range opts.Env {
		if e != "" {
			args = append(args, "--env", e)
		}
	}
	// FAC-172: request daemon-hosted uid only after structured capability
	// negotiation. Never sudo the CLI; never scrape --help for flags.
	if opts.HostedUID > 0 {
		if err := RequireHerdrBuilderSpawnCapability(opts.HostedUID); err != nil {
			return nil, err
		}
		fa, err := hostedUIDFlagArgs(opts.HostedUID)
		if err != nil {
			return nil, err
		}
		args = append(args, fa...)
	}
	output, err := runHerdr(args...)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: %w", err)
	}

	var resp struct {
		Result struct {
			Tab struct {
				TabID         string `json:"tab_id"`
				Label         string `json:"label"`
				Generation    string `json:"generation,omitempty"`
				TabGeneration string `json:"tab_generation,omitempty"`
			} `json:"tab"`
			RootPane struct {
				PaneID     string `json:"pane_id"`
				TabID      string `json:"tab_id"`
				TerminalID string `json:"terminal_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parsing tab create output: %s: %w", output, err)
	}
	// terminal_id is required on herdr's PaneInfo schema. Without it we
	// cannot bind an agent's authority to its incarnation, so fail closed
	// rather than hand back a slot-only identity (FAC-145).
	if strings.TrimSpace(resp.Result.RootPane.TerminalID) == "" {
		return nil, fmt.Errorf("herdr tab create: root pane carries no terminal_id; cannot bind agent incarnation (FAC-145): %s", output)
	}

	tab := &TabInfo{
		ID:         resp.Result.Tab.TabID,
		Label:      resp.Result.Tab.Label,
		Generation: strings.TrimSpace(resp.Result.Tab.Generation),
		Cwd:        opts.Cwd,
		Pane: PaneInfo{
			ID:         resp.Result.RootPane.PaneID,
			TabID:      resp.Result.RootPane.TabID,
			TerminalID: resp.Result.RootPane.TerminalID,
		},
	}
	if tab.Generation == "" {
		tab.Generation = strings.TrimSpace(resp.Result.Tab.TabGeneration)
	}
	// FAC-172: every HostedUID path proves shell/tree UID after create — not
	// flag-and-trust-daemon. Proof failure kills bound PIDs and closes the tab.
	if opts.HostedUID > 0 {
		if _, err := AssertHostedPaneUID(tab.Pane.ID, opts.HostedUID); err != nil {
			return nil, FailHostedIsolationProof(tab.Pane.ID, tab.ID,
				fmt.Errorf("herdr tab create: hosted uid proof failed: %w", err))
		}
	}
	return tab, nil
}

// AgentRoleEnv marks every task-agent pane: herdr injects it at tab
// creation so processes the agent spawns inherit it, and the receipt
// signer refuses to operate under it. It is a ROLE MARKER, not identity —
// an ordinary environment variable a determined same-UID process can
// scrub. Non-bypassable identity containment is FAC-133's sandbox
// boundary; until that merges, coordinator-only signing rests on the
// out-of-tree key location plus this marker and the cwd refusal.
const AgentRoleEnv = "HERD_ROLE=agent"

const (
	childWorkspaceSourceEnv       = "HERD_WORKSPACE_SOURCE"
	childWorkspaceOverrideIgnored = "HERD_WORKSPACE_OVERRIDE_IGNORED"
)

// bindChildWorkspaceEnv makes the launch target the sole source of workspace
// identity for a child pane. Herdr's --workspace flag selects the tab, but it
// does not rewrite the shell environment inherited by that tab. Keep the
// identity values last so a stale operator export or caller-provided value
// cannot route callbacks and receipts to another repository.
//
// HERD_WORKSPACE_OVERRIDE_IGNORED is durable child evidence that an inherited
// HERD_WORKSPACE was present and deliberately superseded.
func bindChildWorkspaceEnv(root, workspace string, env []string) []string {
	root = strings.TrimSpace(root)
	workspace = strings.TrimSpace(workspace)
	bound := append([]string(nil), env...)
	ignored := strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "HERD_WORKSPACE" && strings.TrimSpace(value) != "" {
			ignored = strings.TrimSpace(value)
		}
	}
	bound = append(bound,
		"HERD_ROOT="+root,
		"HERD_WORKSPACE="+workspace,
		"HERDR_WORKSPACE_ID="+workspace,
		childWorkspaceSourceEnv+"=launch-target",
	)
	if ignored != "" {
		bound = append(bound, childWorkspaceOverrideIgnored+"="+ignored)
	}
	return bound
}

// TabForAgent creates an agent pane WITHOUT a task worktree cwd (standing
// agents, pulse/review/forge spawns). It still carries the agent role
// marker so every agent-facing pane is uniformly marked (FAC-145).
func TabForAgent(workspaceID, label string, noFocus bool) (*TabInfo, error) {
	return TabCreate(TabCreateOptions{
		Workspace: workspaceID,
		Label:     label,
		NoFocus:   noFocus,
		Env:       []string{AgentRoleEnv},
	})
}

// TabCreateForTask is the FAC-121 launch entry: requires workspace and cwd.
// Rejects empty cwd so shared-root / unknown-directory starts cannot slip through.
// Resolves cwd to an absolute path at the caller and requires it to be an existing
// directory before contacting Herdr, so relative paths cannot be re-resolved from
// the Herdr server process cwd.
//
// FAC-172: when isolation is required, the herdr daemon must host the tab
// shell as BuilderUID. Capability negotiation is structured only; TabCreate
// proves shell/process-group/descendant UIDs after create. Proof failure
// terminates start-token-bound PIDs (procsignal) and closes the orphan tab.
//
// Optional env entries are KEY=VALUE pairs passed to herdr tab create --env
// (FAC-190: PATH must put the confinement agent wrapper first). HostedUID
// launch env is appended after caller env when isolation is required.
//
// FAC-145: every task pane also carries the agent role marker.
func TabCreateForTask(workspaceID, label, cwd string, noFocus bool, env ...string) (*TabInfo, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil, fmt.Errorf("herdr tab create: cwd is required for task agents")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: resolve cwd %q: %w", cwd, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: cwd %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("herdr tab create: cwd %q is not a directory", abs)
	}
	opts := TabCreateOptions{
		Workspace: workspaceID,
		Label:     label,
		Cwd:       abs,
		NoFocus:   noFocus,
		// FAC-145 role marker first; caller env (FAC-190 PATH wrapper) follows,
		// then bindChildWorkspaceEnv appends the launch identity last.
		Env: bindChildWorkspaceEnv(abs, workspaceID, append([]string{AgentRoleEnv}, env...)),
	}
	if HostedUIDIsolationRequired() {
		bUID, err := BuilderUID()
		if err != nil {
			return nil, fmt.Errorf("herdr tab create: %w", err)
		}
		if err := RequireHerdrBuilderSpawnCapability(bUID); err != nil {
			return nil, fmt.Errorf("herdr tab create: %w", err)
		}
		opts.HostedUID = bUID
		opts.Env = append(opts.Env, hostedUIDLaunchEnv(bUID)...)
	}
	return TabCreate(opts)
}

// AgentStart starts an agent in the specified pane. Extra agentArgs are
// passed through to the agent executable after `--` (e.g. --model X) — a
// lane's configured model MUST reach the launch argv or the agent silently
// runs on the harness default (observed: worker lane configured for
// deepseek-v4-flash launched on the opencode default instead).
// Newly created tabs may need a brief moment before the pane shell is ready;
// we sleep and retry once if herdr reports agent_pane_busy.
func AgentStart(name, kind string, paneID string, agentArgs ...string) error {
	// Raw starts are the incident path. There is no trustworthy role, shape,
	// provider, effort, or decision provenance in this API, so it can never
	// create a process. The failed receipt records the attempted raw request.
	return launch.Validate(launch.Request{}, nil)
}

func validatePreparedPiStart(tabID string, lc ToolChildLifecycle, name, paneID string, req launch.Request) error {
	if lc == nil || req.Decision == nil {
		return fmt.Errorf("prepared Pi lifecycle and decision are required")
	}
	d := req.Decision
	sessionPath := filepath.Clean(strings.TrimSpace(d.HarnessSession))
	if d.Harness != router.PiHarness || sessionPath == "." || !filepath.IsAbs(sessionPath) {
		return fmt.Errorf("process start requires a bound Pi harness session")
	}
	if len(d.HarnessArgv) != 7 || d.HarnessArgv[5] != "--session" || filepath.Clean(d.HarnessArgv[6]) != sessionPath {
		return fmt.Errorf("process start requires exact bound Pi harness argv")
	}
	concrete, ok := lc.(*toolchild.Lifecycle)
	if !ok {
		// Non-concrete lifecycles exist only behind the explicit test seam.
		return nil
	}
	owner := concrete.Inventory.Owner
	digest := launch.DecisionDigest(d)
	if owner.TabID != tabID || owner.PaneID != paneID || owner.Name != name || owner.SessionGeneration != req.SessionGeneration || owner.LaunchID != digest || owner.Repository != req.Repository || owner.Role != string(d.Role) || owner.Lane != req.Lane || owner.TaskRef != req.TaskRef || owner.Provider != d.Harness || owner.ArgvDigest != digest || !equalArgs(owner.Argv, d.HarnessArgv) {
		return fmt.Errorf("prepared Pi lifecycle authority does not match launch request")
	}
	return nil
}

// AgentStartWithDecision is the direct Herdr adapter. Validation happens
// before the process API is invoked, including for recovery/rescue callers.
func AgentStartWithDecision(name, kind, paneID string, req launch.Request) error {
	req.Name, req.PaneID = name, paneID
	lc := lifecycleForPane(paneID)
	compensateValidation := func(primary error) error {
		if lc == nil {
			return primary
		}
		if rollbackErr := rollbackToolChild(tabForPane(paneID), paneID, lc, "pre-start-validation-failed"); rollbackErr != nil {
			return errors.Join(primary, rollbackErr)
		}
		return primary
	}
	if err := launch.Validate(req, nil); err != nil {
		return compensateValidation(err)
	}
	if req.Decision == nil || strings.TrimSpace(kind) != strings.TrimSpace(req.Decision.Harness) {
		return compensateValidation(launch.RecordRejected(req, nil, fmt.Sprintf("herdr kind %q does not match decision harness", kind)))
	}
	if req.Decision == nil || len(req.Decision.HarnessArgv) == 0 {
		return compensateValidation(fmt.Errorf("launch decision harness argv is empty"))
	}
	if req.SessionGeneration <= 0 {
		if concrete, ok := lc.(*toolchild.Lifecycle); ok {
			req.SessionGeneration = concrete.Inventory.Owner.SessionGeneration
		}
	}
	if req.SessionGeneration <= 0 {
		return fmt.Errorf("durable Herdr session generation is unavailable")
	}
	if lc == nil && localDirectMode() {
		if err := agentStartProcess(name, kind, paneID, req.Decision.HarnessArgv[1:]...); err != nil {
			return err
		}
		observation, err := verifyAgentLaunch(name, paneID, 15*time.Second)
		if err != nil {
			cleanupErr := compensateStartedProcessExact(name, paneID)
			if cleanupErr != nil {
				return errors.Join(fmt.Errorf("herdr launch %s: %w (%s)", observation.State, err, observation.Reason), fmt.Errorf("launch cleanup: %w", cleanupErr))
			}
			return fmt.Errorf("herdr launch %s: %w (%s)", observation.State, err, observation.Reason)
		}
		return nil
	}
	if lc == nil {
		return fmt.Errorf("prepared tool-child lifecycle is required before process start")
	}
	if err := validatePreparedPiStart(tabForPane(paneID), lc, name, paneID, req); err != nil {
		return compensateValidation(err)
	}
	// FAC-172: when isolation is configured, negotiate capability and attach
	// structured daemon uid flags before process start (never CLI setuid).
	var builderUID int
	if HostedUIDIsolationRequired() {
		var err error
		builderUID, err = BuilderUID()
		if err != nil {
			return compensateValidation(err)
		}
		if err := RequireHerdrBuilderSpawnCapability(builderUID); err != nil {
			return compensateValidation(err)
		}
	}
	if err := agentStartProcess(name, kind, paneID, req.Decision.HarnessArgv[1:]...); err != nil {
		if rollbackErr := rollbackToolChild(tabForPane(paneID), paneID, lc, "failed-launch"); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		_ = launch.RecordRejected(req, nil, err.Error())
		return err
	}
	// FAC-172: prove shell/tree and exact routed agent credentials BEFORE
	// binding tool-child inventory — a wrong-UID process must never become
	// durable owner. Agent proof polls readiness like bindToolChildLifecycle.
	if builderUID > 0 {
		tabID := tabForPane(paneID)
		wantGID, err := AssertHostedPaneUID(paneID, builderUID)
		if err != nil {
			proof := FailHostedIsolationProofWithLifecycle(paneID, tabID, lc,
				fmt.Errorf("herdr agent start: hosted uid proof failed: %w", err))
			_ = launch.RecordRejected(req, nil, proof.Error())
			return proof
		}
		if err := AssertAgentHostedAsBuilder(name, builderUID, wantGID, kind, req.Decision.HarnessArgv); err != nil {
			proof := FailHostedIsolationProofWithLifecycle(paneID, tabID, lc,
				fmt.Errorf("herdr agent start: agent descendant uid proof failed: %w", err))
			_ = launch.RecordRejected(req, nil, proof.Error())
			return proof
		}
	}
	{
		if err := bindToolChildLifecycle(paneID, name, req); err != nil {
			if rollbackErr := rollbackToolChild(tabForPane(paneID), paneID, lc, "failed-bind"); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			_ = launch.RecordRejected(req, nil, err.Error())
			return err
		}
		if err := lc.Begin(); err != nil {
			if rollbackErr := rollbackToolChild(tabForPane(paneID), paneID, lc, "failed-inventory"); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			_ = launch.RecordRejected(req, nil, err.Error())
			return fmt.Errorf("tool-child inventory failed: %w", err)
		}
	}
	if err := launch.RecordStarted(req, nil); err != nil {
		if lc != nil {
			if rollbackErr := rollbackToolChild(tabForPane(paneID), paneID, lc, "launch-receipt-failed"); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
			return fmt.Errorf("launch receipt failed: %w; compensated exact launch", err)
		}
		cleanupErr := compensateStartedProcess(name)
		if cleanupErr != nil {
			return fmt.Errorf("launch receipt failed: %w; compensating unaccounted process failed: %v", err, cleanupErr)
		}
		return fmt.Errorf("launch receipt failed: %w; process stopped", err)
	}
	return nil
}

func compensateStartedProcess(name string) error {
	return compensateStartedProcessExact(name, "")
}

func compensateStartedProcessExact(name, paneID string) error {
	if strings.TrimSpace(name) == "" && strings.TrimSpace(paneID) == "" {
		return fmt.Errorf("exact compensation identity is required")
	}
	agents, err := AgentList()
	if err != nil {
		return err
	}
	matches := make([]AgentEntry, 0, 1)
	for _, a := range agents {
		if (name != "" && a.Name != name) || (paneID != "" && a.PaneID != paneID) {
			continue
		}
		matches = append(matches, a)
	}
	if len(matches) == 0 {
		return fmt.Errorf("cannot compensate launch %q: %w", name, ErrAgentNotFound)
	}
	if len(matches) != 1 {
		return fmt.Errorf("cannot compensate launch %q: identity is ambiguous (%d matches)", name, len(matches))
	}
	target := matches[0]
	if target.TabID == "" || target.PaneID == "" || target.Name == "" {
		return fmt.Errorf("cannot compensate launch %q: exact tab/name/pane identity is incomplete", name)
	}
	if err := tabCloseRaw(target.TabID); err != nil {
		return err
	}
	remaining, err := AgentList()
	if err != nil {
		return fmt.Errorf("verify compensated launch %q: %w", name, err)
	}
	for _, live := range remaining {
		if live.Name == target.Name && live.PaneID == target.PaneID && live.TabID == target.TabID {
			return fmt.Errorf("compensated launch %q remains present", name)
		}
	}
	return nil
}

func tabCloseRaw(tabID string) error {
	out, err := runHerdr("tab", "close", tabID)
	if err != nil {
		return fmt.Errorf("herdr tab close %s: %s: %w", tabID, out, err)
	}
	return nil
}

func agentStartProcess(name, kind, paneID string, agentArgs ...string) error {
	// small delay to let the pane shell initialize
	time.Sleep(500 * time.Millisecond)

	args := []string{"agent", "start", name, "--kind", kind, "--pane", paneID}
	// FAC-172: daemon-hosted uid flags only from negotiated capability.
	if HostedUIDIsolationRequired() {
		bUID, err := BuilderUID()
		if err != nil {
			return fmt.Errorf("herdr agent start: %w", err)
		}
		fa, err := agentStartUIDFlagArgs(bUID)
		if err != nil {
			return fmt.Errorf("herdr agent start: %w", err)
		}
		args = append(args, fa...)
	}
	if len(agentArgs) > 0 {
		args = append(args, "--")
		args = append(args, agentArgs...)
	}

	output, err := runHerdr(args...)
	// A freshly created tab's shell can take several seconds to become an
	// available target on a loaded host (observed: dispatch launch failing
	// with agent_pane_busy under swap pressure while the 1.5s retry gave up).
	// Back off up to ~12s before failing.
	for attempt := 0; err != nil && strings.Contains(string(output), "agent_pane_busy") && attempt < 6; attempt++ {
		time.Sleep(2 * time.Second)
		output, err = runHerdr(args...)
	}
	if err != nil {
		return fmt.Errorf("herdr agent start: %s: %w", output, err)
	}
	return nil
}

// StartReviewAgent starts a warm-pool reviewer only after the pane shell has
// become attachable, then proves that the named agent still owns the pane and
// has a foreground process. A created tab or a successful start command is
// not launch success. Any failed proof compensates the exact tab.
func StartReviewAgent(tabID, name, paneID, model string) error {
	if strings.TrimSpace(tabID) == "" || strings.TrimSpace(name) == "" || strings.TrimSpace(paneID) == "" {
		return fmt.Errorf("review launch identity requires tab, agent, and pane")
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("review launch model is required")
	}
	if err := agentStartProcess(name, "opencode", paneID, "--model", model, "--auto"); err != nil {
		return errors.Join(err, hardCloseTab(tabID, name))
	}
	if _, err := VerifyAgentLaunch(name, paneID, 15*time.Second); err != nil {
		return errors.Join(err, hardCloseTab(tabID, name))
	}
	return nil
}

// LaneAgentArgs builds the launch args a lane's config demands: the
// configured model, when set, always reaches the agent argv.
func LaneAgentArgs(model string) []string {
	if model == "" {
		return nil
	}
	return []string{"--model", model}
}

// AgentPrompt sends a prompt to a running agent via direct process argv.
// The free-form text is always one argv element — never shell-concatenated
// (FAC-183). If wait is true, herdr blocks until a settled state is observed.
// Prefer DeliverOperator (or herd herdr-deliver --file) when a durable
// content digest / readback receipt is required.
func AgentPrompt(target, text string, wait bool) (string, error) {
	if strings.IndexByte(text, 0) >= 0 {
		return "", fmt.Errorf("herdr agent prompt: payload contains NUL")
	}
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
	bin, err := binaryPath()
	if err != nil {
		return false
	}
	if filepath.IsAbs(bin) {
		fi, sErr := os.Stat(bin)
		return sErr == nil && !fi.IsDir()
	}
	_, err = exec.LookPath(bin)
	return err == nil
}

// AgentList returns all agents managed by herdr.
func AgentList() ([]AgentEntry, error) {
	output, err := runHerdr("agent", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr agent list: %w", err)
	}
	var resp struct {
		Result struct {
			Agents []AgentEntry `json:"agents"`
			Type   string       `json:"type"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parsing agent list: %s: %w", output, err)
	}
	if resp.Result.Agents == nil {
		return nil, fmt.Errorf("herdr agent list returned no agents inventory")
	}
	return resp.Result.Agents, nil
}

// AgentSession is optional provenance. Grok never reports it; claude/opencode
// fill it only after boot (after launch-time control bind).
type AgentSession struct {
	Source string `json:"source,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Value  string `json:"value,omitempty"`
}

// AgentEntry matches live herdr agent list rows. Revision and StateChangeSeq
// are mutable counters; TabGeneration, when present, is the immutable tab
// identity surface and must not be substituted with either counter. Optional
// FAC-158 observation fields are filled by authority adapters / fixtures,
// never invented from the raw agent list.
type AgentEntry struct {
	Name             string       `json:"name,omitempty"`
	Kind             string       `json:"agent,omitempty"`
	Status           string       `json:"agent_status,omitempty"`
	AssignmentStatus string       `json:"assignment_status,omitempty"`
	LoopMode         string       `json:"loop_mode,omitempty"`
	PaneID           string       `json:"pane_id,omitempty"`
	TabID            string       `json:"tab_id,omitempty"`
	Workspace        string       `json:"workspace_id,omitempty"`
	TerminalID       string       `json:"terminal_id,omitempty"`
	Cwd              string       `json:"cwd,omitempty"`
	TerminalTitle    string       `json:"terminal_title,omitempty"` // UI title (login/auth detection)
	ForegroundCwd    string       `json:"foreground_cwd,omitempty"`
	Session          AgentSession `json:"agent_session,omitempty"`
	Revision         uint64       `json:"revision,omitempty"`
	StateChangeSeq   uint64       `json:"state_change_seq,omitempty"`
	// TabGeneration is the immutable tab identity exposed by newer Herdr
	// pulse/agent-list surfaces. It is deliberately distinct from
	// StateChangeSeq, which only counts agent state transitions.
	TabGeneration uint64 `json:"tab_generation,omitempty"`
}

// SessionID renders the launch-time pane identity a receipt binds to.
// tab/pane alone name a reusable slot, so the incarnation token is part of
// the identity (FAC-145).
func SessionID(p PaneInfo) string {
	return fmt.Sprintf("%s/%s/%s", p.TabID, p.ID, p.TerminalID)
}

// splitSessionID parses a SessionID. A two-part (slot-only) id is refused:
// it cannot distinguish the launched agent from its replacement.
func splitSessionID(sessionID string) (tab, pane, terminal string, err error) {
	parts := strings.Split(sessionID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("malformed agent session id %q: want <tab>/<pane>/<terminal_id> (FAC-145: a slot-only id cannot prove incarnation)", sessionID)
	}
	return parts[0], parts[1], parts[2], nil
}

// findPane locates the live agent entry for an exact incarnation.
func findPaneIncarnation(tab, pane, terminal string) (*AgentEntry, error) {
	agents, err := AgentList()
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.TabID == tab && a.PaneID == pane && a.TerminalID == terminal {
			e := a
			return &e, nil
		}
	}
	return nil, nil
}

// PaneLiveCwd reads the LIVE foreground cwd herdr reports for the exact
// pane incarnation named by sessionID. The value returned by tab create is
// only the requested cwd echoed back; this is the terminal's actual state,
// which is what a cwd guarantee must rest on. Matching the incarnation too
// means a pane replaced between create and readback cannot answer for the
// pane we launched (FAC-145).
func PaneLiveCwd(sessionID string) (string, error) {
	tab, pane, terminal, err := splitSessionID(sessionID)
	if err != nil {
		return "", err
	}
	a, err := findPaneIncarnation(tab, pane, terminal)
	if err != nil {
		return "", err
	}
	if a == nil {
		return "", fmt.Errorf("herdr: no live pane %s", sessionID)
	}
	if a.ForegroundCwd != "" {
		return a.ForegroundCwd, nil
	}
	return a.Cwd, nil
}

// SessionExists reports whether "<tab>/<pane>/<terminal_id>" names a live
// herdr session. A replacement agent that took the same tab/pane reports a
// different terminal_id and therefore does NOT revive the dead agent's
// receipt authority (FAC-145).
func SessionExists(sessionID string) (bool, error) {
	tab, pane, terminal, err := splitSessionID(sessionID)
	if err != nil {
		return false, err
	}
	a, err := findPaneIncarnation(tab, pane, terminal)
	if err != nil {
		return false, err
	}
	return a != nil, nil
}

type PaneProcess struct {
	PID  int      `json:"pid"`
	Name string   `json:"name"`
	Cwd  string   `json:"cwd"`
	Argv []string `json:"argv"`
}

var readPIDArgv = systemPIDArgv

func SetPIDArgvReader(f func(int) ([]string, error)) func() {
	old := readPIDArgv
	if f == nil {
		readPIDArgv = systemPIDArgv
	} else {
		readPIDArgv = f
	}
	return func() { readPIDArgv = old }
}

func hydratePaneProcesses(processes []PaneProcess) ([]PaneProcess, []error) {
	hydrated := make([]PaneProcess, 0, len(processes))
	var readErrors []error
	for _, process := range processes {
		process.Argv = append([]string(nil), process.Argv...)
		if len(process.Argv) == 0 {
			if process.PID <= 0 {
				readErrors = append(readErrors, fmt.Errorf("process argv requires positive pid, got %d", process.PID))
			} else {
				argv, err := readPIDArgv(process.PID)
				if err != nil {
					readErrors = append(readErrors, fmt.Errorf("pid %d argv: %w", process.PID, err))
				} else if len(argv) == 0 {
					readErrors = append(readErrors, fmt.Errorf("pid %d argv is empty", process.PID))
				} else {
					process.Argv = append([]string(nil), argv...)
				}
			}
		}
		hydrated = append(hydrated, process)
	}
	return hydrated, readErrors
}

var readPIDStartToken = func(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("empty start token for pid %d", pid)
	}
	return token, nil
}

func SetPIDStartTokenReader(f func(int) (string, error)) func() {
	old := readPIDStartToken
	if f == nil {
		readPIDStartToken = func(pid int) (string, error) {
			out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(out)), nil
		}
	} else {
		readPIDStartToken = f
	}
	return func() { readPIDStartToken = old }
}

var readPIDParent = func(pid int) (int, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
	if err != nil {
		return 0, err
	}
	parent, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || parent <= 0 {
		return 0, fmt.Errorf("invalid parent pid for %d", pid)
	}
	return parent, nil
}

func SetPIDParentReader(f func(int) (int, error)) func() {
	old := readPIDParent
	if f == nil {
		readPIDParent = func(pid int) (int, error) {
			out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "ppid=").Output()
			if err != nil {
				return 0, err
			}
			parent, err := strconv.Atoi(strings.TrimSpace(string(out)))
			if err != nil || parent <= 0 {
				return 0, fmt.Errorf("invalid parent pid for %d", pid)
			}
			return parent, nil
		}
	} else {
		readPIDParent = f
	}
	return func() { readPIDParent = old }
}

func paneProcesses(paneID string) ([]PaneProcess, error) {
	output, err := runHerdr("pane", "process-info", "--pane", paneID)
	if err != nil {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			Result struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(output), &envelope) == nil && (envelope.Error.Code == "pane_not_found" || envelope.Result.Error.Code == "pane_not_found") {
			return nil, ErrPaneNotFound
		}
		return nil, fmt.Errorf("herdr pane process-info: %w", err)
	}
	var envelope struct {
		Result struct {
			ProcessInfo struct {
				ForegroundProcesses []PaneProcess `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		return nil, err
	}
	if envelope.Result.ProcessInfo.ForegroundProcesses == nil {
		return nil, fmt.Errorf("pane process-info returned no process inventory")
	}
	return envelope.Result.ProcessInfo.ForegroundProcesses, nil
}

func exactArgv(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

func piEntrypoint(value string) bool {
	clean := filepath.ToSlash(strings.ToLower(filepath.Clean(value)))
	base := filepath.Base(clean)
	return base == "pi" || (base == "cli.js" && strings.Contains(clean, "/pi-coding-agent/"))
}

func piProcessTitleCandidate(p PaneProcess) bool {
	if filepath.Base(strings.ToLower(p.Name)) != "node" || len(p.Argv) == 0 || filepath.Base(strings.ToLower(p.Argv[0])) != router.PiHarness {
		return false
	}
	for _, arg := range p.Argv[1:] {
		if arg != "" {
			return false
		}
	}
	return true
}

func routedProcessCandidates(provider string, routed []string, sessionPath string, processes []PaneProcess) ([]PaneProcess, []PaneProcess, string, error) {
	var exact, titled, wrappers []PaneProcess
	for _, p := range processes {
		if nativeCandidate(provider, routed, p) {
			exact = append(exact, p)
		} else if strings.EqualFold(provider, router.PiHarness) && piProcessTitleCandidate(p) {
			titled = append(titled, p)
		}
		if wrapperCandidate(p) {
			wrappers = append(wrappers, p)
		}
	}
	if strings.EqualFold(provider, router.PiHarness) && (len(exact) > 0 || len(titled) > 0) {
		if err := verifyPiSessionRoute(sessionPath, routed); err != nil {
			if errors.Is(err, ErrPiSessionNotReady) {
				return nil, wrappers, err.Error(), nil
			}
			return nil, nil, "", err
		}
	}
	exact = append(exact, titled...)
	return exact, wrappers, "", nil
}

func nativeCandidate(provider string, routed []string, p PaneProcess) bool {
	if len(routed) < 1 || len(p.Argv) < 1 {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	argv0 := filepath.Base(strings.ToLower(p.Argv[0]))
	name := filepath.Base(strings.ToLower(p.Name))
	if provider == router.PiHarness {
		if argv0 == "node" {
			return len(p.Argv) >= 2 && piEntrypoint(p.Argv[1]) && exactArgv(routed[1:], p.Argv[2:])
		}
		if argv0 != router.PiHarness {
			return false
		}
		return exactArgv(routed[1:], p.Argv[1:])
	}
	if argv0 != provider && name != provider {
		return false
	}
	return exactArgv(routed[1:], p.Argv[1:])
}

func wrapperCandidate(p PaneProcess) bool {
	return len(p.Argv) > 0 && filepath.Base(strings.ToLower(p.Argv[0])) == "node"
}

var (
	ErrAgentNotFound         = errors.New("herdr agent not found")
	ErrAgentIdentityMismatch = errors.New("herdr agent identity mismatch")
	ErrPaneNotFound          = errors.New("herdr pane not found")
)

// ResolveAgentTab finds a standing agent by name and returns its tab label.
// Returns an error if no agent with that name exists.
func ResolveAgentTab(name string) (string, error) {
	agents, err := AgentList()
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Name == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("no standing agent named '%s' found: %w", name, ErrAgentNotFound)
}

// ResolveAgentTabWithDecision is the standing/resume trust boundary. A newly
// computed route never proves an existing process: herdr must report the same
// durable role, task identity, lease, provider, model, effort, and shape.
func ResolveAgentTabWithDecision(name string, req launch.Request) (string, error) {
	if err := launch.Validate(req, nil); err != nil {
		return "", err
	}
	if req.Decision == nil {
		return "", fmt.Errorf("resume requires a routed decision")
	}
	agents, err := AgentList()
	if err != nil {
		return "", err
	}
	for _, a := range agents {
		if a.Name != name {
			continue
		}
		req.Name, req.PaneID = name, a.PaneID
		generation, err := recoverStandingLifecycle(a, req)
		if err != nil {
			return "", err
		}
		req.SessionGeneration = generation
		ok, err := launch.HasStarted(req)
		if err != nil {
			return "", fmt.Errorf("resume lifecycle lookup: %w", err)
		}
		if !ok {
			return "", fmt.Errorf("standing agent %q has no matching durable launch identity: %w", name, ErrAgentIdentityMismatch)
		}
		if err := reconcileRecoveredToolChild(a.TabID, "recovery", generation); err != nil {
			return "", fmt.Errorf("resume tool-child recovery: %w", err)
		}
		if _, _, err := ReconcileAgentLabel(a); err != nil {
			return "", fmt.Errorf("resume label reconciliation: %w", err)
		}
		return name, nil
	}
	return "", fmt.Errorf("no standing agent named '%s' found: %w", name, ErrAgentNotFound)
}

// EnsureHerdforgeLabel prefixes the label with "Herdforge · " if it does not
// already start with that prefix, or with the older "Herdforge | " prefix
// (FAC-199 accepts either as already-canonical and never rewrites one form
// to the other). HasPrefix (not Contains) is required so a mid-string match
// such as "review of Herdforge · thing" still gets prefixed.
func EnsureHerdforgeLabel(label string) string {
	if strings.HasPrefix(label, herdforgeLabel) || strings.HasPrefix(label, herdforgeLabelPipe) {
		return label
	}
	return herdforgeLabel + label
}

func runHerdrReal(args ...string) (string, error) {
	bin, binErr := binaryPath()
	if binErr != nil {
		return "", binErr
	}
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Fold herdr's stderr into the error. Returning a bare
		// *exec.ExitError leaves every caller reporting "exit status 1"
		// with no cause, which makes a failed launch undiagnosable.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			return "", err
		}
		return msg, fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.String(), nil
}

// TabCreateForTaskEnv is the FAC-133 process-boundary tab create: requires
// workspace + cwd and injects only the provided KEY=VALUE env pairs (never
// the parent ambient environment).
//
// FAC-172: same absolute-cwd gate and HostedUID isolation as TabCreateForTask.
// Control-plane launches (HERD_CONTROL_SECRET / launcherSpawner) must not skip
// tab-level hosted_uid or AgentStart/AssertHostedPaneUID fails closed against
// a non-hosted shell.
func TabCreateForTaskEnv(workspaceID, label, cwd string, env []string, noFocus bool) (*TabInfo, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil, fmt.Errorf("herdr tab create: cwd is required for task agents")
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: resolve cwd %q: %w", cwd, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: cwd %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("herdr tab create: cwd %q is not a directory", abs)
	}
	opts := TabCreateOptions{
		Workspace: workspaceID,
		Label:     label,
		Cwd:       abs,
		Env:       bindChildWorkspaceEnv(abs, workspaceID, env),
		NoFocus:   noFocus,
	}
	if HostedUIDIsolationRequired() {
		bUID, err := BuilderUID()
		if err != nil {
			return nil, fmt.Errorf("herdr tab create: %w", err)
		}
		if err := RequireHerdrBuilderSpawnCapability(bUID); err != nil {
			return nil, fmt.Errorf("herdr tab create: %w", err)
		}
		opts.HostedUID = bUID
		opts.Env = append(opts.Env, hostedUIDLaunchEnv(bUID)...)
	}
	return TabCreate(opts)
}

// LookupAgent returns the live Herdr agent entry for name.
// Returns ErrAgentNotFound when absent (distinct from list/parse errors).
func LookupAgent(name string) (*AgentEntry, error) {
	agents, err := AgentList()
	if err != nil {
		return nil, err
	}
	for i := range agents {
		if agents[i].Name == name {
			return &agents[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, name)
}

// LoginOrAuthScreen reports whether title/body indicates the harness is stuck
// on browser login / device auth and is NOT a model/tool session.
func LoginOrAuthScreen(title, body string) bool {
	s := strings.ToLower(title + "\n" + body)
	needles := []string{
		"browser-login", "browser login", "log in", "sign in", "sign-in",
		"login to continue", "authenticate", "authorization", "device code",
		"visit https://", "open the following url", "auth0", "oauth",
		"chatgpt.com", "platform.openai.com", "please log in", "login required",
		"not logged in", "api key", "enter your api key", "press enter to open",
		"trust this folder", "trust this workspace", "allow access", "consent required",
	}
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// RealModelSessionID is true only for a non-provisional, non-terminal-fallback
// agent_session value emitted by herdr for a real model session.
func RealModelSessionID(sid string) bool {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return false
	}
	if strings.HasPrefix(sid, "pending-") ||
		strings.HasPrefix(sid, "ses_probe_") ||
		strings.HasPrefix(sid, "ses_real_") ||
		strings.HasPrefix(sid, "ses_spawn_") ||
		strings.HasPrefix(sid, "test-session-") ||
		strings.HasPrefix(sid, "herdr-term:") ||
		strings.HasPrefix(sid, "herdr-pane:") {
		return false
	}
	return true
}
