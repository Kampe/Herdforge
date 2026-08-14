package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/memory"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// ErrClaimWithoutLaunch is returned when a daemon tick would leave an In Progress
// board task with no verified worker. Production must never report success after
// RunPulse without a proven launch or generation-fenced compensation.
var ErrClaimWithoutLaunch = errors.New("daemon: claim without verified worker launch is forbidden")

// ErrDaemonPrep is returned for non-compensable launch preparation failures
// that must occur before any board claim (missing lane, invalid model, Herdr
// unavailable, empty repository identity).
var ErrDaemonPrep = errors.New("daemon: launch preparation refused before claim")

// WorkerLauncher is the Herdr surface the daemon tick uses after a fenced claim.
// Production wires dispatch.LiveHerdr (or an adapter that embeds it). Tests inject fakes.
// TabCreateForTask matches dispatch.HerdrLauncher / LiveHerdr: optional env is
// the confinement/seatbelt seat (FAC-190); callers may pass zero env vars.
type WorkerLauncher interface {
	Available() bool
	RequireWorkspace(repoRoot string) (string, error)
	TabCreateForTask(workspaceID, label, cwd string, noFocus bool, env ...string) (*herdr.TabInfo, error)
	AgentStart(req launch.Request, name, kind, paneID string) error
	DeliverAndProve(target, text string, timeout time.Duration) (*herdr.PromptReceipt, error)
	TabClose(tabID string) error
}

// Compile-time proof: production LiveHerdr satisfies WorkerLauncher without a
// parallel Herdr surface. A broken adapter would fail to compile this package.
var _ WorkerLauncher = dispatch.LiveHerdr{}

// StandingAgent is an already-running worker that may be reused only after
// identity/model/worktree readback matches the tick's decision.
type StandingAgent struct {
	Name    string
	TabID   string
	PaneID  string
	Session string
	CWD     string
	Model   string
	Harness string
}

// StandingResolver looks up a standing agent by name. A nil resolver always
// creates an ephemeral task tab. Non-nil resolvers must return (nil, nil) when
// no exact standing agent exists.
type StandingResolver func(ctx context.Context, name string, req launch.Request) (*StandingAgent, error)

// TickOptions configures one claim-to-dispatch daemon transaction (FAC-196).
// Decision, Lane, Repository, and Herdr are required before any board claim.
type TickOptions struct {
	// Decision is a pre-admitted LaunchDecision (FAC-175 / FAC-194 routing).
	// Must already prove role/shape/provider/model/effort/argv; the tick rebinds
	// it to the claimed task ref and lease generation after RunPulse.
	Decision *router.LaunchDecision
	// Lane is the configured worker lane for this role.
	Lane *config.LaneDef
	// Repository is the authenticated repository identity (never a display name).
	Repository string
	// Herdr launches or reuses the exact worker session.
	Herdr WorkerLauncher
	// Worktree creates the isolated task worktree. Required unless Standing
	// supplies a verified cwd and CreateWorktree is false.
	Worktree dispatch.WorktreeService
	// CreateWorktree defaults true. Set false only when Standing reuse supplies
	// a verified isolated cwd that already matches the task worktree.
	CreateWorktree *bool
	// StandingName is the forge-<lane> standing agent label to try first.
	StandingName string
	// ResolveStanding is optional identity readback for standing reuse.
	ResolveStanding StandingResolver
	// Lifecycle is optional durable projection (Claimed → Dispatched).
	Lifecycle *lifecycle.Machine
	// PromptTimeout bounds DeliverAndProve (default 60s).
	PromptTimeout time.Duration
	// Packet builds the wake packet body. When nil, a compact task summary is used
	// for DeliverAndProve; production should still land TASK-PACKET.md via Dispatch
	// or write it before prove. Tests inject fixed packets for digest proof.
	Packet func(task *provider.Task, lane *config.LaneDef, worktreePath string) string
	// ScopedMemory optionally injects reviewed/global or task-authorized memory
	// into the default packet. Nil preserves the historic packet unchanged.
	// The caller supplies the authenticated actor and durable run ID; the daemon
	// supplies the claimed task ref, lane role, and live graph revision.
	ScopedMemory      *memory.ScopedMemoryStore
	ScopedMemoryActor memory.Actor
	ScopedMemoryRunID string
	// DefaultBranch is used for worktree create (default origin/main's branch name).
	DefaultBranch string
}

// TickReceipt binds every identity of a successful claim-to-dispatch tick.
type TickReceipt struct {
	TaskRef          string
	TaskID           string
	LeaseGeneration  int64
	GraphRevision    string
	ProviderRevision string
	Repository       string
	Lane             string
	Worktree         string
	Branch           string
	TabID            string
	PaneID           string
	SessionID        string
	AgentName        string
	Model            string
	Effort           string
	Harness          string
	Argv             []string
	PacketDigest     string
	PromptSequence   string
	Launched         bool
	ReusedStanding   bool
	LifecycleState   lifecycle.State
	OwnershipOwnerID string
}

// RunDaemonTick is the FAC-196 claim-to-dispatch transaction for herd daemon.
//
// Ordering (hard):
//  1. Non-compensable prep: lane, decision, repository, Herdr availability.
//  2. Fenced claim via RunPulse (durable lease + board In Progress).
//  3. Rebind decision to task+generation; create/resolve worktree; start or
//     reuse exact worker; prove packet consumption.
//  4. On any failure after claim: generation-fenced board reverse + lease
//     release (or retain lease when compensation is uncertain).
//
// A successful tick proves one working agent consumed the exact packet.
// Calling RunPulse alone and returning after "Claimed" is forbidden.
func (e *Engine) RunDaemonTick(ctx context.Context, role string, opts TickOptions) (*TickReceipt, error) {
	if e == nil {
		return nil, fmt.Errorf("daemon: engine is required")
	}
	if err := prepareDaemonTick(opts); err != nil {
		return nil, err
	}

	task, err := e.RunPulse(ctx, role)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, nil
	}

	tok := e.LastClaimToken()
	if tok == nil || string(tok.TaskRef) != task.Ref || tok.Generation == 0 {
		// Claim without a retained ownership token cannot be compensated safely.
		// Surface BLOCKED rather than free capacity.
		return nil, fmt.Errorf("%w: ownership token missing after claim for %s", ErrClaimWithoutLaunch, task.Ref)
	}

	receipt, launchErr := e.launchAfterClaim(ctx, task, tok, opts)
	if launchErr != nil {
		if cErr := e.compensateAfterClaim(ctx, task, tok, "daemon_launch_failed"); cErr != nil {
			return nil, errors.Join(fmt.Errorf("%w: %v", ErrClaimWithoutLaunch, launchErr), cErr)
		}
		return nil, fmt.Errorf("%w: %v", ErrClaimWithoutLaunch, launchErr)
	}
	return receipt, nil
}

// prepareDaemonTick validates non-compensable launch prerequisites before any
// board claim. Failures here leave zero In Progress orphans.
func prepareDaemonTick(opts TickOptions) error {
	if opts.Lane == nil || strings.TrimSpace(opts.Lane.Name) == "" {
		return fmt.Errorf("%w: configured lane is required", ErrDaemonPrep)
	}
	if strings.TrimSpace(opts.Repository) == "" {
		return fmt.Errorf("%w: authenticated repository identity is required", ErrDaemonPrep)
	}
	if opts.Decision == nil {
		return fmt.Errorf("%w: pre-admitted LaunchDecision is required", ErrDaemonPrep)
	}
	if err := router.VerifyDecision(opts.Decision, opts.Decision.TaskRef, opts.Decision.LeaseGeneration); err != nil {
		return fmt.Errorf("%w: launch decision: %v", ErrDaemonPrep, err)
	}
	// Validate the decision as a worker boundary before claim. Scope may still
	// be lane-level; rebind happens after claim with the lease generation.
	req := launch.Request{
		Decision:        opts.Decision,
		TaskRef:         opts.Decision.TaskRef,
		LeaseGeneration: opts.Decision.LeaseGeneration,
		Scope:           opts.Decision.Scope,
		Repository:      opts.Repository,
		Lane:            opts.Lane.Name,
	}
	if req.Scope == "" {
		req.Scope = router.ScopeLane
	}
	if err := launch.Validate(req, nil); err != nil {
		return fmt.Errorf("%w: %v", ErrDaemonPrep, err)
	}
	if opts.Herdr == nil {
		return fmt.Errorf("%w: Herdr launcher is required", ErrDaemonPrep)
	}
	if !opts.Herdr.Available() {
		return fmt.Errorf("%w: Herdr is unavailable", ErrDaemonPrep)
	}
	createWT := true
	if opts.CreateWorktree != nil {
		createWT = *opts.CreateWorktree
	}
	if createWT && opts.Worktree == nil {
		// Callers may rely on Engine.Worktree; checked at launch time.
	}
	return nil
}

// resolveWorktree returns the WorktreeService for this tick. Prefer the explicit
// option; fall back to the engine's manager adapted to the dispatch interface.
func (e *Engine) resolveWorktree(opts TickOptions) dispatch.WorktreeService {
	if opts.Worktree != nil {
		return opts.Worktree
	}
	if e != nil && e.Worktree != nil {
		return engineWorktree{m: e.Worktree}
	}
	return nil
}

type engineWorktree struct{ m *worktree.WorktreeManager }

func (w engineWorktree) CreateTaskWorktreeFrom(ctx context.Context, taskRef, defaultBranch string) (*worktree.WorktreeInfo, error) {
	if w.m == nil {
		return nil, errors.New("worktree manager is nil")
	}
	return w.m.CreateTaskWorktreeFrom(ctx, taskRef, defaultBranch)
}
func (w engineWorktree) RepoRoot() string {
	if w.m == nil {
		return ""
	}
	return w.m.RepoRoot
}

func (e *Engine) launchAfterClaim(ctx context.Context, task *provider.Task, tok *deps.OwnershipToken, opts TickOptions) (*TickReceipt, error) {
	bound, err := router.RebindDecision(opts.Decision, task.Ref, tok.Generation)
	if err != nil {
		return nil, fmt.Errorf("rebind decision after claim: %w", err)
	}

	createWT := true
	if opts.CreateWorktree != nil {
		createWT = *opts.CreateWorktree
	}
	wtSvc := e.resolveWorktree(opts)

	var (
		wtPath string
		branch string
		base   string
	)
	if createWT {
		if wtSvc == nil {
			return nil, errors.New("worktree service is required")
		}
		defaultBranch := opts.DefaultBranch
		if defaultBranch == "" && e.Config != nil {
			defaultBranch = e.Config.Project.DefaultBranch
		}
		if defaultBranch == "" {
			defaultBranch = worktree.DefaultBranch
		}
		wtInfo, werr := wtSvc.CreateTaskWorktreeFrom(ctx, task.Ref, defaultBranch)
		if werr != nil {
			return nil, fmt.Errorf("create task worktree: %w", werr)
		}
		if wtInfo == nil {
			return nil, errors.New("worktree service returned nil info")
		}
		if err := worktree.RejectSharedRoot(wtSvc.RepoRoot(), wtInfo.Path); err != nil {
			return nil, err
		}
		if wtInfo.Branch == "" {
			return nil, errors.New("worktree created without a Git branch")
		}
		wtPath, branch, base = wtInfo.Path, wtInfo.Branch, wtInfo.BaseSHA
	}

	req := launch.Request{
		Decision:        bound,
		TaskRef:         task.Ref,
		LeaseGeneration: tok.Generation,
		Scope:           router.ScopeTask,
		Repository:      opts.Repository,
		Lane:            opts.Lane.Name,
		CWD:             wtPath,
	}
	if err := launch.Validate(req, nil); err != nil {
		return nil, fmt.Errorf("post-claim launch validate: %w", err)
	}

	packet := ""
	if opts.Packet != nil {
		packet = opts.Packet(task, opts.Lane, wtPath)
	} else {
		var packetErr error
		packet, packetErr = daemonPacketWithMemory(task, opts.Lane, wtPath, tok.GraphRev, opts.ScopedMemory, opts.ScopedMemoryActor, opts.ScopedMemoryRunID)
		if packetErr != nil {
			return nil, packetErr
		}
	}
	digest := packetDigest(packet)
	req.PacketDigest = digest

	agentName := fmt.Sprintf("daemon-%s", strings.ToLower(task.Ref))
	if len(agentName) > 32 {
		agentName = agentName[:32]
	}
	reused := false
	var tabID, paneID, sessionID string

	// Standing reuse only after exact identity/model/worktree readback.
	if opts.ResolveStanding != nil && strings.TrimSpace(opts.StandingName) != "" {
		standing, serr := opts.ResolveStanding(ctx, opts.StandingName, req)
		if serr != nil {
			return nil, fmt.Errorf("standing agent readback: %w", serr)
		}
		if standing != nil {
			if err := verifyStandingReuse(standing, req, wtPath); err != nil {
				return nil, err
			}
			agentName = standing.Name
			tabID, paneID, sessionID = standing.TabID, standing.PaneID, standing.Session
			if wtPath == "" {
				wtPath = standing.CWD
			}
			reused = true
		}
	}

	if !reused {
		if wtPath == "" {
			return nil, errors.New("worktree path required when no standing agent is reused")
		}
		repoRoot := ""
		if wtSvc != nil {
			repoRoot = wtSvc.RepoRoot()
		}
		ws, werr := opts.Herdr.RequireWorkspace(repoRoot)
		if werr != nil {
			return nil, fmt.Errorf("herdr workspace: %w", werr)
		}
		tab, terr := opts.Herdr.TabCreateForTask(ws, agentName, wtPath, true)
		if terr != nil {
			return nil, fmt.Errorf("tab create: %w", terr)
		}
		tabID, paneID = tab.ID, tab.Pane.ID
		if err := opts.Herdr.AgentStart(req, agentName, bound.Harness, paneID); err != nil {
			_ = opts.Herdr.TabClose(tabID)
			return nil, fmt.Errorf("agent start: %w", err)
		}
	}

	timeout := opts.PromptTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	// Wake text keeps the durable control contract: point at TASK-PACKET.md
	// and the task ref. Packet digest is bound into the receipt separately.
	wake := control.WakeTextForTask(task.Ref)
	if packet != "" && opts.Packet != nil {
		// Tests inject full packets and need them consumed as-is for digest proof.
		wake = packet
	}
	promptRec, perr := opts.Herdr.DeliverAndProve(agentName, wake, timeout)
	if perr != nil {
		if !reused && tabID != "" {
			_ = opts.Herdr.TabClose(tabID)
		}
		return nil, fmt.Errorf("prompt consumption: %w", perr)
	}
	if promptRec == nil || !promptRec.Consumed || !promptRec.Verified {
		if !reused && tabID != "" {
			_ = opts.Herdr.TabClose(tabID)
		}
		return nil, errors.New("prompt receipt did not prove consumption")
	}
	if !herdr.ConsumptionProven(promptRec.BaselineStatus, promptRec.FinalStatus) {
		if !reused && tabID != "" {
			_ = opts.Herdr.TabClose(tabID)
		}
		return nil, fmt.Errorf("prompt sequence %q is not a valid consumption proof", promptRec.SequenceToken)
	}

	// Still own the generation after launch proof.
	own, oerr := e.ownershipClaimer()
	if oerr != nil {
		return nil, fmt.Errorf("ownership after launch: %w", oerr)
	}
	owns, oerr := own.StillOwns(ctx, tok)
	if oerr != nil {
		return nil, oerr
	}
	if !owns {
		return nil, fmt.Errorf("%w: lost lease after launch proof", deps.ErrNotOwner)
	}

	lcState := lifecycle.StateDispatched
	if opts.Lifecycle != nil {
		if err := projectDaemonLifecycle(opts.Lifecycle, task.Ref, opts.Repository, tok, digest, branch, base); err != nil {
			return nil, fmt.Errorf("lifecycle projection: %w", err)
		}
	}

	return &TickReceipt{
		TaskRef:          task.Ref,
		TaskID:           task.ID,
		LeaseGeneration:  tok.Generation,
		GraphRevision:    tok.GraphRev,
		ProviderRevision: tok.ProviderRev,
		Repository:       opts.Repository,
		Lane:             opts.Lane.Name,
		Worktree:         wtPath,
		Branch:           branch,
		TabID:            tabID,
		PaneID:           paneID,
		SessionID:        sessionID,
		AgentName:        agentName,
		Model:            bound.Model,
		Effort:           bound.Effort,
		Harness:          bound.Harness,
		Argv:             append([]string(nil), bound.Argv...),
		PacketDigest:     digest,
		PromptSequence:   promptRec.SequenceToken,
		Launched:         true,
		ReusedStanding:   reused,
		LifecycleState:   lcState,
		OwnershipOwnerID: tok.OwnerID,
	}, nil
}

func verifyStandingReuse(standing *StandingAgent, req launch.Request, expectedCWD string) error {
	if standing == nil {
		return errors.New("standing agent is nil")
	}
	if strings.TrimSpace(standing.Name) == "" || strings.TrimSpace(standing.TabID) == "" || strings.TrimSpace(standing.PaneID) == "" {
		return fmt.Errorf("standing agent identity incomplete: name/tab/pane required")
	}
	if req.Decision != nil {
		if standing.Model != "" && !strings.EqualFold(standing.Model, req.Decision.Model) {
			return fmt.Errorf("standing agent model %q does not match decision model %q", standing.Model, req.Decision.Model)
		}
		if standing.Harness != "" && !strings.EqualFold(standing.Harness, req.Decision.Harness) {
			return fmt.Errorf("standing agent harness %q does not match decision harness %q", standing.Harness, req.Decision.Harness)
		}
	}
	if expectedCWD != "" && standing.CWD != "" && standing.CWD != expectedCWD {
		return fmt.Errorf("standing agent cwd %q does not match task worktree %q", standing.CWD, expectedCWD)
	}
	if expectedCWD == "" && standing.CWD == "" {
		return errors.New("standing agent reuse requires verified worktree cwd")
	}
	return nil
}

func projectDaemonLifecycle(m *lifecycle.Machine, taskRef, repo string, tok *deps.OwnershipToken, digest, branch, base string) error {
	// First transition for a new task may land at Eligible; then Claimed; then Dispatched.
	steps := []lifecycle.State{lifecycle.StateEligible, lifecycle.StateClaimed, lifecycle.StateDispatched}
	for i, to := range steps {
		_, err := m.Transition(lifecycle.TransitionRequest{
			TaskRef:          taskRef,
			Repo:             repo,
			To:               to,
			Actor:            "daemon",
			IdempotencyKey:   fmt.Sprintf("daemon-tick:%s:g%d:%s", taskRef, tok.Generation, to),
			LeaseGeneration:  tok.Generation,
			ProviderRevision: tok.ProviderRev,
			Branch:           branch,
			CandidateSHA:     base,
			EvidenceDigest:   digest,
			Payload:          fmt.Sprintf("step=%d", i),
		})
		if err != nil {
			// Idempotent replay or already-advanced state is acceptable when
			// concurrent recovery projected the same path.
			if errors.Is(err, lifecycle.ErrInvalidTransition) {
				// Try to continue if we're already past this step.
				cur, cerr := m.EventStore().CurrentState(taskRef)
				if cerr == nil && cur != nil && lifecycleRank(cur.State) >= lifecycleRank(to) {
					continue
				}
			}
			return err
		}
	}
	return nil
}

func lifecycleRank(s lifecycle.State) int {
	order := []lifecycle.State{
		lifecycle.StateDraft, lifecycle.StateEligible, lifecycle.StateClaimed,
		lifecycle.StateDispatched, lifecycle.StateBuilding,
	}
	for i, st := range order {
		if st == s {
			return i
		}
	}
	return -1
}

// compensateAfterClaim reverses the board to to-do and releases the generation
// fence only while owner+generation still match. On board failure the lease is
// retained (Recovering) — never release-first.
func (e *Engine) compensateAfterClaim(ctx context.Context, task *provider.Task, tok *deps.OwnershipToken, reason string) error {
	if e == nil || task == nil || tok == nil {
		return fmt.Errorf("daemon compensate: missing engine/task/token")
	}
	own, err := e.ownershipClaimer()
	if err != nil {
		return fmt.Errorf("daemon compensate ownership: %w", err)
	}
	owns, err := own.StillOwns(ctx, tok)
	if err != nil {
		return err
	}
	if !owns {
		return fmt.Errorf("%w: refuse board compensate (%s)", deps.ErrNotOwner, reason)
	}
	if e.TaskProv != nil {
		if boardErr := e.TaskProv.UpdateStatus(ctx, task.ID, provider.StatusToDo); boardErr != nil {
			// Retain lease — Recovering. Do not open an acquire window.
			return fmt.Errorf("board compensate retained lease (Recovering): %w", boardErr)
		}
	}
	if rErr := own.ReleaseIfOwner(ctx, tok, reason); rErr != nil && !errors.Is(rErr, deps.ErrNotOwner) {
		return rErr
	}
	e.clearClaimToken()
	return nil
}

func defaultDaemonPacket(task *provider.Task, lane *config.LaneDef, worktreePath string) string {
	laneName := ""
	if lane != nil {
		laneName = lane.Name
	}
	return fmt.Sprintf("Task [%s]: %s\nWorktree: %s\nLane: %s\nRead TASK-PACKET.md and execute the workflow.\n",
		task.Ref, task.Title, worktreePath, laneName)
}

// daemonPacketWithMemory is the sole default packet construction path. It is
// intentionally optional to preserve legacy daemon callers, and uses the
// post-claim task/ref revision rather than untrusted caller-selected values.
// A stale memory store is excluded (not promoted into a packet); other store
// failures fail closed before a worker receives a partially-authorized packet.
func daemonPacketWithMemory(task *provider.Task, lane *config.LaneDef, worktreePath, revision string, store *memory.ScopedMemoryStore, actor memory.Actor, runID string) (string, error) {
	packet := defaultDaemonPacket(task, lane, worktreePath)
	if store == nil {
		return packet, nil
	}
	role := ""
	if lane != nil {
		role = lane.Name
	}
	entries, err := store.Inject(memory.ReadRequest{Actor: actor, RunID: runID, TaskID: task.Ref, Role: role, Revision: revision, ReadAt: time.Now().UTC()})
	if errors.Is(err, memory.ErrStaleRevision) {
		return packet + "Scoped memory excluded: stale revision.\n", nil
	}
	if err != nil {
		return "", fmt.Errorf("daemon: inject scoped memory: %w", err)
	}
	if len(entries) == 0 {
		return packet, nil
	}
	var b strings.Builder
	b.WriteString(packet)
	b.WriteString("Authorized scoped memory:\n")
	for _, entry := range entries {
		fmt.Fprintf(&b, "- [%s] %s\n", entry.ID, entry.Content)
	}
	return b.String(), nil
}

func packetDigest(packet string) string {
	sum := sha256.Sum256([]byte(packet))
	return "sha256:" + hex.EncodeToString(sum[:])
}
