package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/confinement"
	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/envplan"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/recovery"
	"github.com/Kampe/Herdforge/pkg/residual"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/runstate"
	"github.com/Kampe/Herdforge/pkg/scopefence"
	"github.com/Kampe/Herdforge/pkg/security"
	"github.com/Kampe/Herdforge/pkg/standing"
	"github.com/Kampe/Herdforge/pkg/toolchild"
	"github.com/Kampe/Herdforge/pkg/toolprobe"
	"github.com/Kampe/Herdforge/pkg/worktree"
	"github.com/Kampe/Herdforge/pkg/worktreebootstrap"
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
	// TabCreateWithEnv creates a pane with explicit KEY=VALUE env (FAC-133 scrubbed env).
	// Empty env must be rejected by sandboxed launch paths.
	TabCreateWithEnv(workspaceID, label, cwd string, env []string, noFocus bool) (*herdr.TabInfo, error)
	// TabCreateForTask creates a task tab; optional env is used by FAC-190 confinement PATH/ZDOTDIR.
	TabCreateForTask(workspaceID, label, cwd string, noFocus bool, env ...string) (*herdr.TabInfo, error)
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
func (LiveHerdr) TabCreateWithEnv(workspaceID, label, cwd string, env []string, noFocus bool) (*herdr.TabInfo, error) {
	if len(env) == 0 {
		return nil, fmt.Errorf("sandboxed tab create: scrubbed env required (refuse ambient inheritance)")
	}
	return herdr.TabCreateForTaskEnv(workspaceID, label, cwd, env, noFocus)
}
func (LiveHerdr) TabCreateForTask(workspaceID, label, cwd string, noFocus bool, env ...string) (*herdr.TabInfo, error) {
	return herdr.TabCreateForTask(workspaceID, label, cwd, noFocus, env...)
}

// dispatchTabOpener routes boundary Open through the injectable HerdrLauncher
// so crash-point tests still observe TabCreateForTask.
type dispatchTabOpener struct{ h HerdrLauncher }

func (o dispatchTabOpener) OpenTab(workspace, label, cwd string, noFocus bool, env ...string) (string, string, error) {
	tab, err := o.h.TabCreateForTask(workspace, label, cwd, noFocus, env...)
	if err != nil {
		return "", "", err
	}
	if tab == nil {
		return "", "", fmt.Errorf("herdr tab create returned nil")
	}
	return tab.ID, tab.Pane.ID, nil
}

// resolveToolProbe returns the artifact tool-probe receipt required by the
// launch boundary. Production requires an explicit or live PASS; non-production
// synthesizes a PASS bound to the decision so hermetic unit tests stay offline.
func (d *Dispatcher) resolveToolProbe(opts DispatchOptions, decision *router.LaunchDecision) (*toolprobe.Receipt, error) {
	if opts.Probe != nil {
		return opts.Probe, nil
	}
	if decision == nil {
		return nil, fmt.Errorf("tool-probe requires LaunchDecision")
	}
	id, err := toolprobe.IdentityFromDecision(decision)
	if err != nil {
		return nil, err
	}
	if d != nil && d.Production {
		cache := toolprobe.NewFileCache(toolprobe.DefaultCachePath)
		r, err := toolprobe.Ensure(context.Background(), id, cache, &toolprobe.ExecRunner{}, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		if !r.Passes(time.Now().UTC()) {
			return &r, fmt.Errorf("tool-probe status %s blocks launch: %s", r.Status, r.Reason)
		}
		return &r, nil
	}
	r, err := toolprobe.NewReceipt(id, toolprobe.StatusPASS, "test-synthetic", "sha256:test", time.Now().UTC(), toolprobe.DefaultTTL)
	if err != nil {
		return nil, err
	}
	return &r, nil
}
func (LiveHerdr) AgentStart(req launch.Request, name, kind, paneID string) error {
	return herdr.AgentStartWithDecision(name, kind, paneID, req)
}
func (LiveHerdr) DeliverAndProve(target, text string, timeout time.Duration) (*herdr.PromptReceipt, error) {
	// Prefer session-exact delivery when live agent_session is known.
	// Does not invent a prompt-delivery subcommand — uses AgentPromptExact.
	if a, err := herdr.LookupAgent(target); err == nil && a != nil {
		sid := strings.TrimSpace(a.Session.Value)
		if sid != "" && herdr.RealModelSessionID(sid) {
			b := herdr.BindingFromSpawn(a.Name, a.TabID, a.PaneID, sid, a.Kind)
			return herdr.DeliverAndProveExact(b, text, timeout)
		}
	}
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
		// Tab, pane, name, workspace and kind are the exact identity herdr
		// guarantees for every agent kind. A session id is provenance that
		// only some surfaces report — grok never does — so requiring it here
		// rejected every non-claude launch as "no longer current" moments
		// after it started healthy.
		if a.TabID == target.TabID && a.PaneID == target.PaneID && a.Name == target.AgentName && a.Workspace == target.Workspace && a.Kind == target.Provider {
			target.SessionID = a.Session.Value
			return target, nil
		}
	}
	return control.WakeTarget{}, fmt.Errorf("launched Herdr tab/pane/agent/session is no longer current")
}

type DispatchOptions struct {
	TicketRef string
	Decision  *router.LaunchDecision
	// Probe is a current artifact-backed tool-probe PASS for Decision's surface
	// (FAC-139). Production write-capable launches fail closed when absent or
	// stale. Non-production tests may omit it; launch synthesizes a PASS bound
	// to Decision only in that mode.
	Probe    *toolprobe.Receipt
	NoLaunch bool
	// EnvironmentPlanID is the durable operator-approved environment plan for
	// this dispatch. It is required for production external actions.
	EnvironmentPlanID string
	LaneName          string
	// LeaseID/LeaseGeneration bind the launch receipt to the durable claim
	// fencing this dispatch (FAC-145) AND bind trusted control envelopes for the
	// launch (FAC-133). Both tickets added this field independently; it is one
	// value serving both, not two concepts.
	//
	// Zero means "unleased" and skips generation fencing downstream — but a
	// sandboxed write-capable launch requires >0 and fails closed without it.
	LeaseID         string
	LeaseGeneration int64
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
	// ControlBound is true when a MAC-signed launch control envelope was posted (FAC-133).
	ControlBound bool
	// SandboxGranted is true when least-privilege LaunchPolicy authorized the agent.
	SandboxGranted bool
	// SecurityEvents is the count of injection/denial events recorded at launch.
	SecurityEvents int
}

// ReplyTarget carries the coordinator identity a packet embeds so agents
// know where to report completion and BLOCKED (FAC-222). Before this, no
// packet carried a reply address — the coordinator discovered every finished
// branch by polling git state, late and lossy.
type ReplyTarget struct {
	Name             string // the coordinator's stable name (never empty)
	ReviewSupervisor string // standing supervisor that owns review handoffs
	LeaseGeneration  int64  // the lease generation the agent must cite in callbacks
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
	// Claims, when set, routes board status/comment writes through FAC-147
	// Begin/Complete + FencedCAS instead of bare TaskProvider.
	Claims *provider.ClaimStack
	// ScopeFence is an optional injected admission boundary. Production wiring
	// supplies a durable scopefence.Fence; nil preserves packet-only callers.
	ScopeFence    ScopeAdmission
	scopeFenceErr error
	scopeCloser   io.Closer
	closeOnce     sync.Once
	closeErr      error

	// Confinement is the FAC-190 write-boundary gate for production launches.
	// When nil under Production, launch constructs ProductionEnforcer() and
	// fails closed without a MAC issuer + OS write-denial proof.
	Confinement *confinement.Enforcer
	// FAC-133 trusted control plane (MAC envelopes) — distinct from Orders/ControlFactory.
	// Required for write-capable launch when least-privilege sandbox is enforced.
	Control *ControlPlane
	// ControlSecret is the shared MAC secret (fail-closed when empty on sandboxed launch).
	ControlSecret string
	// RepoIdentity / RepoAllowlist gate which repository the agent may touch.
	RepoIdentity  string
	RepoAllowlist []string
	// PackageAllowlist exclusive FS roots under the worktree (structured provenance).
	PackageAllowlist []string
	// SandboxEvents receives denial/injection security events (optional sink).
	SandboxEvents security.EventSink
	// SecurityEventLog is the durable JSONL path for security events.
	SecurityEventLog string
	// ClaimLookup proves LeaseGeneration against live FAC-147 claim records.
	ClaimLookup security.LiveClaimLookup

	// CoordinatorName is the reply target embedded in every TASK-PACKET.md
	// (FAC-222). When empty, dispatch resolves it from the durable coordinator
	// registration (.herd/coordinator.json) or falls back to the well-known
	// name "coordinator" (matching mail.CoordinatorInbox).
	CoordinatorName string

	// RunStates is the revision-bound resume authority for dispatch. When set,
	// Dispatch checkpoints the first exact task observation, then always resumes
	// it against live provider and graph evidence before any mutation. A terminal,
	// stale, or ambiguous saved state therefore cannot enter redispatch.
	RunStates            *runstate.Store
	RunStateGraph        runstate.GraphAuthority
	RunStateGraphForTask runstate.GraphAuthorityForTask
	Recovery             *recovery.Store
	RecoveryActor        string
	// EnvironmentPlans is the FAC-241 durable capability authority. It is
	// consulted before scope, worktree, board, harness, or credential effects.
	EnvironmentPlans *envplan.Store
	// Bootstrap executes the repository-declared worktree bootstrap only after
	// dispatch owns the task and has admitted its worktree. Nil uses the
	// production-safe executor; tests may inject an attributable failure seam.
	Bootstrap WorktreeBootstrapper

	// health projects BLOCKED(provider_timeout) for board calls (FAC-150).
	health dispatchHealth
}

// WorktreeBootstrapper is deliberately narrower than the dispatcher: the
// bootstrap contract receives only the admitted worktree and validated config.
// It cannot create a worktree, claim a task, or launch an agent.
type WorktreeBootstrapper interface {
	Execute(context.Context, string, config.WorktreeBootstrap) (*worktreebootstrap.Result, error)
}

func (d *Dispatcher) bootstrapWorktree(ctx context.Context, worktreePath string) error {
	if d.Config == nil || !d.Config.WorktreeBootstrap.Enabled() {
		return nil
	}
	runner := d.Bootstrap
	if runner == nil {
		runner = worktreebootstrap.Executor{}
	}
	if _, err := runner.Execute(ctx, worktreePath, d.Config.WorktreeBootstrap); err != nil {
		return fmt.Errorf("worktree bootstrap failed: %w\n  recovery: inspect .herd/bootstrap/receipt.json and repair the declared worktree_bootstrap command", err)
	}
	return nil
}

// coordinatorName returns the reply target name for packets. An explicitly
// set CoordinatorName wins; otherwise the well-known default is used. The
// caller (cmd/herd) is expected to populate CoordinatorName from the durable
// coordinator registration at wiring time.
func (d *Dispatcher) coordinatorName() string {
	if name := strings.TrimSpace(d.CoordinatorName); name != "" {
		return name
	}
	return "coordinator"
}

func (d *Dispatcher) reviewSupervisorName() string {
	laneName := "review-supervisor"
	if d.Config == nil {
		return standing.AgentName(laneName)
	}
	for _, role := range []string{"review-supervisor", "review_harvest_supervisor", "harvest-supervisor", "reviewer", "harvest"} {
		for _, lane := range d.Config.Lanes {
			if strings.ToLower(strings.TrimSpace(lane.Role)) == role && strings.TrimSpace(lane.Name) != "" {
				laneName = strings.TrimSpace(lane.Name)
				goto resolved
			}
		}
	}

resolved:
	// Builder packets contain a delivery address, not a configured role
	// reference. Standing lanes are repository-qualified in Herdr, so use the
	// same live identity here as direct review handoffs do. Test/non-production
	// dispatchers retain the legacy name only when identity resolution fails.
	if repository, err := d.repositoryIdentity(); err == nil {
		return standing.AgentNameForRepository(laneName, repository)
	}
	return standing.AgentName(laneName)
}

type ScopeAdmission interface {
	Acquire(context.Context, scopefence.AcquireRequest) (scopefence.Decision, error)
	Release(context.Context, scopefence.ReleaseRequest) error
}

// ScopePublisher publishes the graph snapshot and task scope that the fence
// resolves against. Dispatch calls this after the deps gate so the coordinator
// does not need to run `herd scope publish` manually before every dispatch.
// When ScopeFence does not implement ScopePublisher, dispatch falls back to
// requiring a pre-published scope (the original three-command dance).
type ScopePublisher interface {
	Publish(context.Context, PublishRequest) error
}

// PublishRequest carries the graph and scope data dispatch publishes.
type PublishRequest struct {
	Repository string
	Task       string
	Revision   string
	Files      int
	Scope      scopefence.Scope
}

type durableScopeAdmission struct{ fence scopefence.ResolvingFence }

func (a durableScopeAdmission) Acquire(ctx context.Context, req scopefence.AcquireRequest) (scopefence.Decision, error) {
	req.Scope = scopefence.Scope{}
	return a.fence.Acquire(ctx, req)
}

func (a durableScopeAdmission) Release(ctx context.Context, req scopefence.ReleaseRequest) error {
	return a.fence.Release(ctx, req)
}

// Publish writes the graph snapshot and task scope to the durable store and
// rebinds the graph authority to the published revision. This lets dispatch
// proceed without a separate `herd scope publish` step: the coordinator's
// provenance (already validated by the deps gate) is the scope authority.
func (a durableScopeAdmission) Publish(ctx context.Context, req PublishRequest) error {
	store, ok := a.fence.Fence.Store.(*scopefence.SQLiteStore)
	if !ok {
		return errors.New("dispatch: scope publish requires a durable SQLite store")
	}
	graph := scopefence.Graph{
		Revision: req.Revision,
		Files:    req.Files,
		Nodes:    req.Files,
		Edges:    req.Files,
		Flows:    1,
		Complete: true,
	}
	if err := store.PutGraphSnapshot(ctx, req.Repository, graph); err != nil {
		return fmt.Errorf("dispatch: publish graph snapshot: %w", err)
	}
	if err := store.PutScopeDeclaration(ctx, req.Repository, req.Task, req.Revision, req.Scope); err != nil {
		return fmt.Errorf("dispatch: publish scope declaration: %w", err)
	}
	if ga, ok := a.fence.Fence.Graph.(*scopefence.SQLiteGraphAuthority); ok {
		ga.UpdateExpected(req.Revision, req.Files)
	}
	return nil
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
	d.scopeFenceErr = errors.New("dispatch scope authority requires separately protected coordinator/root verifier (FAC-169 authority surface is not present)")
	return d
}

// NewProductionDispatcherWithAuthorities is the only production constructor
// that can install scope admission. Both verify-only authorities are injected
// by protected coordinator surfaces: graph/scope publication cannot authorize
// RootAdmittedMerge or FencedAbandonment. No key, signer, environment value,
// or database row is accepted as authority.
func NewProductionDispatcherWithAuthorities(cfg *config.Config, tp provider.TaskProvider, wm *worktree.WorktreeManager, graphVerifier scopefence.GraphScopeVerifier, releaseAuthority scopefence.ReleaseAuthority, expectedRevision string, expectedFiles int) *Dispatcher {
	d := NewDispatcher(cfg, tp, wm)
	d.Production = true
	if graphVerifier == nil || releaseAuthority == nil {
		d.scopeFenceErr = errors.New("dispatch scope authority requires separately protected coordinator/root verifier")
		return d
	}
	root := "."
	if wm != nil && wm.RepoRoot != "" {
		root = wm.RepoRoot
	}
	repository, err := AuthenticatedRepositoryIdentity(root)
	if err != nil {
		d.scopeFenceErr = fmt.Errorf("dispatch scope authority identity: %w", err)
		return d
	}
	store, err := scopefence.NewSQLiteStore(filepath.Join(root, ".herd", "scopefence.db"))
	if err != nil {
		d.scopeFenceErr = err
		return d
	}
	graph := scopefence.NewSQLiteGraphAuthority(store, repository, expectedRevision, expectedFiles)
	graph.Verifier = graphVerifier
	resolver := scopefence.NewSQLiteScopeAuthority(store)
	resolver.Verifier = graphVerifier
	d.ScopeFence = durableScopeAdmission{fence: scopefence.ResolvingFence{
		Fence: scopefence.Fence{Store: store, ReleaseAuthority: releaseAuthority, Graph: graph}, Authority: resolver,
	}}
	d.scopeCloser = store
	return d
}

// Close propagates the owned durable fence resource exactly once.
func (d *Dispatcher) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		if d.scopeCloser != nil {
			d.closeErr = d.scopeCloser.Close()
		}
	})
	return d.closeErr
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
	// FAC-155: the launch lease is keyed by board identity. Defaulting an
	// absent/blank provider to "memory" silently leased the ticket against a
	// board nobody configured, so an unbound dispatcher fails closed instead.
	if d.Config == nil {
		return nil, fmt.Errorf("dispatch: no repository config; task provider identity is unbound")
	}
	providerType := strings.ToLower(strings.TrimSpace(d.Config.TaskProvider.Type))
	if providerType == "" {
		return nil, fmt.Errorf("dispatch: task_provider.type is required for launch lease ownership")
	}
	project := d.Config.TaskProvider.ProjectID
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

func (d *Dispatcher) requireScopeAdmission() error {
	if !d.Production {
		return nil
	}
	if d.scopeFenceErr != nil {
		return d.scopeFenceErr
	}
	if d.ScopeFence == nil {
		return errors.New("dispatch scope fence is required in production")
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
	if err := d.requireScopeAdmission(); err != nil {
		return nil, err
	}
	// FAC-175: reject an under-specified worker launch before even reading or
	// mutating provider/worktree state. --no-launch is the explicit packet-only
	// mode and therefore has no launch boundary to validate.
	if !opts.NoLaunch {
		if _, err := validateWorkerLaunchRequest(opts); err != nil {
			return nil, err
		}
	}

	// Fail closed before any side effect when the task-provider project is
	// unknown (FAC-145): an agent spawned without it resolves project_id=NULL
	// and every downstream board read/mutation becomes nondeterministic.
	if strings.TrimSpace(d.Config.TaskProvider.ProjectID) == "" {
		return nil, fmt.Errorf("task_provider.project_id is required in .herd/herd.yaml (FAC-145 fail-closed; isolated agents cannot resolve a NULL project at spawn)")
	}

	// FAC-145/FAC-147 seam: every dispatch is backed by an ACQUIRED durable
	// claim lease — no generation is ever fabricated here. The CLI acquires
	// from the claim store and passes the real lease.
	if strings.TrimSpace(opts.LeaseID) == "" || opts.LeaseGeneration < 1 {
		return nil, fmt.Errorf("dispatch requires an acquired claim lease (FAC-145 fail-closed; the canonical fence source is the claim store)")
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
	_, err = d.admitRunState(ctx, task)
	if err != nil {
		return nil, err
	}
	if err := d.admitEnvironmentPlan(ctx, task, opts); err != nil {
		return nil, err
	}

	// FAC-133: provider title/description stay untrusted at the write-capable
	// launch boundary — never elevate card text into control authority.
	if err := RefuseProviderControlElevation(task); err != nil {
		return nil, err
	}
	// Observable injection indicators only; never elevates authority.
	_ = security.DetectProviderAuthorityClaims(security.ProviderTextBundle(task.Title, task.Description))

	// 2. Determine lane (still no side effects).
	laneName := opts.LaneName
	if laneName == "" {
		laneName = "worker"
	}
	lane, err := config.ResolveLane(d.Config, laneName)
	if err != nil {
		return nil, err
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
	// Memoize the live claimer. The control-order check later in this dispatch
	// reads d.Ownership directly and fails closed on nil; without this the
	// claimer exists only as a local here, so every production launch was
	// rejected with "control: live ownership authority is required" even though
	// a working authority had just been constructed.
	if d.Ownership == nil {
		d.Ownership = own
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
	var scopeOwned bool
	var scopeRelease scopefence.ReleaseRequest
	worktreeCreated := false
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
		if scopeOwned && !worktreeCreated {
			scopeRelease.Authority = scopefence.CompensatedNoCandidate
			// Production release authentication is performed by the injected
			// protected authority; no worker-mintable hash is a credential.
			scopeRelease.Proof = ""
			if sErr := d.ScopeFence.Release(ctx, scopeRelease); sErr != nil {
				return errors.Join(primary, fmt.Errorf("scopefence compensation retained ownership: %w", sErr))
			}
			scopeOwned = false
		}
		if rErr := own.ReleaseIfOwner(ctx, tok, reason); rErr != nil && !errors.Is(rErr, deps.ErrNotOwner) {
			return errors.Join(primary, rErr)
		}
		return primary
	}
	if d.ScopeFence != nil {
		repository, ierr := d.repositoryIdentity()
		if ierr != nil {
			return nil, failOwned("scopefence_identity_failed", ierr)
		}
		// Auto-publish: when ScopeFence implements ScopePublisher, dispatch
		// publishes the graph snapshot and task scope itself. The coordinator's
		// provenance (already validated by the deps gate) is the scope authority.
		// This eliminates the three-command dance (dispatch → scope publish →
		// dispatch) that made concurrent dispatch structurally impossible.
		if publisher, ok := d.ScopeFence.(ScopePublisher); ok {
			scope := deriveScopeFromProvenance(depProv)
			if err := scope.Validate(); err != nil {
				return nil, failOwned("scope_publish_failed", fmt.Errorf(
					"dispatch auto-publish: %w\n"+
						"  declare scope_packages or scope_files in the herd-deps-v1 fence, or run: herd scope publish %s --revision %s --packages <pkg>",
					err, task.Ref, pre.GraphRevision))
			}
			fileCount := countTrackedFiles(d.Worktree.RepoRoot())
			if fileCount <= 0 {
				fileCount = 1
			}
			if err := publisher.Publish(ctx, PublishRequest{
				Repository: repository,
				Task:       task.Ref,
				Revision:   pre.GraphRevision,
				Files:      fileCount,
				Scope:      scope,
			}); err != nil {
				return nil, failOwned("scope_publish_failed", err)
			}
		}
		admissionReq := scopefence.AcquireRequest{Ownership: scopefence.Ownership{
			Identity:      scopefence.Identity{Repository: repository, Branch: worktree.TaskBranch(task.Ref), Task: task.Ref},
			Generation:    tok.Generation,
			State:         scopefence.Active,
			GraphRevision: pre.GraphRevision,
		}, ExpectedGraphRevision: pre.GraphRevision}
		admission, aerr := d.ScopeFence.Acquire(ctx, admissionReq)
		if aerr != nil {
			// Name the exact revision to publish at. This error used to say only
			// "trusted task scope unavailable", and the revision is a deps-graph
			// hash that appears nowhere else, so there was no way to act on it.
			if strings.Contains(aerr.Error(), "trusted task scope unavailable") ||
				strings.Contains(aerr.Error(), "trusted graph snapshot unavailable") {
				aerr = fmt.Errorf("%w\n  publish it first: herd scope publish %s --revision %s --packages <pkg>",
					aerr, task.Ref, pre.GraphRevision)
			}
			return nil, failOwned("scopefence_error", aerr)
		}
		if !admission.Granted {
			// Always name the revision. A fence rejection is only actionable
			// if you know which deps-graph revision to publish against, and
			// that hash appears nowhere else in the output.
			return nil, failOwned("scopefence_rejected", fmt.Errorf(
				"dispatch scope fence rejected: %s (graph revision %s)\n"+
					"  republish scope at this revision: herd scope publish %s --revision %s --packages <pkg>",
				admission.Evidence.Reason, pre.GraphRevision, task.Ref, pre.GraphRevision))
		}
		if admission.Lease == nil {
			return nil, failOwned("scopefence_missing_lease", errors.New("scopefence granted without durable lease"))
		}
		scopeOwned = true
		scopeRelease = scopefence.ReleaseRequest{Ownership: *admission.Lease}
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
	worktreeCreated = wtInfo != nil
	if wtInfo == nil {
		return nil, failOwned("worktree_create_failed", errors.New("worktree service returned nil info"))
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
	// FAC-147: when Claims is wired, board writes use Begin/Complete + fence.
	// Tests without Claims keep the unfenced bound path.
	if d.Claims != nil {
		// Prefer lane.Role (forge-smith/worker/…), never lane.Name (scout/smith/…).
		// TaskOwnershipRole only accepts known implementation roles; lane names fail closed.
		if err := d.updateStatusFenced(ctx, task, claimRole, "in-progress"); err != nil {
			return nil, failOwned("board_status_failed", formatBoardErr("failed to update ticket status", err))
		}
	} else if err := d.updateStatusBound(ctx, task.ID, "in-progress"); err != nil {
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
	if d.Claims != nil {
		if err := d.addCommentFenced(ctx, task, claimRole, comment); err != nil {
			return nil, failOwned("board_comment_failed", formatBoardErr("failed to add comment", err))
		}
	} else if err := d.addCommentBound(ctx, task.ID, comment); err != nil {
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
	if err := preflight.CheckWorktreeBoundaryChanged(wtInfo.Path, d.Config.WorktreeBoundary.AllowedAbsolutePaths); err != nil {
		return nil, failOwned("preflight_failed", fmt.Errorf("preflight failed in worktree: %w", err))
	}
	// 6b. Execute the repository-declared bootstrap only after ownership,
	// scope admission, worktree creation, and worktree boundary admission.
	// A failed or stale bootstrap is an attributable recovery state and must
	// never fall through to a write-capable agent launch.
	if err := d.bootstrapWorktree(ctx, wtInfo.Path); err != nil {
		return nil, failOwned("worktree_bootstrap_failed", err)
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

	packet := buildTaskPacket(task, branch, rolePath, d.Config.TaskProvider.Type, d.Config.TaskProvider.ProjectID, lane, d.Config.Verification, ReplyTarget{
		Name:             d.coordinatorName(),
		ReviewSupervisor: d.reviewSupervisorName(),
		LeaseGeneration:  tok.Generation,
	})
	if len(task.Residuals) > 0 {
		section, residualErr := residual.PacketSection(task.Residuals)
		if residualErr != nil {
			return nil, failOwned("residual_packet_invalid", residualErr)
		}
		packet += "\nResidual work (does not waive acceptance criteria):\n" + section
	}
	if depProv != nil {
		packet = packet + "\n" + deps.PacketSection(depProv)
	}
	packetPath := filepath.Join(wtInfo.Path, "TASK-PACKET.md")
	if err := os.WriteFile(packetPath, []byte(packet), 0644); err != nil {
		return nil, failOwned("task_packet_write_failed", fmt.Errorf("failed to write task packet: %w", err))
	}

	// 7b. Inject the task-provider context receipt (FAC-145): every isolated
	// agent gets provider + project + task binding at spawn — NoLaunch
	// worktrees included, so a later manual/review launch inherits it too.
	baseReceipt := d.taskContext(task, wtInfo, branch, lane, opts)
	tc0, err := d.signReceipt(baseReceipt)
	if err != nil {
		return nil, failOwned("task_context_write_failed",
			fmt.Errorf("failed to sign task context: %w", err))
	}
	if err := WriteTaskContext(wtInfo.Path, tc0); err != nil {
		return nil, failOwned("task_context_write_failed",
			fmt.Errorf("failed to write task context: %w", err))
	}
	// Durable canonical copy OUTSIDE the ephemeral worktree (FAC-145):
	// approval/callback/readback bind through it after worktree GC.
	if err := StoreCanonicalReceipt(d.Worktree.RepoRoot(), tc0); err != nil {
		return nil, failOwned("task_context_write_failed",
			fmt.Errorf("failed to store canonical receipt: %w", err))
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
		if err := d.launch(ctx, opts, task, lane, wtInfo, branch, packet, result, tok, baseReceipt); err != nil {
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

// admitRunState is deliberately placed after the read-only task lookup and
// before every dispatch mutation (scope publication, worktree, board, packet,
// or launcher). The task ID makes a durable record exact-bound across retries.
func (d *Dispatcher) admitRunState(ctx context.Context, task *provider.Task) (*runstate.RunState, error) {
	if d.RunStates == nil {
		if d.Production {
			return nil, errors.New("dispatch runstate: durable run-state store is required in production")
		}
		return nil, nil
	}
	if task == nil || strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.Ref) == "" {
		return nil, fmt.Errorf("dispatch runstate: %w: incomplete task identity", runstate.ErrAmbiguous)
	}
	if d.RunStateGraph == nil && d.RunStateGraphForTask == nil {
		return nil, fmt.Errorf("dispatch runstate: %w: missing graph authority", runstate.ErrAmbiguous)
	}
	id := "dispatch:" + task.ID
	authority := runstate.Authority{Tasks: d.TaskProvider, Graph: d.RunStateGraph, GraphForTask: d.RunStateGraphForTask}
	state, err := d.RunStates.Resume(ctx, id, authority)
	if errors.Is(err, runstate.ErrNotFound) {
		graph, graphErr := d.graphRevisionForTask(ctx, task)
		if graphErr != nil || strings.TrimSpace(graph) == "" {
			if graphErr != nil {
				return nil, fmt.Errorf("dispatch runstate graph authority: %w", graphErr)
			}
			return nil, fmt.Errorf("dispatch runstate: %w: empty graph revision", runstate.ErrAmbiguous)
		}
		next, buildErr := runstate.FromTasks(id, "dispatch", task.Ref, graph, runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
		if buildErr != nil {
			return nil, fmt.Errorf("dispatch runstate build: %w", buildErr)
		}
		if _, checkpointErr := d.RunStates.Checkpoint(ctx, next, 0); checkpointErr != nil {
			return nil, fmt.Errorf("dispatch runstate checkpoint: %w", checkpointErr)
		}
		state, err = d.RunStates.Resume(ctx, id, authority)
	}
	if err != nil && !d.Production && errors.Is(err, runstate.ErrStale) {
		// A local retry owns this checkout and may have changed the board while
		// recovering a failed pane. Discard only the stale local snapshot and
		// rebuild it from the current provider evidence; hosted mode remains
		// strictly fail-closed.
		if delErr := d.RunStates.Delete(ctx, id); delErr != nil {
			return nil, fmt.Errorf("dispatch runstate stale recovery: %w", delErr)
		}
		graph, graphErr := d.graphRevisionForTask(ctx, task)
		if graphErr != nil || strings.TrimSpace(graph) == "" {
			if graphErr != nil {
				return nil, fmt.Errorf("dispatch runstate graph authority: %w", graphErr)
			}
			return nil, fmt.Errorf("dispatch runstate: %w: empty graph revision", runstate.ErrAmbiguous)
		}
		next, buildErr := runstate.FromTasks(id, "dispatch", task.Ref, graph, runstate.Policy{Lane: "dispatch", Model: "dispatch"}, 0, 0, []*provider.Task{task})
		if buildErr != nil {
			return nil, fmt.Errorf("dispatch runstate build: %w", buildErr)
		}
		if _, checkpointErr := d.RunStates.Checkpoint(ctx, next, 0); checkpointErr != nil {
			return nil, fmt.Errorf("dispatch runstate checkpoint: %w", checkpointErr)
		}
		state, err = d.RunStates.Resume(ctx, id, authority)
	}
	if err != nil {
		return nil, fmt.Errorf("dispatch runstate resume: %w", err)
	}
	if err := state.Dispatchable(task.Ref); err != nil {
		return nil, fmt.Errorf("dispatch runstate gate: %w", err)
	}
	if d.Recovery != nil {
		actor := d.RecoveryActor
		if strings.TrimSpace(actor) == "" {
			actor = "dispatch"
		}
		packet := recovery.Packet{Run: id, Task: task.ID, Actor: actor, Evidence: task.Ref, Revision: state.Revision, Graph: state.DependencyGraphRevision}
		if _, err := d.Recovery.BeginAttempt(packet); err != nil {
			return nil, fmt.Errorf("dispatch recovery gate: %w", err)
		}
	}
	return state, nil
}

func (d *Dispatcher) graphRevisionForTask(ctx context.Context, task *provider.Task) (string, error) {
	if d.RunStateGraphForTask != nil {
		return d.RunStateGraphForTask(ctx, runstate.TaskState{ID: task.ID, Ref: task.Ref})
	}
	if d.RunStateGraph == nil {
		return "", fmt.Errorf("dispatch runstate: %w: missing graph authority", runstate.ErrAmbiguous)
	}
	return d.RunStateGraph(ctx)
}

// admitEnvironmentPlan derives the live FAC-235 binding and checks all plan
// requests before scope admission. It deliberately has no side effects.
func (d *Dispatcher) admitEnvironmentPlan(ctx context.Context, task *provider.Task, opts DispatchOptions) error {
	if !d.Production {
		return nil
	}
	if d.EnvironmentPlans == nil {
		return errors.New("dispatch envplan: durable environment plan store is required in production")
	}
	if strings.TrimSpace(opts.EnvironmentPlanID) == "" {
		return errors.New("dispatch envplan: plan id is required in production")
	}
	if task == nil || d.RunStates == nil || (d.RunStateGraph == nil && d.RunStateGraphForTask == nil) {
		return errors.New("dispatch envplan: task and runstate authorities are required")
	}
	run, err := d.RunStates.Resume(ctx, "dispatch:"+task.ID, runstate.Authority{Tasks: d.TaskProvider, Graph: d.RunStateGraph, GraphForTask: d.RunStateGraphForTask})
	if err != nil {
		return fmt.Errorf("dispatch envplan runstate: %w", err)
	}
	var saved *runstate.TaskState
	for i := range run.Tasks {
		if run.Tasks[i].ID == task.ID && run.Tasks[i].Ref == task.Ref {
			saved = &run.Tasks[i]
			break
		}
	}
	if saved == nil {
		return fmt.Errorf("dispatch envplan: %w: task absent from run", envplan.ErrStale)
	}
	binding := envplan.Binding{TaskRef: task.Ref, TaskID: task.ID, Provider: d.Config.TaskProvider.Type, ProviderRevision: saved.ProviderRevision, GraphRevision: run.DependencyGraphRevision, RunID: run.ID, RunRevision: run.Revision}
	plan, err := d.EnvironmentPlans.Load(ctx, opts.EnvironmentPlanID)
	if err != nil {
		return fmt.Errorf("dispatch envplan load: %w", err)
	}
	for _, request := range plan.Requests {
		if err := d.EnvironmentPlans.Authorize(ctx, plan.ID, binding, request.Capability, time.Now().UTC()); err != nil {
			return fmt.Errorf("dispatch envplan admission %s: %w", request.Capability, err)
		}
	}
	// External effects dispatch will perform must be explicitly planned too;
	// Authorize returns ErrUnplanned when an empty/least-privilege plan omits one.
	required := []envplan.Capability{envplan.CapabilityBoardWrite}
	if !opts.NoLaunch {
		required = append(required, envplan.CapabilityCredential)
		laneName := opts.LaneName
		if laneName == "" {
			laneName = "worker"
		}
		lane, err := config.ResolveLane(d.Config, laneName)
		if err != nil {
			return fmt.Errorf("dispatch envplan lane: %w", err)
		}
		for _, cap := range lane.Capabilities {
			if cap == config.CapabilityNetwork {
				required = append(required, envplan.CapabilityNetwork)
				break
			}
		}
	}
	for _, cap := range required {
		if err := d.EnvironmentPlans.Authorize(ctx, plan.ID, binding, cap, time.Now().UTC()); err != nil {
			return fmt.Errorf("dispatch envplan required %s: %w", cap, err)
		}
	}
	return nil
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

// requireControlOrdersOrClose returns non-nil orders unchanged. A nil factory
// result is a control_factory_failed intent with local tab cleanup only.
func requireControlOrdersOrClose(h HerdrLauncher, tabID string, orders *control.CoordinatorOrders) (*control.CoordinatorOrders, error) {
	if orders != nil {
		return orders, nil
	}
	primary := fmt.Errorf("nil control orders")
	return nil, &launchFailure{
		Reason: "control_factory_failed",
		Err:    closeTabLocal(h, tabID, "control_factory_failed", primary),
	}
}

// confinementEnforcer returns the production enforcer (injected or constructed).
func (d *Dispatcher) confinementEnforcer(worktreePath string) (*confinement.Enforcer, error) {
	enf := d.Confinement
	if enf == nil {
		var err error
		enf, err = confinement.ProductionEnforcer()
		if err != nil {
			return nil, fmt.Errorf("confinement production enforcer: %w", err)
		}
		enf.ReceiptDir = filepath.Join(worktreePath, ".herd", "confinement")
		return enf, nil
	}
	if enf.Issuer == nil || enf.OS == nil {
		return nil, fmt.Errorf("confinement: injected Enforcer requires Issuer and OS backend")
	}
	if strings.TrimSpace(enf.ReceiptDir) == "" {
		cloned := *enf
		cloned.ReceiptDir = filepath.Join(worktreePath, ".herd", "confinement")
		return &cloned, nil
	}
	return enf, nil
}

// prepareConfinementOS installs the seatbelt profile + PATH wrapper and proves
// write denials. Must run before TabCreate so PATH can be injected into the pane.
func (d *Dispatcher) prepareConfinementOS(
	request launch.Request,
	wtInfo *worktree.WorktreeInfo,
) (*confinement.Enforcer, *confinement.PreparedOS, error) {
	if request.Decision == nil {
		return nil, nil, fmt.Errorf("confinement: launch decision is required")
	}
	if strings.TrimSpace(request.Decision.Harness) == "" && len(request.Decision.Argv) == 0 {
		return nil, nil, fmt.Errorf("confinement: decision harness or argv is required")
	}
	enf, err := d.confinementEnforcer(wtInfo.Path)
	if err != nil {
		return nil, nil, err
	}
	leaseGen := request.LeaseGeneration
	if leaseGen <= 0 {
		// Caller may only know lease after ownership; require positive for session dir.
		return nil, nil, fmt.Errorf("confinement: positive lease generation required for session dir")
	}
	taskRef := request.TaskRef
	if taskRef == "" {
		taskRef = "unknown-task"
	}
	branch := wtInfo.Branch
	// Production starts herdr kind = Decision.Harness. A PATH wrapper with that
	// exact name is required or sandbox-exec never wraps the agent.
	harness := strings.TrimSpace(request.Decision.Harness)
	if harness == "" {
		return nil, nil, fmt.Errorf("confinement: decision harness is required")
	}
	if harness != router.PiHarness && !router.IsVendorHarness(harness) {
		return nil, nil, fmt.Errorf("confinement: harness %q unsupported", harness)
	}
	realAgent := harness
	if len(request.Decision.HarnessArgv) > 0 && strings.TrimSpace(request.Decision.HarnessArgv[0]) != "" {
		realAgent = request.Decision.HarnessArgv[0]
	}
	// Optional extras (provider/argv0) for diagnostics only — harness is mandatory.
	extraNames := []string{request.Decision.Provider, request.Decision.Argv[0]}
	// Builder lanes must not be able to route around the review supervisor from
	// their shell. These wrappers are installed in the same frozen,
	// coordinator-owned PATH directory as the harness wrapper, so gh/git resolve
	// through the structural command gate rather than prompt prose.
	if request.Decision.Role == router.RoleWorker || request.Decision.Role == router.RoleForgeSmith || request.Decision.Role == router.RoleRecovery {
		extraNames = append(extraNames, "gh", "git")
	}
	prep, err := enf.PrepareOS(
		wtInfo.Path,
		d.Worktree.RepoRoot(),
		taskRef,
		leaseGen,
		branch,
		harness,
		realAgent,
		extraNames...,
	)
	if err != nil {
		return nil, nil, err
	}
	if !prep.WrapperResolves(harness) {
		return nil, nil, fmt.Errorf("confinement: harness wrapper %q not installed (agent would not be sandboxed)", harness)
	}
	return enf, prep, nil
}

// bindConfinement is the FAC-190 post-tab gate: MAC-authenticated worktree
// binding attached to the PreparedOS proof that already wraps the agent PATH.
func (d *Dispatcher) bindConfinement(
	enf *confinement.Enforcer,
	prep *confinement.PreparedOS,
	request launch.Request,
	task *provider.Task,
	lane *config.LaneDef,
	wtInfo *worktree.WorktreeInfo,
	result *DispatchResult,
	tab *herdr.TabInfo,
	tabLabel string,
) error {
	if d == nil || enf == nil || task == nil || lane == nil || wtInfo == nil || result == nil || tab == nil {
		return fmt.Errorf("confinement: incomplete launch context")
	}
	if request.Decision == nil {
		return fmt.Errorf("confinement: launch decision is required")
	}
	leaseGen := result.LeaseGeneration
	if leaseGen <= 0 {
		leaseGen = request.LeaseGeneration
	}
	if leaseGen <= 0 {
		return fmt.Errorf("confinement: lease generation is required")
	}
	// Session generation MUST be the one PrepareToolChildLifecycle reserved for
	// the live agent. Do not mint a second durable generation here — that
	// burned N+1 into the AuthTuple while the agent ran under N.
	sessionGen := request.SessionGeneration
	if sessionGen <= 0 {
		return fmt.Errorf("confinement: session generation required after tool-child lifecycle prep")
	}
	processIdentity := strings.TrimSpace(request.ProcessIdentity)
	if processIdentity == "" {
		processIdentity = fmt.Sprintf("prestart:%s:%s:%s", tab.ID, tab.Pane.ID, launch.DecisionDigest(request.Decision))
	}
	session := strings.TrimSpace(request.HerdrSession)
	if session == "" {
		session = tabLabel
	}
	// Prefer harness argv (what AgentStart actually runs); fall back to Argv.
	argv := append([]string(nil), request.Decision.HarnessArgv...)
	if len(argv) == 0 {
		argv = append(argv, request.Decision.Argv...)
	}
	if len(argv) == 0 {
		return fmt.Errorf("confinement: compiled harness argv is required")
	}
	id := confinement.LaunchIdentity{
		Repository:        request.Repository,
		Task:              task.Ref,
		LeaseGeneration:   leaseGen,
		Lane:              lane.Name,
		Session:           session,
		SessionGeneration: sessionGen,
		HerdrTab:          tab.ID,
		HerdrPane:         tab.Pane.ID,
		ProcessIdentity:   processIdentity,
		Argv:              argv,
		WorktreeRoot:      wtInfo.Path,
		SharedRoot:        d.Worktree.RepoRoot(),
		AgentKind:         request.Decision.Harness,
	}
	binding, err := enf.BindAndProve(id, prep)
	if err != nil {
		return err
	}
	if !binding.WrapperInstalled || !binding.OSProved {
		return fmt.Errorf("confinement: binding missing wrapper install or OS proof")
	}
	// Persist receipt under the session dir (outside worktree) when available;
	// fall back to worktree only for diagnostics (HMAC still authenticates).
	receiptDir := ""
	if prep != nil && prep.Session.Root != "" {
		receiptDir = prep.Session.Root
	} else {
		receiptDir = filepath.Join(wtInfo.Path, ".herd", "confinement")
	}
	receiptPath := filepath.Join(receiptDir, "last-binding.json")
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o755); err != nil {
		return err
	}
	// Thaw prior freeze (dir 0755 + file 0444) so same-lease relaunch can rewrite.
	_ = os.Chmod(filepath.Dir(receiptPath), 0o755)
	_ = os.Chmod(receiptPath, 0o644)
	raw, err := binding.MarshalReceipt()
	if err != nil {
		return err
	}
	if err := os.WriteFile(receiptPath, raw, 0o644); err != nil {
		return err
	}
	_ = os.Chmod(receiptPath, 0o444)
	return nil
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
	baseReceipt TaskContext,
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

	// Normalize production lease/task identity for confinement + control.
	if d.Production {
		if request.LeaseGeneration <= 0 && result.LeaseGeneration > 0 {
			request.LeaseGeneration = result.LeaseGeneration
		}
		if request.TaskRef == "" {
			request.TaskRef = task.Ref
		}
	}

	// Two launch paths, and they are mutually EXCLUSIVE by design — correcting
	// 373ec213's commit message, which claimed FAC-190 confinement still ran
	// "alongside" this one. It does not, and it must not: both install a
	// seatbelt PATH wrapper named after the harness, so running both would put
	// two rival `pi` wrappers on disk with only one reachable on PATH, and
	// bindConfinement's BindAndProve receipt would attest a wrapper the agent
	// never execs. A receipt for a dead wrapper is worse than no receipt.
	//
	// Production sets HERD_CONTROL_SECRET, so production takes the Control
	// path. Each FAC-190 guarantee is carried, not dropped:
	//   seatbelt profile    -> RequireContainment().Install + ProveDenials,
	//                          which actively probes denials (FAC-190 did not)
	//   WrapperResolves     -> security.requireWrapperOnPATH, asserted against
	//                          the constructed agent env inside LaunchAgent
	//   MAC-authenticated   -> VerifyAndEnforceControl before Install, then
	//   re-proof               bindLaunchControlKind (IssueAndEnforce + sealed
	//                          control + profile reinstall) after start
	// The FAC-190 else-branch remains for non-Control (non-production) callers.
	var tabID, paneID string
	var boundSession string
	var spawner *launcherSpawner
	if d.Control != nil {
		grant, policy, aerr := d.authorizeAgentSandbox(lane, task, wtInfo.Path)
		if aerr != nil {
			return &launchFailure{Reason: "sandbox_denied", Err: aerr}
		}
		result.SandboxGranted = true
		if opts.LeaseGeneration <= 0 {
			return &launchFailure{
				Reason: "lease_missing",
				Err:    fmt.Errorf("%w: DispatchOptions.LeaseGeneration must be >0 (live claim/control lease required)", security.ErrUnknownPolicy),
			}
		}
		leaseGen, lerr := security.LeaseFromOpts(opts.LeaseGeneration)
		if lerr != nil {
			return &launchFailure{Reason: "lease_invalid", Err: lerr}
		}
		lookup := d.ClaimLookup
		if lookup == nil {
			lookup = security.ResolveClaimLookup()
		}
		if err := security.ValidateLiveTaskLease(ctx, lookup, task.Ref, leaseGen, false, "", ""); err != nil {
			return &launchFailure{Reason: "lease_not_live", Err: err}
		}
		if err := security.RequireFleetReady(); err != nil {
			return &launchFailure{Reason: "fleet_blocked", Err: err}
		}
		eventLog := d.SecurityEventLog
		if eventLog == "" && d.Worktree != nil {
			eventLog = filepath.Join(d.Worktree.RepoRoot(), ".herd", "security-events.jsonl")
		}
		scope, scErr := launchControlScope(policy, wtInfo.Path)
		if scErr != nil {
			return &launchFailure{Reason: "package_provenance_unknown", Err: scErr}
		}
		if err := security.ApplyControlScopeToPolicy(policy, grant, scope); err != nil {
			return &launchFailure{Reason: "package_scope_apply_failed", Err: err}
		}
		spawner = newLauncherSpawner(h, request)
		skipContain := !d.Production
		spawn, serr := security.LaunchAgent(spawner, security.AgentSpawnRequest{
			Policy:          policy,
			Grant:           grant,
			Name:            tabLabel,
			Kind:            request.Decision.Harness,
			Model:           model,
			Workspace:       ws,
			Label:           tabLabel,
			NoFocus:         true,
			EventLogPath:    eventLog,
			TaskRef:         task.Ref,
			LeaseGeneration: leaseGen,
			ClaimLookup:     lookup,
			SessionResolver: spawner,
			SkipContainment: skipContain,
			ControlSecret:   d.Control.Secret,
			Ambient: map[string]string{
				"HERD_EXPECTED_TASK":  task.Ref,
				"HERD_EXPECTED_LEASE": leaseGen,
			},
		})
		if serr != nil {
			tid := ""
			if spawn != nil {
				tid = spawn.TabID
			}
			reason := "sandbox_spawn_failed"
			if strings.Contains(serr.Error(), "agent start") {
				reason = "agent_start_failed"
			} else if strings.Contains(serr.Error(), "tab create") {
				reason = "tab_create_failed"
			}
			if tid != "" {
				return &launchFailure{Reason: reason, Err: closeTabLocal(h, tid, reason, serr)}
			}
			return &launchFailure{Reason: reason, Err: serr}
		}
		tabID, paneID = spawn.TabID, spawn.PaneID
		result.TabID = tabID
		result.AgentName = tabLabel
		if sink := eventCount(policy); sink > 0 {
			result.SecurityEvents = sink
		}
		if err := herdr.PrepareToolChildLifecycle(tabID, paneID, &request, tabLabel); err != nil {
			return &launchFailure{Reason: "tool_child_lifecycle_failed", Err: closeTabLocal(h, tabID, "tool_child_lifecycle_failed", err)}
		}
		if request.SessionGeneration <= 0 {
			return &launchFailure{
				Reason: "tool_child_lifecycle_failed",
				Err:    closeTabLocal(h, tabID, "tool_child_lifecycle_failed", fmt.Errorf("tool-child lifecycle left session generation unset")),
			}
		}
		if err := d.record(ctx, StepRecord{
			TicketRef: task.Ref, Step: StepTab, Worktree: wtInfo.Path, Branch: branch,
			TabID: tabID, PaneID: paneID, AgentName: tabLabel,
		}); err != nil {
			return &launchFailure{Reason: "record_tab_failed", Err: closeTabLocal(h, tabID, "record_tab_failed", err)}
		}
		if err := d.record(ctx, StepRecord{
			TicketRef: task.Ref, Step: StepAgentStart, Worktree: wtInfo.Path, Branch: branch,
			TabID: tabID, PaneID: paneID, AgentName: tabLabel,
		}); err != nil {
			return &launchFailure{Reason: "record_agent_start_failed", Err: closeTabLocal(h, tabID, "record_agent_start_failed", err)}
		}
		// Bind control to live worker identity (session optional for grok).
		wantSess := strings.TrimSpace(spawn.AgentSessionID)
		if err := security.RefuseProvisionalWorkerSession(wantSess); err != nil {
			wantSess = ""
		}
		agentSess := wantSess
		if live, lerr := spawner.requireLiveIdentity(tabLabel, wantSess, tabID, paneID); lerr == nil && live != nil {
			if bid, berr := bindingID(live); berr == nil {
				agentSess = bid
			}
		} else if d.Production {
			return &launchFailure{Reason: "session_drift", Err: closeTabLocal(h, tabID, "session_drift", lerr)}
		}
		if err := security.RefuseProvisionalWorkerSession(agentSess); err != nil {
			return &launchFailure{Reason: "control_bind_failed", Err: closeTabLocal(h, tabID, "control_bind_failed", err)}
		}
		reinstallKind := ""
		if d.Production {
			reinstallKind = request.Decision.Harness
		}
		if err := d.bindLaunchControlKind(agentSess, task.Ref, opts.LeaseGeneration, wtInfo.Path, policy, grant, reinstallKind, ""); err != nil {
			return &launchFailure{Reason: "control_bind_failed", Err: closeTabLocal(h, tabID, "control_bind_failed",
				fmt.Errorf("control-plane launch binding failed: %w", err))}
		}
		result.ControlBound = true
		boundSession = agentSess
	} else {
		// FAC-190 non-Control path: prepare OS profile + agent PATH wrapper.
		var (
			confEnf  *confinement.Enforcer
			confPrep *confinement.PreparedOS
		)
		if d.Production {
			var perr error
			confEnf, confPrep, perr = d.prepareConfinementOS(request, wtInfo)
			if perr != nil {
				return &launchFailure{Reason: "confinement_rejected", Err: perr}
			}
		}
		var tabEnv []string
		if confPrep != nil {
			var eerr error
			tabEnv, eerr = confPrep.TabEnv(wtInfo.Path, os.Getenv("PATH"))
			if eerr != nil {
				return &launchFailure{Reason: "confinement_rejected", Err: eerr}
			}
			for _, n := range confPrep.Names {
				if !confPrep.WrapperResolves(n) {
					return &launchFailure{Reason: "confinement_rejected", Err: fmt.Errorf("confinement: wrapper %q not installed", n)}
				}
			}
		}
		// Stamp the resolved herdr workspace into the launch receipt (FAC-145)
		// so the agent's callbacks bind to the exact workspace it lives in.
		// SAME issued receipt (identical expiry and identity), with only the
		// sanctioned same-generation transition: the herdr workspace stamp.
		tc := baseReceipt
		tc.HerdrWorkspace = ws
		tc, err = d.signReceipt(tc)
		if err != nil {
			return &launchFailure{
				Reason: "task_context_write_failed",
				Err:    fmt.Errorf("failed to sign task context: %w", err),
			}
		}
		if err := WriteTaskContext(wtInfo.Path, tc); err != nil {
			return &launchFailure{
				Reason: "task_context_write_failed",
				Err:    fmt.Errorf("failed to stamp herdr workspace into task context: %w", err),
			}
		}
		if err := StoreCanonicalReceipt(d.Worktree.RepoRoot(), tc); err != nil {
			return &launchFailure{
				Reason: "task_context_write_failed",
				Err:    fmt.Errorf("failed to store canonical receipt: %w", err),
			}
		}
		// FAC-139: write-capable Tab creation goes only through the launch boundary
		// (LaunchDecision + current artifact tool-probe PASS). Direct TabCreate is
		// forbidden on this path.
		probe, perr := d.resolveToolProbe(opts, request.Decision)
		if perr != nil {
			return &launchFailure{Reason: "tool_probe_rejected", Err: perr}
		}
		plan, tID, pID, lErr := launch.Open(dispatchTabOpener{h: h}, launch.BoundarySpec{
			Decision:  request.Decision,
			Request:   request,
			Probe:     probe,
			Lane:      lane,
			Workspace: ws,
			Label:     tabLabel,
			Cwd:       wtInfo.Path,
			Env:       tabEnv,
			NoFocus:   true,
		})
		if lErr != nil {
			return &launchFailure{
				Reason: "tab_create_failed",
				Err:    fmt.Errorf("worktree ready but launch boundary rejected tab: %w", lErr),
			}
		}
		if plan != nil && plan.Model != "" {
			result.Model = plan.Model
		}
		tab := &herdr.TabInfo{ID: tID, Label: tabLabel, Cwd: wtInfo.Path, Pane: herdr.PaneInfo{ID: pID, TabID: tID}}
		tabID, paneID = tab.ID, tab.Pane.ID
		result.TabID = tabID
		result.AgentName = tabLabel
		if err := herdr.PrepareToolChildLifecycle(tabID, paneID, &request, tabLabel); err != nil {
			return &launchFailure{Reason: "tool_child_lifecycle_failed", Err: closeTabLocal(h, tabID, "tool_child_lifecycle_failed", err)}
		}
		if request.SessionGeneration <= 0 {
			return &launchFailure{
				Reason: "tool_child_lifecycle_failed",
				Err:    closeTabLocal(h, tabID, "tool_child_lifecycle_failed", fmt.Errorf("tool-child lifecycle left session generation unset")),
			}
		}
		if err := d.record(ctx, StepRecord{
			TicketRef: task.Ref, Step: StepTab, Worktree: wtInfo.Path, Branch: branch,
			TabID: tabID, PaneID: paneID, AgentName: tabLabel,
		}); err != nil {
			return &launchFailure{Reason: "record_tab_failed", Err: closeTabLocal(h, tabID, "record_tab_failed", err)}
		}
		if d.Production {
			if err := d.bindConfinement(confEnf, confPrep, request, task, lane, wtInfo, result, tab, tabLabel); err != nil {
				return &launchFailure{Reason: "confinement_rejected", Err: closeTabLocal(h, tabID, "confinement_rejected", err)}
			}
		}
		if err := h.AgentStart(request, tabLabel, request.Decision.Harness, paneID); err != nil {
			return &launchFailure{
				Reason: "agent_start_failed",
				Err: closeTabLocal(h, tabID, "agent_start_failed",
					fmt.Errorf("worktree ready but agent start failed: %w", err)),
			}
		}
		if err := d.record(ctx, StepRecord{
			TicketRef: task.Ref, Step: StepAgentStart, Worktree: wtInfo.Path, Branch: branch,
			TabID: tabID, PaneID: paneID, AgentName: tabLabel,
		}); err != nil {
			return &launchFailure{Reason: "record_agent_start_failed", Err: closeTabLocal(h, tabID, "record_agent_start_failed", err)}
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
	wakeTarget := control.WakeTarget{Target: tabLabel, Workspace: ws, TabID: tabID, PaneID: paneID, AgentName: tabLabel, Provider: request.Decision.Harness, LeaseGeneration: result.LeaseGeneration}
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
			return &launchFailure{Reason: "control_target_drift", Err: closeTabLocal(h, tabID, "control_target_drift", err)}
		}
		if err == nil {
			wakeTarget = actual
		}
	} else if d.Production {
		return &launchFailure{Reason: "control_target_unverifiable", Err: closeTabLocal(h, tabID, "control_target_unverifiable", fmt.Errorf("Herdr launcher cannot verify exact target"))}
	}
	var evidence control.Evidence
	if d.Production {
		orders, factoryErr := d.ControlFactory(ctx, ControlScope{Identity: identity, Wake: wakeTarget, Check: check})
		if factoryErr != nil {
			return &launchFailure{Reason: "control_factory_failed", Err: closeTabLocal(h, tabID, "control_factory_failed", factoryErr)}
		}
		var orderGuardErr error
		orders, orderGuardErr = requireControlOrdersOrClose(h, tabID, orders)
		if orderGuardErr != nil {
			return orderGuardErr
		}
		var orderErr error
		evidence, orderErr = orders.Repair(ctx, packet)
		if orderErr != nil {
			return &launchFailure{Reason: "control_order_failed", Err: closeTabLocal(h, tabID, "control_order_failed", orderErr)}
		}
	} else if d.Orders != nil {
		if e, orderErr := d.Orders.Repair(ctx, packet); orderErr != nil {
			return &launchFailure{Reason: "control_order_failed", Err: closeTabLocal(h, tabID, "control_order_failed", orderErr)}
		} else {
			evidence = e
		}
	}
	// Pre-delivery live identity ownership (liveLookup only; session optional).
	if spawner != nil && boundSession != "" && d.Production {
		if _, lerr := spawner.requireLiveIdentity(tabLabel, boundSession, tabID, paneID); lerr != nil {
			return &launchFailure{Reason: "session_drift", Err: closeTabLocal(h, tabID, "session_drift", lerr)}
		}
	}

	var receipt *herdr.PromptReceipt
	var receiptErr error
	if evidence.MessageID != "" {
		// Delivery already performed the one Herdr wake and returned its actual
		// receipt. Never nudge the same order again from this outer layer.
		if !evidence.Wake.Consumed || !evidence.Wake.Verified {
			return &launchFailure{Reason: "prompt_receipt_invalid", Err: closeTabLocal(h, tabID, "prompt_receipt_invalid", fmt.Errorf("durable control wake did not prove consumption"))}
		}
		receipt = &herdr.PromptReceipt{Target: evidence.Wake.Target, Consumed: evidence.Wake.Consumed, Verified: evidence.Wake.Verified, BaselineStatus: evidence.Wake.Baseline, FinalStatus: evidence.Wake.Final, SequenceToken: evidence.Wake.SequenceToken}
	} else {
		// Explicit non-production test mode has no durable control port; it still
		// sends only a fixed wake reference, never the task packet.
		// control.WakeTextForTask, not a hand-copied string: a second copy is a
		// second place that can regress to a bare protocol directive with a green
		// suite, and the two had already drifted apart in wording.
		// LiveHerdr.DeliverAndProve uses AgentPromptExact when a live session exists.
		receipt, receiptErr = h.DeliverAndProve(tabLabel, control.WakeTextForTask(task.Ref), timeout)
	}
	result.Receipt = receipt
	if receiptErr != nil {
		return &launchFailure{
			Reason: "prompt_delivery_failed",
			Err: closeTabLocal(h, tabID, "prompt_delivery_failed",
				fmt.Errorf("worktree ready but prompt consumption not proven: %w", err)),
		}
	}
	// Post-delivery live identity ownership — same liveLookup path.
	if spawner != nil && boundSession != "" && d.Production {
		if _, lerr := spawner.requireLiveIdentity(tabLabel, boundSession, tabID, paneID); lerr != nil {
			return &launchFailure{Reason: "session_drift_post", Err: closeTabLocal(h, tabID, "session_drift_post", lerr)}
		}
	}
	if receipt == nil || !receipt.Consumed || !receipt.Verified {
		return &launchFailure{
			Reason: "prompt_receipt_invalid",
			Err: closeTabLocal(h, tabID, "prompt_receipt_invalid",
				fmt.Errorf("worktree ready but prompt receipt did not prove consumption")),
		}
	}
	if !herdr.ConsumptionProven(receipt.BaselineStatus, receipt.FinalStatus) {
		return &launchFailure{
			Reason: "prompt_sequence_invalid",
			Err: closeTabLocal(h, tabID, "prompt_sequence_invalid",
				fmt.Errorf("prompt receipt sequence %q is not a valid consumption proof", receipt.SequenceToken)),
		}
	}

	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepPrompt,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tabID,
		AgentName: tabLabel,
		Receipt:   receipt.SequenceToken,
		MessageID: evidence.MessageID,
		Sequence:  evidence.Sequence,
	}); err != nil {
		return &launchFailure{
			Reason: "record_prompt_failed",
			Err:    closeTabLocal(h, tabID, "record_prompt_failed", err),
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

// signReceipt issues the receipt with the coordinator's private key (kept
// outside the repository tree; see authority.go). No signer, no dispatch.
func (d *Dispatcher) signReceipt(tc TaskContext) (TaskContext, error) {
	s, err := LoadSignerForConfig(d.Config.Project.Name, d.Worktree.RepoRoot())
	if err != nil {
		return tc, fmt.Errorf("receipt signer: %w", err)
	}
	return s.Issue(tc)
}

// taskContext builds the FAC-145 launch receipt for one dispatched task.
// HerdrWorkspace is stamped later by launch() once RequireWorkspace resolves.
// Agent receipts never carry the mutate op: board transitions stay
// coordinator-owned.
func (d *Dispatcher) taskContext(task *provider.Task, wtInfo *worktree.WorktreeInfo, branch string, lane *config.LaneDef, opts DispatchOptions) TaskContext {
	// No silent role defaulting: the lane must state a known role, and the
	// op set follows the role policy strictly.
	role := ""
	if lane != nil {
		role = strings.TrimSpace(lane.Role)
	}
	// Every isolated agent role gets its sanctioned op set; an unknown role
	// yields nil and the receipt fails Validate before any launch.
	ops := OpsForRole(role)
	return TaskContext{
		ProviderType:      d.Config.TaskProvider.Type,
		ProjectID:         d.Config.TaskProvider.ProjectID,
		ProviderWorkspace: d.Config.TaskProvider.WorkspaceID,
		ProviderProfile:   d.Config.TaskProvider.APIKeyEnv,
		Repository:        RepositoryIdentityOrName(d.Worktree.RepoRoot(), d.Config.Project.Name),
		Role:              role,
		TaskRef:           task.Ref,
		TaskID:            task.ID,
		Branch:            branch,
		BaseSHA:           wtInfo.BaseSHA,
		AnchorRef:         wtInfo.AnchorRef,
		LeaseID:           opts.LeaseID,
		LeaseGeneration:   opts.LeaseGeneration,
		LeaseTaskRef:      task.Ref,
		SessionID:         NewSessionID(role, task.Ref, wtInfo.BaseSHA, opts.LeaseID),
		AllowedOps:        ops,
		ExpiresAt:         time.Now().Add(DefaultReceiptTTL),
	}
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
// FAC-155: the packet names the configured provider AND its project binding,
// and forbids every other board tool. A worker that reaches for a different
// provider CLI is reaching for a board this repository never activated.
// FAC-222: the packet carries a ReplyTarget so agents report completion and
// BLOCKED to the coordinator by name, instead of relying on the coordinator
// to notice by polling. Polling stays as the backstop, not the primary signal.
//
// FAC-145: the read itself goes through the receipt-gated broker
// (`herd task get`) rather than any direct provider CLI: the broker reads the
// worktree's signed TASK-CONTEXT.json, so the agent's very first board read
// binds to the exact provider/project/task with no ambient credentials and no
// provider-native context file.
func buildTaskPacket(task *provider.Task, branch, rolePath, taskProviderType, taskProviderProject string, lane *config.LaneDef, verification config.Verification, reply ReplyTarget) string {
	var b strings.Builder

	verifySummary := verification.TestCommand
	verifyFlags := fmt.Sprintf("--test %q", verification.TestCommand)
	if verification.PreflightCommand != "" {
		verifySummary = verification.PreflightCommand + " && " + verification.TestCommand
		verifyFlags = fmt.Sprintf("--build %q %s", verification.PreflightCommand, verifyFlags)
	}

	fmt.Fprintf(&b, "BUILD %s — EXECUTE. No menus, no questions. Do not stop until "+
		"`%s` passes AND you have committed.\n\n", task.Ref, verifySummary)

	fmt.Fprintf(&b, "Worktree: current directory (Herdr cwd-enforced), branch %s. Work ONLY here — never edit files outside it.\n", branch)
	// The wake is delivered through the same pane as automated Stop-hook
	// output. Put the assignment's provenance in the lane-local packet so the
	// lane can verify it from its own context instead of trusting transport
	// wording. Keep this compact: packets are deliberately context-light.
	coordinator := reply.Name
	if coordinator == "" {
		coordinator = "coordinator"
	}
	fmt.Fprintf(&b, "ASSIGNMENT ENVELOPE: ADDRESSED ASSIGNMENT; issuer: %s; task_ref: %s; task_id: %s; lease_generation: %d; ASSIGNMENT ENVELOPE END.\n", coordinator, task.Ref, task.ID, reply.LeaseGeneration)

	fmt.Fprintf(&b, "Read the full spec yourself (do not wait for it inline) via the receipt-gated broker (provider=%s project=%s):\n", taskProviderType, taskProviderProject)
	fmt.Fprintf(&b, "  herd task get %s --full\n\n", task.Ref)
	// The forbid line names no adapter: enumerating them would both go stale
	// and hand a non-kaneo worker the string "kaneo" to reach for.
	fmt.Fprintf(&b, "This repository activates ONLY the provider and project named above. Do NOT read from or write to any other task board, and do NOT invoke any other provider CLI or API — no other provider tool is authorized for this task.\n\n")

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

	// FAC-222: completion authority remains the coordinator, while review
	// handoff authority is the standing review supervisor. Keeping those
	// addresses distinct prevents the coordinator from becoming a review queue.
	coordinatorName := reply.Name
	if coordinatorName == "" {
		coordinatorName = "coordinator"
	}
	fmt.Fprintf(&b, "Report to the coordinator (%s) — do not wait to be asked:\n", coordinatorName)
	reviewSupervisor := reply.ReviewSupervisor
	if strings.TrimSpace(reviewSupervisor) == "" {
		reviewSupervisor = "review-supervisor"
	}
	fmt.Fprintf(&b, "  READY-FOR-REVIEW: post the completion callback, then ping the review supervisor (%s) with the exact SHA; do not ask the coordinator to run review.\n", reviewSupervisor)
	fmt.Fprintf(&b, "  Completion callback: herd shot %s --report complete --sha <sha> --lease %d\n", task.Ref, reply.LeaseGeneration)
	fmt.Fprintf(&b, "  BLOCKED: herd shot %s --report blocked --detail \"<why>\" --lease %d\n", task.Ref, reply.LeaseGeneration)
	b.WriteString("Report as soon as the condition is true. Polling is the backstop, not the primary signal.\n\n")

	b.WriteString("Do NOT push, PR, or merge — the coordinator harvests your branch. Do NOT touch the root checkout.\n")
	if lane != nil && rolePath != "" {
		fmt.Fprintf(&b, "Role contract: %s\n", rolePath)
	}
	return b.String()
}

// deriveScopeFromProvenance builds a scopefence.Scope from the provenance's
// declared scope_packages/scope_files. When the coordinator did not declare
// a scope, falls back to collecting paths from holds (collision_ownership
// holds carry Paths). An empty scope is returned — the caller validates and
// fails closed if no scope could be derived.
func deriveScopeFromProvenance(p *deps.Provenance) scopefence.Scope {
	if p == nil {
		return scopefence.Scope{}
	}
	scope := scopefence.Scope{
		Packages: append([]string(nil), p.ScopePackages...),
		Files:    append([]string(nil), p.ScopeFiles...),
	}
	if len(scope.Packages) > 0 || len(scope.Files) > 0 {
		return scope
	}
	for _, h := range p.Holds {
		scope.Files = append(scope.Files, h.Paths...)
	}
	return scope
}

// countTrackedFiles returns the number of files tracked by git at HEAD in the
// given repo root. Returns 0 on any error (caller falls back to a minimum).
func countTrackedFiles(repoRoot string) int {
	if repoRoot == "" {
		repoRoot = "."
	}
	out, err := exec.Command("git", "-C", repoRoot, "ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(l) != "" {
			count++
		}
	}
	return count
}
