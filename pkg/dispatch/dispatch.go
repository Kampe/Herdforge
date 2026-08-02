package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		tab, err := herdr.Tab("wF", tabLabel, true)
		if err != nil {
			return result, fmt.Errorf("worktree ready but failed to launch agent: %w", err)
		}

		if err := herdr.AgentStart(tabLabel, lane.AgentKind, tab.Pane.ID); err != nil {
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

func buildTaskPacket(task *provider.Task, wtPath, branch, rolePath string, lane *config.LaneDef) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("# Task Packet: %s\n\n", task.Ref))
	b.WriteString(fmt.Sprintf("**Title**: %s\n", task.Title))
	b.WriteString(fmt.Sprintf("**Priority**: %s\n", task.Priority))
	b.WriteString(fmt.Sprintf("**Status**: %s\n", task.Status))
	b.WriteString(fmt.Sprintf("**Labels**: %s\n\n", strings.Join(task.Labels, ", ")))

	b.WriteString("## Worktree\n\n")
	b.WriteString(fmt.Sprintf("**Path**: `%s`\n", wtPath))
	b.WriteString(fmt.Sprintf("**Branch**: `%s`\n", branch))

	if lane != nil {
		b.WriteString(fmt.Sprintf("**Role**: %s\n", lane.Role))
		b.WriteString(fmt.Sprintf("**Agent**: %s / %s\n", lane.AgentKind, lane.Model))
		if lane.Worktree != "" {
			b.WriteString(fmt.Sprintf("**Assigned Worktree**: %s\n", lane.Worktree))
		}
	}

	b.WriteString("\n## Description\n\n")
	b.WriteString(task.Description)
	b.WriteString("\n\n")

	b.WriteString("## Workflow\n\n")
	b.WriteString(fmt.Sprintf("1. Enter worktree: `cd %s`\n", wtPath))
	b.WriteString("2. Inspect existing code and understand what needs to change\n")
	b.WriteString("3. Write failing tests first (TDD)\n")
	b.WriteString("4. Implement the minimal solution\n")
	b.WriteString("5. Verify: `go test ./...` (or equivalent test command)\n")
	b.WriteString("6. Commit with a conventional commit message\n")
	b.WriteString("7. Signal completion by moving the card to 'in-progress' (review pipeline)\n\n")

	if lane != nil {
		b.WriteString(fmt.Sprintf("## Role Context\n\nRole prompt from: `%s`\n", rolePath))
	}

	return b.String()
}
