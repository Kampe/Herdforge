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
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

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
}

// HerdrLauncher isolates herdr operations for crash-point tests (FAC-121).
// DeliverAndProve always requires consumption proof (no verify bypass).
type HerdrLauncher interface {
	Available() bool
	RequireWorkspace(repoRoot string) (string, error)
	TabCreateForTask(workspaceID, label, cwd string, noFocus bool) (*herdr.TabInfo, error)
	AgentStart(name, kind, paneID string, agentArgs ...string) error
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
func (LiveHerdr) AgentStart(name, kind, paneID string, agentArgs ...string) error {
	return herdr.AgentStart(name, kind, paneID, agentArgs...)
}
func (LiveHerdr) DeliverAndProve(target, text string, timeout time.Duration) (*herdr.PromptReceipt, error) {
	return herdr.DeliverAndProve(target, text, timeout)
}
func (LiveHerdr) TabClose(tabID string) error { return herdr.TabClose(tabID) }
func (LiveHerdr) ResolveHealthyModel(ctx context.Context, primary string, fallbacks []string) (string, []herdr.ProbeResult) {
	return herdr.ResolveHealthyModel(ctx, primary, fallbacks)
}

type DispatchOptions struct {
	TicketRef string
	Provider  string
	Model     string
	Role      string
	NoLaunch  bool
	TaskShape string
	LaneName  string
	// PromptVerifyTimeout bounds DeliverAndProve polling (default 60s).
	// Production launches always require consumption proof — there is no
	// SkipPromptVerify bypass (FAC-121 R3 repair).
	PromptVerifyTimeout time.Duration
}

type DispatchResult struct {
	TicketRef   string
	TicketTitle string
	Worktree    string
	Branch      string
	BaseSHA     string
	AnchorRef   string
	TaskPacket  string
	Launched    bool
	Lane        string
	Model       string // the model that actually launched (after fallback)
	TabID       string
	AgentName   string
	Receipt     *herdr.PromptReceipt
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
	// Herdr is optional; defaults to LiveHerdr.
	Herdr HerdrLauncher
	// PromptVerifyTimeout overrides the delivery poll window.
	PromptVerifyTimeout time.Duration
	// Deps is the relation store for the FAC-159 pre-side-effect gate.
	// When nil, constructed from TaskProvider via deps.StoreFor.
	Deps deps.RelationStore
	// SkipDepsGate is test-only escape for unit tests that intentionally
	// exercise later steps without a relation-capable provider. Production
	// paths must leave this false.
	SkipDepsGate bool

	// health projects BLOCKED(provider_timeout) for board calls (FAC-150).
	health dispatchHealth
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

// failWithCompensate runs compensation and joins any compensate error with primary.
// Primary is always preserved; compensation errors never replace it.
func (d *Dispatcher) failWithCompensate(ctx context.Context, ticketRef, reason string, primary error) error {
	if cErr := d.compensate(ctx, ticketRef, reason); cErr != nil {
		return errors.Join(primary, cErr)
	}
	return primary
}

// rollbackTab closes a partial-launch tab then runs durable compensation.
// TabClose errors are never discarded: they are joined into primary and
// additionally compensated under reason+"_orphan_tab_close_failed" so an
// orphan session cannot be silently accepted (FAC-121 R3).
func (d *Dispatcher) rollbackTab(ctx context.Context, h HerdrLauncher, tabID, ticketRef, reason string, primary error) error {
	if tabID != "" && h != nil {
		if closeErr := h.TabClose(tabID); closeErr != nil {
			primary = errors.Join(primary, fmt.Errorf("tab close %q during %s: %w", tabID, reason, closeErr))
			if cErr := d.compensate(ctx, ticketRef, reason+"_orphan_tab_close_failed"); cErr != nil {
				primary = errors.Join(primary, cErr)
			}
		}
	}
	return d.failWithCompensate(ctx, ticketRef, reason, primary)
}

func (d *Dispatcher) Dispatch(ctx context.Context, opts DispatchOptions) (*DispatchResult, error) {
	// Fail closed before any side effect when durable hooks are missing.
	if err := d.requireCompensator(); err != nil {
		return nil, err
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

	// 2b. FAC-159 PRE-SIDE-EFFECT gate: packet↔board dependency conformance.
	// Must run BEFORE worktree create, status flip, comment, or tab.
	var depProv *deps.Provenance
	if !d.SkipDepsGate {
		store := d.Deps
		if store == nil {
			store = deps.StoreFor(d.TaskProvider, d.Config.TaskProvider.ProjectID)
		}
		desired, perr := deps.ExtractProvenanceFromText(task.Description)
		if perr != nil {
			return nil, fmt.Errorf("dispatch dependency provenance: %w", perr)
		}
		// Empty/missing provenance is never OK — require versioned record.
		if desired == nil || !desired.Present {
			return nil, fmt.Errorf("dispatch: %w for %s (attach herd-deps-v1 fence)", deps.ErrMissingProvenance, task.Ref)
		}
		gate, gerr := deps.ValidateLaunch(ctx, store, deps.EntryDispatch, deps.Ref(task.Ref), desired, "")
		if gerr != nil {
			return nil, gerr
		}
		if gate != nil {
			depProv = gate.Provenance
		}
	}

	// 3. Create worktree from immutable origin/<defaultBranch> (FAC-121).
	// Branch name is whatever Git actually created — never overwrite with a
	// fictional task/<slug> packet alias.
	if d.Worktree == nil {
		return nil, fmt.Errorf("dispatch worktree service is required")
	}
	wtInfo, err := d.Worktree.CreateTaskWorktreeFrom(ctx, task.Ref, defaultBranch)
	if err != nil {
		return nil, fmt.Errorf("failed to create worktree: %w", err)
	}
	if err := worktree.RejectSharedRoot(d.Worktree.RepoRoot(), wtInfo.Path); err != nil {
		return nil, d.failWithCompensate(ctx, task.Ref, "shared_root_denied", err)
	}
	// Worktree side effect already landed (path/branch may exist on disk even
	// when the reported branch string is empty). Empty branch is a hard failure
	// and must compensate — never return bare after CreateTaskWorktreeFrom.
	branch := wtInfo.Branch
	if branch == "" {
		return nil, d.failWithCompensate(ctx, task.Ref, "empty_worktree_branch",
			fmt.Errorf("worktree created without a Git branch; refusing fictional packet branch"))
	}

	// Worktree side effect already landed — every subsequent error must compensate.
	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepWorktree,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		BaseSHA:   wtInfo.BaseSHA,
		AnchorRef: wtInfo.AnchorRef,
	}); err != nil {
		return nil, d.failWithCompensate(ctx, task.Ref, "record_worktree_failed", err)
	}

	// 4. Flip ticket to in-progress (partial: compensator marks Recovering on later failure)
	if err := d.updateStatusBound(ctx, task.ID, "in-progress"); err != nil {
		return nil, d.failWithCompensate(ctx, task.Ref, "board_status_failed",
			formatBoardErr("failed to update ticket status", err))
	}
	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepBoardProgress,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		BaseSHA:   wtInfo.BaseSHA,
		AnchorRef: wtInfo.AnchorRef,
	}); err != nil {
		return nil, d.failWithCompensate(ctx, task.Ref, "record_board_progress_failed", err)
	}

	// 5. Comment with actual Git branch + base (not a fictional name)
	comment := fmt.Sprintf("Dispatched to worktree %s on branch %s (base %s anchor %s)",
		wtInfo.Path, branch, wtInfo.BaseSHA, wtInfo.AnchorRef)
	if err := d.addCommentBound(ctx, task.ID, comment); err != nil {
		return nil, d.failWithCompensate(ctx, task.Ref, "board_comment_failed",
			formatBoardErr("failed to add comment", err))
	}
	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepBoardComment,
		Worktree:  wtInfo.Path,
		Branch:    branch,
	}); err != nil {
		return nil, d.failWithCompensate(ctx, task.Ref, "record_board_comment_failed", err)
	}

	// 6. Preflight in worktree
	if err := preflight.CheckWorktreeBoundary(wtInfo.Path); err != nil {
		return nil, d.failWithCompensate(ctx, task.Ref, "preflight_failed",
			fmt.Errorf("preflight failed in worktree: %w", err))
	}

	// 7. Write TASK-PACKET.md — packet branch MUST equal Git branch
	// Fail closed rather than silently falling back to a hardcoded `go test`
	// (FAC-134): every repo must declare its own verification contract.
	if d.Config.Verification.TestCommand == "" {
		return nil, d.failWithCompensate(ctx, task.Ref, "verification_test_command_missing",
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
		return nil, d.failWithCompensate(ctx, task.Ref, "task_packet_write_failed",
			fmt.Errorf("failed to write task packet: %w", err))
	}

	result := &DispatchResult{
		TicketRef:   task.Ref,
		TicketTitle: task.Title,
		Worktree:    wtInfo.Path,
		Branch:      branch,
		BaseSHA:     wtInfo.BaseSHA,
		AnchorRef:   wtInfo.AnchorRef,
		TaskPacket:  packetPath,
		Lane:        lane.Name,
	}

	// 8. Optionally launch agent with explicit cwd + proven prompt consumption
	h := d.launcher()
	if !opts.NoLaunch && h.Available() {
		if err := d.launch(ctx, opts, task, lane, wtInfo, branch, packet, result); err != nil {
			return result, err
		}
	}

	return result, nil
}

func (d *Dispatcher) launch(
	ctx context.Context,
	opts DispatchOptions,
	task *provider.Task,
	lane *config.LaneDef,
	wtInfo *worktree.WorktreeInfo,
	branch, packet string,
	result *DispatchResult,
) error {
	if err := d.requireCompensator(); err != nil {
		return err
	}

	h := d.launcher()
	tabLabel := fmt.Sprintf("task-%s", strings.ToLower(task.Ref))
	if len(tabLabel) > 32 {
		tabLabel = tabLabel[:32]
	}

	// Shared-root denial before any write-capable agent starts (production guard).
	if err := worktree.RejectSharedRoot(d.Worktree.RepoRoot(), wtInfo.Path); err != nil {
		return d.failWithCompensate(ctx, task.Ref, "shared_root_denied", err)
	}

	model, trail := h.ResolveHealthyModel(ctx, lane.Model, lane.FallbackModels)
	if model == "" {
		var b strings.Builder
		for _, p := range trail {
			fmt.Fprintf(&b, "\n  %s: %s", p.Model, p.Reason)
		}
		return d.failWithCompensate(ctx, task.Ref, "no_healthy_model",
			fmt.Errorf("no healthy model for lane %q — every candidate is exhausted:%s", lane.Name, b.String()))
	}
	result.Model = model

	// Explicit workspace — never hardcoded "wF".
	ws, err := h.RequireWorkspace(d.Worktree.RepoRoot())
	if err != nil {
		return d.failWithCompensate(ctx, task.Ref, "workspace_unknown",
			fmt.Errorf("worktree ready but herdr workspace unresolved: %w", err))
	}

	// Tab with exact task worktree as process cwd.
	tab, err := h.TabCreateForTask(ws, tabLabel, wtInfo.Path, true)
	if err != nil {
		return d.failWithCompensate(ctx, task.Ref, "tab_create_failed",
			fmt.Errorf("worktree ready but failed to launch agent: %w", err))
	}
	result.TabID = tab.ID
	result.AgentName = tabLabel
	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepTab,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tab.ID,
		PaneID:    tab.Pane.ID,
		AgentName: tabLabel,
	}); err != nil {
		return d.rollbackTab(ctx, h, tab.ID, task.Ref, "record_tab_failed", err)
	}

	if err := h.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID, herdr.LaneAgentArgs(model)...); err != nil {
		// Close orphan tab then durable compensate — TabClose errors must not be silent.
		return d.rollbackTab(ctx, h, tab.ID, task.Ref, "agent_start_failed",
			fmt.Errorf("worktree ready but agent start failed: %w", err))
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
		return d.rollbackTab(ctx, h, tab.ID, task.Ref, "record_agent_start_failed", err)
	}

	// Always prove consumption — no production SkipPromptVerify bypass.
	timeout := opts.PromptVerifyTimeout
	if timeout == 0 {
		timeout = d.PromptVerifyTimeout
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	receipt, err := h.DeliverAndProve(tabLabel, packet, timeout)
	result.Receipt = receipt
	if err != nil {
		return d.rollbackTab(ctx, h, tab.ID, task.Ref, "prompt_delivery_failed",
			fmt.Errorf("worktree ready but prompt consumption not proven: %w", err))
	}
	if receipt == nil || !receipt.Consumed || !receipt.Verified {
		return d.rollbackTab(ctx, h, tab.ID, task.Ref, "prompt_receipt_invalid",
			fmt.Errorf("worktree ready but prompt receipt did not prove consumption"))
	}
	if !herdr.ConsumptionProven(receipt.BaselineStatus, receipt.FinalStatus) {
		return d.rollbackTab(ctx, h, tab.ID, task.Ref, "prompt_sequence_invalid",
			fmt.Errorf("prompt receipt sequence %q is not a valid consumption proof", receipt.SequenceToken))
	}

	if err := d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepPrompt,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tab.ID,
		AgentName: tabLabel,
		Receipt:   receipt.SequenceToken,
	}); err != nil {
		return d.rollbackTab(ctx, h, tab.ID, task.Ref, "record_prompt_failed", err)
	}
	result.Launched = true
	return nil
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
