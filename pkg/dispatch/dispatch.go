package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type DispatchOptions struct {
	TicketRef string
	Provider  string
	Model     string
	Role      string
	NoLaunch  bool
	TaskShape string
	LaneName  string
}

type DispatchResult struct {
	TicketRef   string
	TicketTitle string
	Worktree    string
	Branch      string
	TaskPacket  string
	Launched    bool
	Lane        string
	Model       string // the model that actually launched (after fallback)
}

type Dispatcher struct {
	Config       *config.Config
	TaskProvider provider.TaskProvider
	Worktree     *worktree.WorktreeManager
}

func NewDispatcher(cfg *config.Config, tp provider.TaskProvider, wm *worktree.WorktreeManager) *Dispatcher {
	return &Dispatcher{
		Config:       cfg,
		TaskProvider: tp,
		Worktree:     wm,
	}
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

	// 3. Create worktree from origin/main
	wtInfo, err := d.Worktree.CreateTaskWorktree(ctx, task.Ref)
	if err != nil {
		return nil, fmt.Errorf("failed to create worktree: %w", err)
	}

	slug := slugForTask(task.Ref, task.Title)
	branch := fmt.Sprintf("task/%s", slug)
	wtInfo.Branch = branch

	// 5. Flip ticket to in-progress
	if err := d.TaskProvider.UpdateStatus(ctx, task.ID, "in-progress"); err != nil {
		return nil, fmt.Errorf("failed to update ticket status: %w", err)
	}

	// 6. Add commenting note about worktree
	comment := fmt.Sprintf("Dispatched to worktree %s on branch %s", wtInfo.Path, branch)
	if err := d.TaskProvider.AddComment(ctx, task.ID, comment); err != nil {
		return nil, fmt.Errorf("failed to add comment: %w", err)
	}

	// 7. Run preflight in worktree
	if err := preflight.CheckWorktreeBoundary(wtInfo.Path); err != nil {
		return nil, fmt.Errorf("preflight failed in worktree: %w", err)
	}

	// 8. Write TASK-PACKET.md
	rolePath := lane.Prompt
	if rolePath == "" {
		rolePath = ".herd/prompts/worker.md"
	}

	packet := buildTaskPacket(task, wtInfo.Path, branch, rolePath, lane)
	packetPath := filepath.Join(wtInfo.Path, "TASK-PACKET.md")
	if err := os.WriteFile(packetPath, []byte(packet), 0644); err != nil {
		return nil, fmt.Errorf("failed to write task packet: %w", err)
	}

	result := &DispatchResult{
		TicketRef:   task.Ref,
		TicketTitle: task.Title,
		Worktree:    wtInfo.Path,
		Branch:      branch,
		TaskPacket:  packetPath,
		Lane:        lane.Name,
	}

	// 9. Optionally launch agent
	if !opts.NoLaunch && herdr.IsAvailable() {
		tabLabel := fmt.Sprintf("task-%s", strings.ToLower(task.Ref))
		if len(tabLabel) > 32 {
			tabLabel = tabLabel[:32]
		}
		// Probe the lane model (and fall over to fallback_models) BEFORE
		// spawning: a quota-exhausted surface launches an agent that produces
		// plans, not code, and burns the dispatch silently. Catch it here.
		model, trail := herdr.ResolveHealthyModel(ctx, lane.Model, lane.FallbackModels)
		if model == "" {
			var b strings.Builder
			for _, p := range trail {
				fmt.Fprintf(&b, "\n  %s: %s", p.Model, p.Reason)
			}
			return result, fmt.Errorf("no healthy model for lane %q — every candidate is exhausted:%s", lane.Name, b.String())
		}
		result.Model = model

		tab, err := herdr.Tab(herdr.ResolveWorkspace("."), tabLabel, true)
		if err != nil {
			return result, fmt.Errorf("worktree ready but failed to launch agent: %w", err)
		}

		if err := herdr.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID, herdr.LaneAgentArgs(model)...); err != nil {
			return result, fmt.Errorf("worktree ready but agent start failed: %w", err)
		}

		// Deliver task packet as initial prompt
		herdr.AgentPrompt(tabLabel, packet, false)
		result.Launched = true
	}

	return result, nil
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
func buildTaskPacket(task *provider.Task, wtPath, branch, rolePath string, lane *config.LaneDef) string {
	var b strings.Builder

	fmt.Fprintf(&b, "BUILD %s — EXECUTE. No menus, no questions. Do not stop until "+
		"`go build ./...`, `go vet ./...`, and `go test ./...` all pass AND you have committed.\n\n", task.Ref)

	fmt.Fprintf(&b, "Worktree: %s (branch %s). Work ONLY here — never edit files outside it.\n\n", wtPath, branch)

	fmt.Fprintf(&b, "Read the full spec yourself (do not wait for it inline):\n")
	fmt.Fprintf(&b, "  kaneo task get %s --full\n", task.Ref)
	b.WriteString("  and the matching chainseer source at ~/Personal/chainseer/bin/ if this is a port.\n\n")

	b.WriteString("Completion contract (self-gate, FAC-116):\n")
	fmt.Fprintf(&b, "  1. cd %s\n", wtPath)
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
