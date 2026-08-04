package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolchild"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// AuthenticatedRepositoryIdentity is the sole production repository binding
// used by launch/lifecycle authority. The config project name is display-only.
func AuthenticatedRepositoryIdentity(root string) (string, error) {
	return authenticatedRepositoryIdentity(root)
}

var authenticatedRepositoryIdentity = toolchild.RepositoryIdentity

func (d *Dispatcher) repositoryIdentity() (string, error) {
	root := "."
	if d.Worktree != nil && d.Worktree.RepoRoot() != "" {
		root = d.Worktree.RepoRoot()
	}
	id, err := AuthenticatedRepositoryIdentity(root)
	if err == nil {
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("authenticated repository identity is empty")
		}
		return id, nil
	}
	if d.Production {
		return "", err
	}
	return "test-repository:" + filepath.Clean(root), nil
}

// LaunchStep names a partial side-effect during dispatch (FAC-121).
// FAC-119's transactional outbox must persist these for crash recovery.
type LaunchStep string

const (
	StepWorktree      LaunchStep = "worktree"
	StepBoardProgress LaunchStep = "board_in_progress"
	StepBoardComment  LaunchStep = "board_comment"
	StepTab           LaunchStep = "tab"
	StepAgentStart    LaunchStep = "agent_start"
	StepPrompt        LaunchStep = "prompt"
)

// Compensator is the mandatory durable side-effect interface for production
// dispatch (FAC-121 R3). FAC-119 lifecycle/outbox and FAC-120 lease fencing
// implement this. Nil is rejected fail-closed — there is no best-effort path.
type Compensator interface {
	// RecordStep persists a successful side-effect (idempotent by step key).
	// Errors must propagate; callers fail closed.
	RecordStep(ctx context.Context, rec StepRecord) error
	// Compensate undoes or marks Recovering for partial launch state.
	// reason should be stable for outbox replay. Errors must propagate.
	Compensate(ctx context.Context, ticketRef, reason string) error
}

// StepRecord is one outbox-ready launch side-effect.
type StepRecord struct {
	TicketRef string
	Step      LaunchStep
	Worktree  string
	Branch    string
	BaseSHA   string
	AnchorRef string
	TabID     string
	PaneID    string
	AgentName string
	Receipt   string // prompt sequence token when step is prompt
	MessageID string // durable control envelope identity
	Sequence  int64  // durable mailbox sequence
}

// HerdrLauncher isolates herdr operations for crash-point tests (FAC-121).
// DeliverAndProve always requires consumption proof (no verify bypass).
type HerdrLauncher interface {
	Available() bool
	RequireWorkspace(repoRoot string) (string, error)
	TabCreateForTask(workspaceID, label, cwd string, noFocus bool) (*herdr.TabInfo, error)
	AgentStart(req launch.Request, name, kind, paneID string) error
	DeliverAndProve(target, text string, timeout time.Duration) (*herdr.PromptReceipt, error)
	TabClose(tabID string) error
	ResolveHealthyModel(ctx context.Context, primary string, fallbacks []string) (string, []herdr.ProbeResult)
}

// LiveHerdr is the production HerdrLauncher.
type LiveHerdr struct{}

func (LiveHerdr) Available() bool { return herdr.IsAvailable() }
func (LiveHerdr) RequireWorkspace(repoRoot string) (string, error) {
	return herdr.RequireWorkspace(repoRoot)
}
func (LiveHerdr) TabCreateForTask(workspaceID, label, cwd string, noFocus bool) (*herdr.TabInfo, error) {
	return herdr.TabCreateForTask(workspaceID, label, cwd, noFocus)
}
func (LiveHerdr) AgentStart(req launch.Request, name, kind, paneID string) error {
	return herdr.AgentStartWithDecision(name, kind, paneID, req)
}
func (LiveHerdr) DeliverAndProve(target, text string, timeout time.Duration) (*herdr.PromptReceipt, error) {
	return herdr.DeliverAndProve(target, text, timeout)
}
func (LiveHerdr) TabClose(tabID string) error { return herdr.TabClose(tabID) }
func (LiveHerdr) ResolveHealthyModel(ctx context.Context, primary string, fallbacks []string) (string, []herdr.ProbeResult) {
	return herdr.ResolveHealthyModel(ctx, primary, fallbacks)
}

// ValidateControlTarget binds a wake to the exact tab/pane/agent returned by
// AgentStart.  A label alone is not a stable Herdr destination.
func (LiveHerdr) ReadControlTarget(target control.WakeTarget) (control.WakeTarget, error) {
	agents, err := herdr.AgentList()
	if err != nil {
		return control.WakeTarget{}, err
	}
	for _, a := range agents {
		if a.TabID == target.TabID && a.PaneID == target.PaneID && a.Name == target.AgentName && a.Workspace == target.Workspace && a.Kind == target.Provider && a.Session.Value != "" {
			target.SessionID = a.Session.Value
			return target, nil
		}
	}
	return control.WakeTarget{}, fmt.Errorf("launched Herdr tab/pane/agent/session is no longer current")
}

type DispatchOptions struct {
	TicketRef string
	Decision  *router.LaunchDecision
	NoLaunch  bool
	LaneName  string
	// PromptVerifyTimeout bounds DeliverAndProve polling (default 60s).
	// Production launches always require consumption proof — there is no
	// SkipPromptVerify bypass (FAC-121 R3 repair).
	PromptVerifyTimeout time.Duration
}

type DispatchResult struct {
	TicketRef       string
	TicketTitle     string
	Worktree        string
	Branch          string
	BaseSHA         string
	AnchorRef       string
	TaskPacket      string
	Launched        bool
	Lane            string
	LeaseGeneration int64
	Model           string // the model that actually launched (after fallback)
	TabID           string
	AgentName       string
	Receipt         *herdr.PromptReceipt
}

// WorktreeService is the isolation surface Dispatch uses (FAC-121).
// *worktree.WorktreeManager is adapted via NewDispatcher; tests must inject
// a temp-repo manager or a mock — never a manager rooted at the package cwd
// or shared checkout (avoids herd/fac-* pollution under pkg/dispatch/.herd).
type WorktreeService interface {
	CreateTaskWorktreeFrom(ctx context.Context, taskRef, defaultBranch string) (*worktree.WorktreeInfo, error)
	// RepoRoot returns the shared repository root for shared-root denial.
	RepoRoot() string
}

// liveWorktree adapts *worktree.WorktreeManager to WorktreeService.
type liveWorktree struct{ m *worktree.WorktreeManager }

func (l liveWorktree) CreateTaskWorktreeFrom(ctx context.Context, taskRef, defaultBranch string) (*worktree.WorktreeInfo, error) {
	return l.m.CreateTaskWorktreeFrom(ctx, taskRef, defaultBranch)
}
func (l liveWorktree) RepoRoot() string {
	if l.m == nil {
		return ""
	}
	return l.m.RepoRoot
}

type Dispatcher struct {
	Config       *config.Config
	TaskProvider provider.TaskProvider
	Worktree     WorktreeService
	// Compensator is required for every Dispatch call (FAC-121 R3).
	// Wire FAC-119 durable outbox / FAC-120 fenced lease compensator here.
	Compensator Compensator
	// Orders is the durable coordinator-to-lane control port. When configured,
	// every repair prompt is persisted before Herdr is nudged.
	Orders         *control.CoordinatorOrders
	Production     bool
	ControlFactory func(context.Context, ControlScope) (*control.CoordinatorOrders, error)
	// Herdr is optional; defaults to LiveHerdr.
	Herdr HerdrLauncher
	// PromptVerifyTimeout overrides the delivery poll window.
	PromptVerifyTimeout time.Duration
	// Deps is the relation store for the FAC-159 pre-side-effect gate.
	// When nil, constructed from TaskProvider via deps.StoreFor.
	Deps deps.RelationStore
	// Ownership is a durable cross-process lease claimer (claim.ClaimManager +
	// SQLite). When nil, opened at .herd/launch-claims.db under RepoRoot.
	Ownership deps.OwnershipClaimer

	// health projects BLOCKED(provider_timeout) for board calls (FAC-150).
	health dispatchHealth
}

// ControlScope is constructed after AgentStart, so each order is bound to the
// exact task-scoped lease and launched Herdr identity.
type ControlScope struct {
	Identity control.LaneIdentity
	Wake     control.WakeTarget
	Check    func(context.Context, control.Order) error
}

func NewDispatcher(cfg *config.Config, tp provider.TaskProvider, wm *worktree.WorktreeManager) *Dispatcher {
	d := &Dispatcher{
		Config:       cfg,
		TaskProvider: tp,
		Worktree:     liveWorktree{m: wm},
		Herdr:        LiveHerdr{},
		health:       dispatchHealth{state: ProviderOK},
	}
	if tp != nil && cfg != nil {
		g, l, m, c, r, err := cfg.TaskProvider.Deadlines.Resolved()
		if err == nil {
			provider.ApplyDeadlines(tp, provider.DeadlinesFromParts(g, l, m, c, r))
		} else {
			provider.ApplyDeadlines(tp, provider.DefaultDeadlines())
		}
	}
	return d
}

func NewProductionDispatcher(cfg *config.Config, tp provider.TaskProvider, wm *worktree.WorktreeManager) *Dispatcher {
	d := NewDispatcher(cfg, tp, wm)
	d.Production = true
	return d
}

func (d *Dispatcher) ownershipClaimer() (deps.OwnershipClaimer, error) {
	if d.Ownership != nil {
		return d.Ownership, nil
	}
	root := ""
	if d.Worktree != nil {
		root = d.Worktree.RepoRoot()
	}
	if root == "" {
		root = "."
	}
	repo, err := d.repositoryIdentity()
	if err != nil {
		return nil, fmt.Errorf("dispatch: repository identity: %w", err)
	}
	providerType := "memory"
	project := ""
	if d.Config != nil {
		providerType = d.Config.TaskProvider.Type
		project = d.Config.TaskProvider.ProjectID
	}
	ownership, err := deps.OpenLeaseOwnership(deps.ResolveLaunchLeasePath(root), repo, providerType, project)
	if err != nil {
		return nil, err
	}
	if d.Config != nil {
		ownership.LaneResolver = func(role string) (string, error) {
			for _, lane := range d.Config.Lanes {
				if lane.Role == role {
					return lane.Name, nil
				}
			}
			return "", fmt.Errorf("unknown configured role %q", role)
		}
	}
	return ownership, nil
}

func (d *Dispatcher) launcher() HerdrLauncher {
	if d.Herdr != nil {
		return d.Herdr
	}
	return LiveHerdr{}
}

// requireCompensator fails closed when durable outbox hooks are missing.
func (d *Dispatcher) requireCompensator() error {
	if d.Compensator == nil {
		return fmt.Errorf("dispatch compensator is required (FAC-121 fail-closed; wire FAC-119 durable outbox / FAC-120 fencing — nil compensator is not allowed on the production path)")
	}
	return nil
}

func (d *Dispatcher) record(ctx context.Context, rec StepRecord) error {
	if err := d.requireCompensator(); err != nil {
		return err
	}
	if err := d.Compensator.RecordStep(ctx, rec); err != nil {
		return fmt.Errorf("durable RecordStep(%s) failed: %w", rec.Step, err)
	}
	return nil
}

func (d *Dispatcher) compensate(ctx context.Context, ticketRef, reason string) error {
	if err := d.requireCompensator(); err != nil {
		return err
	}
	if err := d.Compensator.Compensate(ctx, ticketRef, reason); err != nil {
		return fmt.Errorf("durable Compensate(%s) failed: %w", reason, err)
	}
	return nil
}

// Durable shared-lifecycle compensation for launch failures is owned exclusively
// by Dispatch.failOwned (StillOwns → exactly-one compensate → ReleaseIfOwner).
// Do not reintroduce failWithCompensate/rollbackTab helpers that compensate
// without a generation lease or double-fire with outer failOwned
// (audit h5d6pay5vamxvv277qtt5qmk).

func (d *Dispatcher) Dispatch(ctx context.Context, opts DispatchOptions) (*DispatchResult, error) {
	// Fail closed before any side effect when durable hooks are missing.
	if err := d.requireCompensator(); err != nil {
		return nil, err
	}
	if d.Production && !opts.NoLaunch && d.ControlFactory == nil {
		return nil, fmt.Errorf("dispatch: durable control factory is required for production launch")
	}
	// FAC-175: reject an under-specified worker launch before even reading or
	// mutating provider/worktree state. --no-launch is the explicit packet-only
	// mode and therefore has no launch boundary to validate.
	if !opts.NoLaunch {
		if _, err := validateWorkerLaunchRequest(opts); err != nil {
			return nil, err
		}
	}

	// 1. Fetch ticket from Kaneo (bounded context + health observe, FAC-150)
	// READ-ONLY — no worktree/status/comment/tab yet (FAC-159).
	tasks, err := d.listTasksBound(ctx, d.Config.TaskProvider.ProjectID, "")
	if err != nil {
		return nil, formatBoardErr("failed to list tasks", err)
	}

	var task *provider.Task
	for _, t := range tasks {
		if t.Ref == opts.TicketRef {
			task = t
			break
		}
	}
	if task == nil {
		return nil, fmt.Errorf("ticket %s not found", opts.TicketRef)
	}

	// 2. Determine lane (still no side effects).
	laneName := opts.LaneName
	if laneName == "" {
		laneName = "worker"
	}
	var lane *config.LaneDef
	for i := range d.Config.Lanes {
		if d.Config.Lanes[i].Name == laneName {
			lane = &d.Config.Lanes[i]
			break
		}
	}
	if lane == nil {
		return nil, fmt.Errorf("lane '%s' not found in config", laneName)
	}

	defaultBranch := d.Config.Project.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = worktree.DefaultBranch
	}

	// 2b. FAC-159 PRE-SIDE-EFFECT fence: selection → re-read bound to revision
	// → exclusive ownership claim BEFORE any worktree/status/comment/tab.
	// SnapshotFence: one project-graph hydration + immutable reuse for pre/post
	// checks; post TOCTOU is O(1) incident ListRelations (not full re-fanout).
	ctx, _ = deps.WithSnapshotFence(ctx)
	var depProv *deps.Provenance
	store := d.Deps
	if store == nil {
		store = deps.StoreFor(d.TaskProvider, d.Config.TaskProvider.ProjectID)
	}
	// Provenance authority is description fence only (no sidecar / second store).
	desired, perr := deps.ExtractProvenanceFromText(task.Description)
	if perr != nil {
		return nil, fmt.Errorf("dispatch dependency provenance: %w", perr)
	}
	if desired == nil || !desired.Present {
		return nil, fmt.Errorf("dispatch: %w for %s (attach herd-deps-v1 description fence or coordinator-run herd deps migrate --apply)", deps.ErrMissingProvenance, task.Ref)
	}
	if berr := desired.BindAndValidate(deps.Ref(task.Ref), deps.TaskID(task.ID)); berr != nil {
		return nil, fmt.Errorf("dispatch provenance bind: %w", berr)
	}
	sel, serr := deps.RequireTaskLaunch(ctx, store, deps.EntryDispatch, deps.Ref(task.Ref), desired, "")
	if serr != nil {
		return nil, serr
	}
	pre, perr2 := deps.RequireTaskLaunch(ctx, store, deps.EntryDispatch, deps.Ref(task.Ref), desired, sel.GraphRevision)
	if perr2 != nil {
		return nil, perr2
	}
	depProv = pre.Provenance

	// 2c. Durable cross-process lease BEFORE first side effect (pkg/claim SQLite).
	// Generation-fenced; not a process-local map. Provider board CAS is FAC-147.
	own, oerr := d.ownershipClaimer()
	if oerr != nil {
		return nil, fmt.Errorf("dispatch lease store: %w", oerr)
	}
	claimRole := "launch"
	if lane.Role != "" {
		claimRole = lane.Role
	} else if lane.Name != "" {
		claimRole = lane.Name
	}
	tok, cerr := own.ClaimExclusive(ctx, pre.TaskID, deps.Ref(task.Ref), claimRole, pre.GraphRevision, pre.ProviderRevision, "")
	if cerr != nil {
		return nil, fmt.Errorf("dispatch lease claim: %w", cerr)
	}
	// Exactly-one durable compensation WHILE owner+generation still held.
	// Release only after durable compensate succeeds. On compensate failure
	// retain the lease (Recovering) — never release-then-compensate (B can
	// acquire and get stomped by stale A). Audit: h5d6pay5vamxvv277qtt5qmk.
	failOwned := func(reason string, primary error) error {
		owns, oerr := own.StillOwns(ctx, tok)
		if oerr != nil {
			return errors.Join(primary, oerr)
		}
		if !owns {
			// Lost generation: refuse unfenced shared lifecycle compensate.
			return fmt.Errorf("%w: refuse unfenced compensate (%s): %w", deps.ErrNotOwner, reason, primary)
		}
		if cErr := d.compensate(ctx, task.Ref, reason); cErr != nil {
			// Retain lease — Recovering. Do not open an acquire window for B.
			return errors.Join(primary, fmt.Errorf("durable compensate retained lease (Recovering): %w", cErr))
		}
		if rErr := own.ReleaseIfOwner(ctx, tok, reason); rErr != nil && !errors.Is(rErr, deps.ErrNotOwner) {
			return errors.Join(primary, rErr)
		}
		return primary
	}
	if !opts.NoLaunch {
		bound, berr := router.RebindDecision(opts.Decision, task.Ref, tok.Generation)
		if berr != nil {
			return nil, failOwned("launch_policy_rejected", berr)
		}
		opts.Decision = bound
		if _, berr = validateWorkerLaunchRequest(opts); berr != nil {
			return nil, failOwned("launch_policy_rejected", berr)
		}
	}

	// 3. Create worktree from immutable origin/<defaultBranch> (FAC-121).
	if d.Worktree == nil {
		return nil, failOwned("worktree_service_missing", fmt.Errorf("dispatch worktree service is required"))
	}
	wtInfo, err := d.Worktree.CreateTaskWorktreeFrom(ctx, task.Ref, defaultBranch)
	if err != nil {
		return nil, failOwned("worktree_create_failed", fmt.Errorf("failed to create worktree: %w", err))
	}
	if err := worktree.RejectSharedRoot(d.Worktree.RepoRoot(), wtInfo.Path); err != nil {
		return nil, failOwned("shared_root_denied", err)
	}
	branch := wtInfo.Branch
	if branch == "" {
		return nil, failOwned("empty_worktree_branch",
			fmt.Errorf("worktree created without a Git branch; refusing fictional packet branch"))
	}

	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepWorktree,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		BaseSHA:   wtInfo.BaseSHA,
		AnchorRef: wtInfo.AnchorRef,
	}); err != nil {
		return nil, failOwned("record_worktree_failed", err)
	}

	// 4. Flip board status only while we still hold the lease generation.
	if owns, _ := own.StillOwns(ctx, tok); !owns {
		return nil, failOwned("lease_lost_before_status", fmt.Errorf("lease owner+generation lost before board status"))
	}
	if err := d.updateStatusBound(ctx, task.ID, "in-progress"); err != nil {
		return nil, failOwned("board_status_failed", formatBoardErr("failed to update ticket status", err))
	}
	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepBoardProgress,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		BaseSHA:   wtInfo.BaseSHA,
		AnchorRef: wtInfo.AnchorRef,
	}); err != nil {
		return nil, failOwned("record_board_progress_failed", err)
	}

	// 5. Comment with actual Git branch + base (not a fictional name)
	comment := fmt.Sprintf("Dispatched to worktree %s on branch %s (base %s anchor %s lease g%d owner %s)",
		wtInfo.Path, branch, wtInfo.BaseSHA, wtInfo.AnchorRef, tok.Generation, tok.OwnerID)
	if err := d.addCommentBound(ctx, task.ID, comment); err != nil {
		return nil, failOwned("board_comment_failed", formatBoardErr("failed to add comment", err))
	}
	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepBoardComment,
		Worktree:  wtInfo.Path,
		Branch:    branch,
	}); err != nil {
		return nil, failOwned("record_board_comment_failed", err)
	}

	// 6. Preflight in worktree
	if err := preflight.CheckWorktreeBoundary(wtInfo.Path); err != nil {
		return nil, failOwned("preflight_failed", fmt.Errorf("preflight failed in worktree: %w", err))
	}

	// 7. Write TASK-PACKET.md — packet branch MUST equal Git branch
	if d.Config.Verification.TestCommand == "" {
		return nil, failOwned("verification_test_command_missing",
			fmt.Errorf("verification.test_command is required in .herd/herd.yaml (FAC-134 fail-closed; no hardcoded go test fallback)"))
	}

	rolePath := lane.Prompt
	if rolePath == "" {
		rolePath = ".herd/prompts/worker.md"
	}

	packet := buildTaskPacket(task, branch, rolePath, d.Config.TaskProvider.Type, lane, d.Config.Verification)
	if depProv != nil {
		packet = packet + "\n" + deps.PacketSection(depProv)
	}
	packetPath := filepath.Join(wtInfo.Path, "TASK-PACKET.md")
	if err := os.WriteFile(packetPath, []byte(packet), 0644); err != nil {
		return nil, failOwned("task_packet_write_failed", fmt.Errorf("failed to write task packet: %w", err))
	}

	result := &DispatchResult{
		TicketRef:       task.Ref,
		TicketTitle:     task.Title,
		Worktree:        wtInfo.Path,
		Branch:          branch,
		BaseSHA:         wtInfo.BaseSHA,
		AnchorRef:       wtInfo.AnchorRef,
		TaskPacket:      packetPath,
		Lane:            lane.Name,
		LeaseGeneration: tok.Generation,
	}

	// 8. Still own the lease generation; re-validate bound to pre-claim GraphRev.
	// Reuse fence snapshot (no N-call project re-fanout). Incident-edge freshness
	// is O(1) ListRelations on the target inside ValidateLaunch.
	owns, oerr := own.StillOwns(ctx, tok)
	if oerr != nil || !owns {
		return result, failOwned("lease_lost", fmt.Errorf("dispatch lease lost during side effects: owns=%v err=%v", owns, oerr))
	}
	if _, postErr := deps.RequireTaskLaunch(ctx, store, deps.EntryDispatch, deps.Ref(task.Ref), desired, tok.GraphRev); postErr != nil {
		return result, failOwned("post_dispatch_graph_drift", postErr)
	}

	// 9. Optionally launch agent with explicit cwd + proven prompt consumption.
	// launch performs local cleanup only; shared lifecycle compensate is exactly
	// once via failOwned while the generation lease is still held.
	h := d.launcher()
	if !opts.NoLaunch && h.Available() {
		if err := d.launch(ctx, opts, task, lane, wtInfo, branch, packet, result, tok); err != nil {
			reason := "agent_launch_failed"
			var lf *launchFailure
			if errors.As(err, &lf) && lf.Reason != "" {
				reason = lf.Reason
			}
			return result, failOwned(reason, err)
		}
	}

	// Success path: keep the generation lease for the live run (FAC-120/147).
	// Release is on completion/recovery paths, not here.
	return result, nil
}

// launchFailure is intent-only: launch never calls Compensator.Compensate.
// Outer Dispatch.failOwned performs the single owner-fenced durable compensate.
type launchFailure struct {
	Reason string
	Err    error
}

func (e *launchFailure) Error() string {
	if e == nil || e.Err == nil {
		return "launch failure"
	}
	return e.Err.Error()
}
func (e *launchFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// closeTabLocal is safely-scoped local cleanup only (no shared lifecycle
// Compensator.Compensate). TabClose errors are joined into primary — never silent.
func closeTabLocal(h HerdrLauncher, tabID, reason string, primary error) error {
	if tabID == "" || h == nil {
		return primary
	}
	if closeErr := h.TabClose(tabID); closeErr != nil {
		return errors.Join(primary, fmt.Errorf("tab close %q during %s: %w", tabID, reason, closeErr))
	}
	return primary
}

func (d *Dispatcher) launch(
	ctx context.Context,
	opts DispatchOptions,
	task *provider.Task,
	lane *config.LaneDef,
	wtInfo *worktree.WorktreeInfo,
	branch, packet string,
	result *DispatchResult,
	tok *deps.OwnershipToken,
) error {
	// Launch does not own shared lifecycle compensation. requireCompensator is
	// enforced by Dispatch before side effects; record() still needs the hook.
	if err := d.requireCompensator(); err != nil {
		return &launchFailure{Reason: "compensator_missing", Err: err}
	}

	h := d.launcher()
	tabLabel := fmt.Sprintf("task-%s", strings.ToLower(task.Ref))
	if len(tabLabel) > 32 {
		tabLabel = tabLabel[:32]
	}

	// Shared-root denial before any write-capable agent starts (production guard).
	if err := worktree.RejectSharedRoot(d.Worktree.RepoRoot(), wtInfo.Path); err != nil {
		return &launchFailure{Reason: "shared_root_denied", Err: err}
	}

	// FAC-175: implementation workers have one explicit routed tier. Validate
	// before workspace/tab/process/prompt side effects; no lane default or
	// provider fallback may substitute a coordinator model.
	request, err := workerRequest(opts, task.Ref)
	if err != nil {
		return &launchFailure{Reason: "launch_policy_rejected", Err: err}
	}
	if err := launch.Validate(request, nil); err != nil {
		return &launchFailure{Reason: "launch_policy_rejected", Err: err}
	}
	if d.Production {
		request.Repository, err = AuthenticatedRepositoryIdentity(d.Worktree.RepoRoot())
		if err != nil {
			return &launchFailure{Reason: "repository_identity_missing", Err: err}
		}
	} else {
		request.Repository, err = d.repositoryIdentity()
		if err != nil {
			return &launchFailure{Reason: "repository_identity_missing", Err: err}
		}
	}
	request.Lane = lane.Name
	if request.Repository == "" || request.Lane == "" {
		return &launchFailure{Reason: "launch_identity_missing", Err: fmt.Errorf("repository and lane identity are required")}
	}
	model := request.Decision.Model
	result.Model = model

	// Explicit workspace — never hardcoded "wF".
	ws, err := h.RequireWorkspace(d.Worktree.RepoRoot())
	if err != nil {
		return &launchFailure{
			Reason: "workspace_unknown",
			Err:    fmt.Errorf("worktree ready but herdr workspace unresolved: %w", err),
		}
	}

	// Tab with exact task worktree as process cwd.
	tab, err := h.TabCreateForTask(ws, tabLabel, wtInfo.Path, true)
	if err != nil {
		return &launchFailure{
			Reason: "tab_create_failed",
			Err:    fmt.Errorf("worktree ready but failed to launch agent: %w", err),
		}
	}
	result.TabID = tab.ID
	result.AgentName = tabLabel
	if err := herdr.PrepareToolChildLifecycle(tab.ID, tab.Pane.ID, request, tabLabel); err != nil {
		return &launchFailure{Reason: "tool_child_lifecycle_failed", Err: closeTabLocal(h, tab.ID, "tool_child_lifecycle_failed", err)}
	}
	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepTab,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tab.ID,
		PaneID:    tab.Pane.ID,
		AgentName: tabLabel,
	}); err != nil {
		return &launchFailure{
			Reason: "record_tab_failed",
			Err:    closeTabLocal(h, tab.ID, "record_tab_failed", err),
		}
	}

	if err := h.AgentStart(request, tabLabel, request.Decision.Provider, tab.Pane.ID); err != nil {
		// Local orphan-tab cleanup only — outer failOwned owns durable compensate.
		return &launchFailure{
			Reason: "agent_start_failed",
			Err: closeTabLocal(h, tab.ID, "agent_start_failed",
				fmt.Errorf("worktree ready but agent start failed: %w", err)),
		}
	}
	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepAgentStart,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tab.ID,
		PaneID:    tab.Pane.ID,
		AgentName: tabLabel,
	}); err != nil {
		return &launchFailure{
			Reason: "record_agent_start_failed",
			Err:    closeTabLocal(h, tab.ID, "record_agent_start_failed", err),
		}
	}

	// Always prove consumption — no production SkipPromptVerify bypass.
	timeout := opts.PromptVerifyTimeout
	if timeout == 0 {
		timeout = d.PromptVerifyTimeout
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	repository := request.Repository
	identity := control.LaneIdentity{Repository: repository, TaskRef: task.Ref, Lane: lane.Name, LeaseGeneration: result.LeaseGeneration, CandidateSHA: wtInfo.BaseSHA}
	if identity.Repository == "" || identity.CandidateSHA == "" {
		identity.Repository = repository
		identity.CandidateSHA = opts.Decision.CandidateSHA
	}
	wakeTarget := control.WakeTarget{Target: tabLabel, Workspace: ws, TabID: tab.ID, PaneID: tab.Pane.ID, AgentName: tabLabel, Provider: request.Decision.Provider, LeaseGeneration: result.LeaseGeneration}
	check := func(checkCtx context.Context, o control.Order) error {
		if o.LaneIdentity != identity {
			return control.ErrStaleIdentity
		}
		if d.Ownership == nil {
			return fmt.Errorf("control: live ownership authority is required")
		}
		owned, err := d.Ownership.StillOwns(checkCtx, tok)
		if err != nil {
			return err
		}
		if !owned {
			return control.ErrStaleIdentity
		}
		return nil
	}
	if verifier, ok := h.(interface {
		ReadControlTarget(control.WakeTarget) (control.WakeTarget, error)
	}); ok {
		actual, err := verifier.ReadControlTarget(wakeTarget)
		if err != nil && d.Production {
			return &launchFailure{Reason: "control_target_drift", Err: closeTabLocal(h, tab.ID, "control_target_drift", err)}
		}
		if err == nil {
			wakeTarget = actual
		}
	} else if d.Production {
		return &launchFailure{Reason: "control_target_unverifiable", Err: closeTabLocal(h, tab.ID, "control_target_unverifiable", fmt.Errorf("Herdr launcher cannot verify exact target"))}
	}
	var evidence control.Evidence
	if d.Production {
		orders, factoryErr := d.ControlFactory(ctx, ControlScope{Identity: identity, Wake: wakeTarget, Check: check})
		if factoryErr != nil {
			return &launchFailure{Reason: "control_factory_failed", Err: closeTabLocal(h, tab.ID, "control_factory_failed", factoryErr)}
		}
		if orders == nil {
			return &launchFailure{Reason: "control_factory_failed", Err: fmt.Errorf("nil control orders")}
		}
		var orderErr error
		evidence, orderErr = orders.Repair(ctx, packet)
		if orderErr != nil {
			return &launchFailure{Reason: "control_order_failed", Err: closeTabLocal(h, tab.ID, "control_order_failed", orderErr)}
		}
	} else if d.Orders != nil {
		if e, orderErr := d.Orders.Repair(ctx, packet); orderErr != nil {
			return &launchFailure{Reason: "control_order_failed", Err: closeTabLocal(h, tab.ID, "control_order_failed", orderErr)}
		} else {
			evidence = e
		}
	}
	var receipt *herdr.PromptReceipt
	var receiptErr error
	if evidence.MessageID != "" {
		// Delivery already performed the one Herdr wake and returned its actual
		// receipt. Never nudge the same order again from this outer layer.
		if !evidence.Wake.Consumed || !evidence.Wake.Verified {
			return &launchFailure{Reason: "prompt_receipt_invalid", Err: closeTabLocal(h, tab.ID, "prompt_receipt_invalid", fmt.Errorf("durable control wake did not prove consumption"))}
		}
		receipt = &herdr.PromptReceipt{Target: evidence.Wake.Target, Consumed: evidence.Wake.Consumed, Verified: evidence.Wake.Verified, BaselineStatus: evidence.Wake.Baseline, FinalStatus: evidence.Wake.Final, SequenceToken: evidence.Wake.SequenceToken}
	} else {
		// Explicit non-production test mode has no durable control port; it still
		// sends only a fixed wake reference, never the task packet.
		receipt, receiptErr = h.DeliverAndProve(tabLabel, fmt.Sprintf("consume durable control envelope task %s", task.Ref), timeout)
	}
	result.Receipt = receipt
	if receiptErr != nil {
		return &launchFailure{
			Reason: "prompt_delivery_failed",
			Err: closeTabLocal(h, tab.ID, "prompt_delivery_failed",
				fmt.Errorf("worktree ready but prompt consumption not proven: %w", err)),
		}
	}
	if receipt == nil || !receipt.Consumed || !receipt.Verified {
		return &launchFailure{
			Reason: "prompt_receipt_invalid",
			Err: closeTabLocal(h, tab.ID, "prompt_receipt_invalid",
				fmt.Errorf("worktree ready but prompt receipt did not prove consumption")),
		}
	}
	if !herdr.ConsumptionProven(receipt.BaselineStatus, receipt.FinalStatus) {
		return &launchFailure{
			Reason: "prompt_sequence_invalid",
			Err: closeTabLocal(h, tab.ID, "prompt_sequence_invalid",
				fmt.Errorf("prompt receipt sequence %q is not a valid consumption proof", receipt.SequenceToken)),
		}
	}

	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepPrompt,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tab.ID,
		AgentName: tabLabel,
		Receipt:   receipt.SequenceToken,
		MessageID: evidence.MessageID,
		Sequence:  evidence.Sequence,
	}); err != nil {
		return &launchFailure{
			Reason: "record_prompt_failed",
			Err:    closeTabLocal(h, tab.ID, "record_prompt_failed", err),
		}
	}
	result.Launched = true
	return nil
}

func workerRequest(opts DispatchOptions, taskRef string) (launch.Request, error) {
	if opts.Decision == nil {
		return launch.Request{TaskRef: taskRef}, fmt.Errorf("compiled LaunchDecision is required; defaults are forbidden")
	}
	d := opts.Decision
	if d.Provider == "" || d.Model == "" || d.Effort == "" || d.Role == "" || d.Shape == "" || len(d.Argv) == 0 {
		return launch.Request{Decision: d, TaskRef: taskRef, LeaseGeneration: d.LeaseGeneration, Scope: d.Scope}, fmt.Errorf("compiled LaunchDecision fields are required; defaults are forbidden")
	}
	return launch.Request{Decision: d, TaskRef: taskRef, LeaseGeneration: d.LeaseGeneration, Scope: d.Scope}, nil
}

func validateWorkerLaunchRequest(opts DispatchOptions) (launch.Request, error) {
	contextRef := ""
	if opts.Decision != nil {
		contextRef = opts.Decision.TaskRef
	}
	req, err := workerRequest(opts, contextRef)
	if err != nil {
		return req, launch.Validate(req, nil)
	}
	if err := launch.Validate(req, nil); err != nil {
		return req, err
	}
	return req, nil
}

func slugForTask(ref, title string) string {
	s := strings.ToLower(title)
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		if r == ' ' || r == '_' || r == '/' {
			return '-'
		}
		return -1
	}, s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")

	refSlug := strings.ToLower(ref)
	return fmt.Sprintf("%s-%s", refSlug, s)
}

func extractCommandFromTitle(title string) string {
	title = strings.TrimSpace(title)
	if idx := strings.Index(title, ":"); idx != -1 {
		title = title[:idx]
	}
	title = strings.TrimSpace(title)
	return strings.ToLower(title)
}

func extractIntentFromTitle(title string) string {
	title = strings.TrimSpace(title)
	if idx := strings.Index(title, ":"); idx != -1 {
		title = strings.TrimSpace(title[idx+1:])
	}
	// Remove parenthetical references like "(FAC-59)"
	title = regexp.MustCompile(`\s*\([A-Z]+-\d+\)`).ReplaceAllString(title, "")
	return strings.TrimSpace(strings.ToLower(title))
}

// buildTaskPacket builds a TIGHT, reference-based packet (FAC-115). It does
// NOT dump the card's (often 150-line) spec inline — that burned the agent's
// context to 80% before it wrote a line and was a direct cause of the
// whiff-and-stall pattern. The agent reads the full spec itself via
// `kaneo task get`, keeping its whole context for the actual build. The
// packet is only the completion contract: where to work, what "done" means,
// and how to signal it.
//
// FAC-121: branch in the packet is the actual Git branch; cwd is technical
// (Herdr --cwd), not merely a prompt instruction.
//
// FAC-134: verification commands come from the repo's own config
// (verification.preflight_command / verification.test_command in
// .herd/herd.yaml) — never a hardcoded `go build`/`go test` literal — so the
// same packet works for Go, Node, Python, or docs-only repositories.
// Callers must ensure verification.TestCommand is non-empty (Dispatch fails
// closed before calling this).
//
// The packet never embeds the host's absolute worktree path (portability;
// FAC-134 review finding). Herdr already cwd-enforces the worker into the
// worktree (TabCreateForTask), so the packet refers to it as "." / "the
// current directory" — cwd is preserved separately from packet text, not
// duplicated into it. Likewise, the "read the full spec" step names a
// concrete CLI only for the task provider actually configured (kaneo); any
// other provider gets a provider-neutral reference instead of an assumed,
// uncredentialed `kaneo` CLI call.
func buildTaskPacket(task *provider.Task, branch, rolePath, taskProviderType string, lane *config.LaneDef, verification config.Verification) string {
	var b strings.Builder

	verifySummary := verification.TestCommand
	verifyFlags := fmt.Sprintf("--test %q", verification.TestCommand)
	if verification.PreflightCommand != "" {
		verifySummary = verification.PreflightCommand + " && " + verification.TestCommand
		verifyFlags = fmt.Sprintf("--build %q %s", verification.PreflightCommand, verifyFlags)
	}

	fmt.Fprintf(&b, "BUILD %s — EXECUTE. No menus, no questions. Do not stop until "+
		"`%s` passes AND you have committed.\n\n", task.Ref, verifySummary)

	fmt.Fprintf(&b, "Worktree: current directory (Herdr cwd-enforced), branch %s. Work ONLY here — never edit files outside it.\n\n", branch)

	fmt.Fprintf(&b, "Read the full spec yourself (do not wait for it inline) via the configured task provider (%s):\n", taskProviderType)
	if taskProviderType == "kaneo" {
		fmt.Fprintf(&b, "  kaneo task get %s --full\n\n", task.Ref)
	} else {
		fmt.Fprintf(&b, "  ref: %s\n\n", task.Ref)
	}

	b.WriteString("Completion contract (self-gate, FAC-116):\n")
	b.WriteString("  1. You are already in the task worktree (Herdr cwd-enforced).\n")
	b.WriteString("  2. Implement per the spec you just read (real code + table tests).\n")
	fmt.Fprintf(&b, "  3. `%s` — ALL green.\n", verifySummary)
	// Flags before the positional worktree arg: Go's flag package stops
	// parsing at the first non-flag token, so flags placed after "." would
	// be silently ignored.
	fmt.Fprintf(&b, "  4. Verify yourself: herd verify %s . (must PASS: real commits + build + tests).\n", verifyFlags)
	fmt.Fprintf(&b, "  5. git add -A && git commit -m \"<msg containing %s>\" (no AI-attribution trailers).\n", task.Ref)
	fmt.Fprintf(&b, "  6. Final message: `BUILD COMPLETE %s` + `git rev-parse HEAD`.\n\n", task.Ref)

	b.WriteString("Do NOT push, PR, or merge — the coordinator harvests your branch. Do NOT touch the root checkout.\n")
	if lane != nil && rolePath != "" {
		fmt.Fprintf(&b, "Role contract: %s\n", rolePath)
	}
	return b.String()
}
