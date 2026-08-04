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
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

const (
	herdrCLI       = "herdr"
	herdforgeLabel = "Herdforge · "
)

// runHerdr is overridable for crash-point / unit tests (FAC-121).
var runHerdr = runHerdrReal

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

func PrepareToolChildLifecycle(tabID, paneID string, req launch.Request, name string) error {
	if tabID == "" || paneID == "" {
		return fmt.Errorf("tool-child lifecycle requires tab and pane")
	}
	if req.Decision == nil || req.TaskRef == "" || req.Repository == "" || req.Lane == "" || (req.Scope != "lane" && req.LeaseGeneration <= 0) {
		return fmt.Errorf("complete launch identity is required before lifecycle preparation")
	}
	if req.SessionGeneration <= 0 {
		var err error
		req.SessionGeneration, err = toolchild.NextSessionGeneration(req.Repository)
		if err != nil {
			return fmt.Errorf("durable Herdr session generation: %w", err)
		}
	}
	toolChildMu.Lock()
	// A reserved identity is occupied even while provisional or test-injected;
	// replacing it would orphan durable authority and race a concurrent launch.
	if toolChildByTab[tabID] != nil || toolChildByPane[paneID] != nil {
		toolChildMu.Unlock()
		return fmt.Errorf("%w: tab %s/pane %s", toolchild.ErrLifecycleCollision, tabID, paneID)
	}
	f := newToolChildLifecycle
	lc, err := f(req, name, paneID)
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
		concrete.SetContext(toolchild.Identity{TabID: tabID, PaneID: paneID, Name: name, SessionGeneration: req.SessionGeneration, LaunchID: launch.DecisionDigest(req.Decision), Repository: req.Repository, Lane: req.Lane, Role: string(req.Decision.Role), TaskRef: req.TaskRef, Provider: req.Decision.Provider, ArgvDigest: launch.DecisionDigest(req.Decision), Argv: append([]string(nil), req.Decision.Argv...)})
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
	if err := PrepareToolChildLifecycle(tabID, paneID, req, name); err != nil {
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
	}
	if err := compensateStartedProcessExact("", paneID); err != nil {
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

func tabList() ([]string, error) {
	output, err := runHerdr("tab", "list")
	if err != nil {
		return nil, fmt.Errorf("herdr tab list: %s: %w", output, err)
	}
	var envelope struct {
		Result struct {
			Tabs []struct {
				ID    string `json:"id"`
				TabID string `json:"tab_id"`
			} `json:"tabs"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		return nil, err
	}
	if envelope.Result.Tabs == nil {
		return nil, fmt.Errorf("herdr tab list returned no tabs inventory")
	}
	ids := make([]string, 0, len(envelope.Result.Tabs))
	for _, t := range envelope.Result.Tabs {
		id := t.TabID
		if id == "" {
			id = t.ID
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func bindToolChildLifecycle(paneID, name string, req launch.Request) error {
	lc := lifecycleForPane(paneID)
	if lc == nil {
		return nil
	}
	agents, err := AgentList()
	if err != nil {
		return fmt.Errorf("tool-child owner lookup: %w", err)
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
	for _, a := range agents {
		if a.Name != name || a.PaneID != paneID || a.Session.Value == "" || a.Kind != req.Decision.Provider {
			continue
		}
		processes, err := paneProcesses(paneID)
		if err != nil {
			return err
		}
		var matches, wrappers []PaneProcess
		for _, p := range processes {
			if nativeCandidate(req.Decision.Provider, req.Decision.Argv, p) {
				matches = append(matches, p)
			}
			if wrapperCandidate(p) {
				wrappers = append(wrappers, p)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("pane %s has %d exact routed agent processes", paneID, len(matches))
		}
		p := matches[0]
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
		if strings.EqualFold(req.Decision.Provider, "codex") {
			if len(wrappers) != 1 || parent != wrappers[0].PID {
				return fmt.Errorf("codex native process requires exactly one node wrapper parent")
			}
		} else if len(wrappers) > 1 {
			return fmt.Errorf("provider process has ambiguous node wrappers")
		}
		role := ""
		if req.Decision != nil {
			role = string(req.Decision.Role)
		}
		if req.Repository == "" || req.Lane == "" {
			return fmt.Errorf("repository and lane binding are required")
		}
		if a.TabID != "" && a.TabID != tabForPane(paneID) {
			return fmt.Errorf("prepared tab identity drift: got %s", a.TabID)
		}
		if a.TabID == "" {
			return fmt.Errorf("agent list returned missing tab identity")
		}
		return lc.Bind(toolchild.Identity{PID: p.PID, ParentPID: parent, StartToken: token, SessionGeneration: sessionGeneration, LaunchID: launch.DecisionDigest(req.Decision), Repository: req.Repository, Role: role, Lane: req.Lane, TaskRef: req.TaskRef, Name: name, SessionID: a.Session.Value, PaneID: paneID, TabID: a.TabID, Provider: req.Decision.Provider, ArgvDigest: launch.DecisionDigest(req.Decision), Argv: append([]string(nil), req.Decision.Argv...)})
	}
	return fmt.Errorf("tool-child owner identity unavailable for %s/%s", name, paneID)
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
	if owner.TabID == "" || owner.PaneID == "" || owner.Provider == "" || len(owner.Argv) == 0 || owner.ArgvDigest == "" || owner.Repository == "" {
		return fmt.Errorf("recovery provisional identity is incomplete")
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
	var native []PaneProcess
	var wrappers []PaneProcess
	for _, p := range processes {
		if nativeCandidate(owner.Provider, owner.Argv, p) {
			native = append(native, p)
		}
		if wrapperCandidate(p) {
			wrappers = append(wrappers, p)
		}
	}
	if len(native) != 1 {
		return fmt.Errorf("recovery routed owner candidates=%d", len(native))
	}
	p := native[0]
	if strings.EqualFold(owner.Provider, "codex") {
		parent, err := readPIDParent(p.PID)
		if err != nil || len(wrappers) != 1 || parent != wrappers[0].PID {
			return fmt.Errorf("recovery codex wrapper ancestry is not exact")
		}
	} else if len(wrappers) > 1 {
		return fmt.Errorf("recovery provider wrapper ancestry is ambiguous")
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
			return fmt.Errorf("tool-child cleanup blocked: %w", err)
		}
		toolChildMu.Lock()
		toolChildByTab[tabID] = lc
		if concrete, ok := lc.(*toolchild.Lifecycle); ok {
			toolChildByPane[concrete.Inventory.Owner.PaneID] = lc
		}
		toolChildMu.Unlock()
	}
	if concrete, ok := lc.(*toolchild.Lifecycle); ok && !concrete.Bound() {
		if err := bindRecoveredToolChildLifecycle(concrete); err != nil {
			return fmt.Errorf("tool-child pre-bind recovery blocked: %w", err)
		}
	}
	return lc.Reconcile(event)
}

type TabInfo struct {
	ID    string
	Label string
	Pane  PaneInfo
	Cwd   string // process cwd requested at tab create (empty for legacy Tab)
}

type PaneInfo struct {
	ID    string
	TabID string
}

// TabCreateOptions is the fail-closed tab launch contract (FAC-121).
// Workspace and Cwd are required; unknown workspace must not fall back.
type TabCreateOptions struct {
	Workspace string
	Label     string
	Cwd       string
	NoFocus   bool
	Env       []string // optional KEY=VALUE pairs
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
	output, err := runHerdr(args...)
	if err != nil {
		return nil, fmt.Errorf("herdr tab create: %w", err)
	}

	var resp struct {
		Result struct {
			Tab struct {
				TabID string `json:"tab_id"`
				Label string `json:"label"`
			} `json:"tab"`
			RootPane struct {
				PaneID string `json:"pane_id"`
				TabID  string `json:"tab_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("parsing tab create output: %s: %w", output, err)
	}

	return &TabInfo{
		ID:    resp.Result.Tab.TabID,
		Label: resp.Result.Tab.Label,
		Cwd:   opts.Cwd,
		Pane: PaneInfo{
			ID:    resp.Result.RootPane.PaneID,
			TabID: resp.Result.RootPane.TabID,
		},
	}, nil
}

// TabCreateForTask is the FAC-121 launch entry: requires workspace and cwd.
// Rejects empty cwd so shared-root / unknown-directory starts cannot slip through.
func TabCreateForTask(workspaceID, label, cwd string, noFocus bool) (*TabInfo, error) {
	if strings.TrimSpace(cwd) == "" {
		return nil, fmt.Errorf("herdr tab create: cwd is required for task agents")
	}
	return TabCreate(TabCreateOptions{
		Workspace: workspaceID,
		Label:     label,
		Cwd:       cwd,
		NoFocus:   noFocus,
	})
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
	if req.Decision == nil || strings.TrimSpace(kind) != strings.TrimSpace(req.Decision.Provider) {
		return compensateValidation(launch.RecordRejected(req, nil, fmt.Sprintf("herdr kind %q does not match decision provider", kind)))
	}
	if req.Decision == nil || len(req.Decision.Argv) == 0 {
		return compensateValidation(fmt.Errorf("launch decision argv is empty"))
	}
	if req.SessionGeneration <= 0 {
		if concrete, ok := lc.(*toolchild.Lifecycle); ok {
			req.SessionGeneration = concrete.Inventory.Owner.SessionGeneration
		}
	}
	if req.SessionGeneration <= 0 {
		return fmt.Errorf("durable Herdr session generation is unavailable")
	}
	if lc == nil {
		return fmt.Errorf("prepared tool-child lifecycle is required before process start")
	}
	if err := agentStartProcess(name, kind, paneID, req.Decision.Argv[1:]...); err != nil {
		if rollbackErr := rollbackToolChild(tabForPane(paneID), paneID, lc, "failed-launch"); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		_ = launch.RecordRejected(req, nil, err.Error())
		return err
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

// LaneAgentArgs builds the launch args a lane's config demands: the
// configured model, when set, always reaches the agent argv.
func LaneAgentArgs(model string) []string {
	if model == "" {
		return nil
	}
	return []string{"--model", model}
}

// AgentPrompt sends a prompt to a running agent. If wait is true, blocks for response.
func AgentPrompt(target, text string, wait bool) (string, error) {
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
	_, err := exec.LookPath(herdrCLI)
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

// AgentEntry represents a single herdr-managed agent.
type AgentEntry struct {
	Name      string `json:"name,omitempty"`
	Kind      string `json:"agent,omitempty"`
	Status    string `json:"agent_status,omitempty"`
	PaneID    string `json:"pane_id,omitempty"`
	TabID     string `json:"tab_id,omitempty"`
	Workspace string `json:"workspace_id,omitempty"`
	Session   struct {
		Value string `json:"value,omitempty"`
	} `json:"agent_session,omitempty"`
}

type PaneProcess struct {
	PID  int      `json:"pid"`
	Name string   `json:"name"`
	Cwd  string   `json:"cwd"`
	Argv []string `json:"argv"`
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

func nativeCandidate(provider string, routed []string, p PaneProcess) bool {
	if len(routed) < 1 || len(p.Argv) < 1 {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if filepath.Base(strings.ToLower(p.Argv[0])) != provider && filepath.Base(strings.ToLower(p.Name)) != provider {
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
		ok, err := launch.HasStarted(req)
		if err != nil {
			return "", fmt.Errorf("resume lifecycle lookup: %w", err)
		}
		if !ok {
			return "", fmt.Errorf("standing agent %q has no matching durable launch identity: %w", name, ErrAgentIdentityMismatch)
		}
		if err := ReconcileToolChild(a.TabID, "recovery"); err != nil {
			return "", fmt.Errorf("resume tool-child recovery: %w", err)
		}
		return name, nil
	}
	return "", fmt.Errorf("no standing agent named '%s' found: %w", name, ErrAgentNotFound)
}

// EnsureHerdforgeLabel prefixes the label with "Herdforge · " if it does not
// already start with that prefix. HasPrefix (not Contains) is required so a
// mid-string match such as "review of Herdforge · thing" still gets prefixed.
func EnsureHerdforgeLabel(label string) string {
	if strings.HasPrefix(label, herdforgeLabel) {
		return label
	}
	return herdforgeLabel + label
}

func runHerdrReal(args ...string) (string, error) {
	cmd := exec.Command(herdrCLI, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stderr.String()), err
	}
	return stdout.String(), nil
}
