package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// LaunchStep names a partial side-effect during dispatch (FAC-121).
// FAC-119's transactional outbox should persist these for crash recovery.
type LaunchStep string

const (
	StepWorktree      LaunchStep = "worktree"
	StepBoardProgress LaunchStep = "board_in_progress"
	StepBoardComment  LaunchStep = "board_comment"
	StepTab           LaunchStep = "tab"
	StepAgentStart    LaunchStep = "agent_start"
	StepPrompt        LaunchStep = "prompt"
)

// LaunchRecord is the narrow compensation interface expected from FAC-119
// (lifecycle outbox) and FAC-120 (lease fencing). Dispatch does not implement
// the durable store; it invokes these hooks so partial launches can be
// reconciled without stranded tabs or false in-progress cards.
type Compensator interface {
	// RecordStep persists a successful side-effect (idempotent by step key).
	RecordStep(ctx context.Context, rec StepRecord) error
	// Compensate undoes or marks Recovering for partial launch state.
	// reason should be stable for outbox replay.
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
type HerdrLauncher interface {
	Available() bool
	RequireWorkspace(repoRoot string) (string, error)
	TabCreateForTask(workspaceID, label, cwd string, noFocus bool) (*herdr.TabInfo, error)
	AgentStart(name, kind, paneID string, agentArgs ...string) error
	DeliverAndProve(target, text string, verify bool, timeout time.Duration) (*herdr.PromptReceipt, error)
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
func (LiveHerdr) DeliverAndProve(target, text string, verify bool, timeout time.Duration) (*herdr.PromptReceipt, error) {
	return herdr.DeliverAndProve(target, text, verify, timeout)
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
	// VerifyPrompt when launching: poll for consumption receipt (default true).
	// Tests may set PromptVerifyTimeout.
	SkipPromptVerify    bool
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

type Dispatcher struct {
	Config       *config.Config
	TaskProvider provider.TaskProvider
	Worktree     *worktree.WorktreeManager
	// Compensator is optional; when set, launch steps and failures are recorded
	// for FAC-119 outbox recovery. Nil means best-effort local compensation only.
	Compensator Compensator
	// Herdr is optional; defaults to LiveHerdr.
	Herdr HerdrLauncher
	// PromptVerifyTimeout overrides the delivery poll window.
	PromptVerifyTimeout time.Duration
}

func NewDispatcher(cfg *config.Config, tp provider.TaskProvider, wm *worktree.WorktreeManager) *Dispatcher {
	return &Dispatcher{
		Config:       cfg,
		TaskProvider: tp,
		Worktree:     wm,
		Herdr:        LiveHerdr{},
	}
}

func (d *Dispatcher) launcher() HerdrLauncher {
	if d.Herdr != nil {
		return d.Herdr
	}
	return LiveHerdr{}
}

func (d *Dispatcher) record(ctx context.Context, rec StepRecord) {
	if d.Compensator == nil {
		return
	}
	_ = d.Compensator.RecordStep(ctx, rec)
}

func (d *Dispatcher) compensate(ctx context.Context, ticketRef, reason string) {
	if d.Compensator == nil {
		return
	}
	_ = d.Compensator.Compensate(ctx, ticketRef, reason)
}

func (d *Dispatcher) Dispatch(ctx context.Context, opts DispatchOptions) (*DispatchResult, error) {
	// 1. Fetch ticket from Kaneo
	tasks, err := d.TaskProvider.ListTasks(ctx, d.Config.TaskProvider.ProjectID, "")
	if err != nil {
		return nil, fmt.Errorf("failed to list tasks: %w", err)
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

	// 2. Determine lane
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

	// 3. Create worktree from immutable origin/<defaultBranch> (FAC-121).
	// Branch name is whatever Git actually created — never overwrite with a
	// fictional task/<slug> packet alias.
	wtInfo, err := d.Worktree.CreateTaskWorktreeFrom(ctx, task.Ref, defaultBranch)
	if err != nil {
		return nil, fmt.Errorf("failed to create worktree: %w", err)
	}
	if err := worktree.RejectSharedRoot(d.Worktree.RepoRoot, wtInfo.Path); err != nil {
		return nil, err
	}
	branch := wtInfo.Branch
	if branch == "" {
		return nil, fmt.Errorf("worktree created without a Git branch; refusing fictional packet branch")
	}

	d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepWorktree,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		BaseSHA:   wtInfo.BaseSHA,
		AnchorRef: wtInfo.AnchorRef,
	})

	// 4. Flip ticket to in-progress (partial: compensator marks Recovering on later failure)
	if err := d.TaskProvider.UpdateStatus(ctx, task.ID, "in-progress"); err != nil {
		return nil, fmt.Errorf("failed to update ticket status: %w", err)
	}
	d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepBoardProgress,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		BaseSHA:   wtInfo.BaseSHA,
		AnchorRef: wtInfo.AnchorRef,
	})

	// 5. Comment with actual Git branch + base (not a fictional name)
	comment := fmt.Sprintf("Dispatched to worktree %s on branch %s (base %s anchor %s)",
		wtInfo.Path, branch, wtInfo.BaseSHA, wtInfo.AnchorRef)
	if err := d.TaskProvider.AddComment(ctx, task.ID, comment); err != nil {
		d.compensate(ctx, task.Ref, "board_comment_failed")
		return nil, fmt.Errorf("failed to add comment: %w", err)
	}
	d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepBoardComment,
		Worktree:  wtInfo.Path,
		Branch:    branch,
	})

	// 6. Preflight in worktree
	if err := preflight.CheckWorktreeBoundary(wtInfo.Path); err != nil {
		d.compensate(ctx, task.Ref, "preflight_failed")
		return nil, fmt.Errorf("preflight failed in worktree: %w", err)
	}

	// 7. Write TASK-PACKET.md — packet branch MUST equal Git branch
	rolePath := lane.Prompt
	if rolePath == "" {
		rolePath = ".herd/prompts/worker.md"
	}

	packet := buildTaskPacket(task, wtInfo.Path, branch, rolePath, lane)
	packetPath := filepath.Join(wtInfo.Path, "TASK-PACKET.md")
	if err := os.WriteFile(packetPath, []byte(packet), 0644); err != nil {
		d.compensate(ctx, task.Ref, "task_packet_write_failed")
		return nil, fmt.Errorf("failed to write task packet: %w", err)
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
	h := d.launcher()
	tabLabel := fmt.Sprintf("task-%s", strings.ToLower(task.Ref))
	if len(tabLabel) > 32 {
		tabLabel = tabLabel[:32]
	}

	// Shared-root denial before any write-capable agent starts.
	if err := worktree.RejectSharedRoot(d.Worktree.RepoRoot, wtInfo.Path); err != nil {
		d.compensate(ctx, task.Ref, "shared_root_denied")
		return err
	}

	model, trail := h.ResolveHealthyModel(ctx, lane.Model, lane.FallbackModels)
	if model == "" {
		var b strings.Builder
		for _, p := range trail {
			fmt.Fprintf(&b, "\n  %s: %s", p.Model, p.Reason)
		}
		d.compensate(ctx, task.Ref, "no_healthy_model")
		return fmt.Errorf("no healthy model for lane %q — every candidate is exhausted:%s", lane.Name, b.String())
	}
	result.Model = model

	// Explicit workspace — never hardcoded "wF".
	ws, err := h.RequireWorkspace(d.Worktree.RepoRoot)
	if err != nil {
		d.compensate(ctx, task.Ref, "workspace_unknown")
		return fmt.Errorf("worktree ready but herdr workspace unresolved: %w", err)
	}

	// Tab with exact task worktree as process cwd.
	tab, err := h.TabCreateForTask(ws, tabLabel, wtInfo.Path, true)
	if err != nil {
		d.compensate(ctx, task.Ref, "tab_create_failed")
		return fmt.Errorf("worktree ready but failed to launch agent: %w", err)
	}
	result.TabID = tab.ID
	result.AgentName = tabLabel
	d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepTab,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tab.ID,
		PaneID:    tab.Pane.ID,
		AgentName: tabLabel,
	})

	if err := h.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID, herdr.LaneAgentArgs(model)...); err != nil {
		// Compensate: close orphan tab so failed start leaves no session.
		_ = h.TabClose(tab.ID)
		d.compensate(ctx, task.Ref, "agent_start_failed")
		return fmt.Errorf("worktree ready but agent start failed: %w", err)
	}
	d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepAgentStart,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tab.ID,
		PaneID:    tab.Pane.ID,
		AgentName: tabLabel,
	})

	verify := !opts.SkipPromptVerify
	timeout := opts.PromptVerifyTimeout
	if timeout == 0 {
		timeout = d.PromptVerifyTimeout
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	receipt, err := h.DeliverAndProve(tabLabel, packet, verify, timeout)
	result.Receipt = receipt
	if err != nil {
		_ = h.TabClose(tab.ID)
		d.compensate(ctx, task.Ref, "prompt_delivery_failed")
		return fmt.Errorf("worktree ready but prompt consumption not proven: %w", err)
	}
	seq := ""
	if receipt != nil {
		seq = receipt.SequenceToken
	}
	d.record(ctx, StepRecord{
		TicketRef: task.Ref,
		Step:      StepPrompt,
		Worktree:  wtInfo.Path,
		Branch:    branch,
		TabID:     tab.ID,
		AgentName: tabLabel,
		Receipt:   seq,
	})
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
func buildTaskPacket(task *provider.Task, wtPath, branch, rolePath string, lane *config.LaneDef) string {
	var b strings.Builder

	fmt.Fprintf(&b, "BUILD %s — EXECUTE. No menus, no questions. Do not stop until "+
		"`go build ./...`, `go vet ./...`, and `go test ./...` all pass AND you have committed.\n\n", task.Ref)

	fmt.Fprintf(&b, "Worktree: %s (branch %s). Work ONLY here — never edit files outside it.\n\n", wtPath, branch)

	fmt.Fprintf(&b, "Read the full spec yourself (do not wait for it inline):\n")
	fmt.Fprintf(&b, "  kaneo task get %s --full\n", task.Ref)
	b.WriteString("  and the matching chainseer source at ~/Personal/chainseer/bin/ if this is a port.\n\n")

	b.WriteString("Completion contract (self-gate, FAC-116):\n")
	fmt.Fprintf(&b, "  1. You are already in %s (Herdr cwd-enforced).\n", wtPath)
	b.WriteString("  2. Implement per the spec you just read (real code + table tests).\n")
	b.WriteString("  3. go build ./... && go vet ./... && go test ./... — ALL green.\n")
	fmt.Fprintf(&b, "  4. Verify yourself: herd verify %s (must PASS: real commits + build + tests).\n", wtPath)
	fmt.Fprintf(&b, "  5. git add -A && git commit -m \"<msg containing %s>\" (no AI-attribution trailers).\n", task.Ref)
	fmt.Fprintf(&b, "  6. Final message: `BUILD COMPLETE %s` + `git rev-parse HEAD`.\n\n", task.Ref)

	b.WriteString("Do NOT push, PR, or merge — the coordinator harvests your branch. Do NOT touch the root checkout.\n")
	if lane != nil && rolePath != "" {
		fmt.Fprintf(&b, "Role contract: %s\n", rolePath)
	}
	return b.String()
}
