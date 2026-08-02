package daemon

import (
	"context"
	"fmt"
	"sort"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/verifier"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type Engine struct {
	Config   *config.Config
	TaskProv provider.TaskProvider
	Router   *router.ModelRouter
	Worktree *worktree.WorktreeManager
	Verifier *verifier.Verifier
}

func NewEngine(cfg *config.Config, tp provider.TaskProvider, r *router.ModelRouter, wm *worktree.WorktreeManager, v *verifier.Verifier) *Engine {
	return &Engine{
		Config:   cfg,
		TaskProv: tp,
		Router:   r,
		Worktree: wm,
		Verifier: v,
	}
}

// SelectNextTask sorts candidate tasks deterministically by Priority DESC, Ticket Ref ASC
func (e *Engine) SelectNextTask(ctx context.Context, role string) (*provider.Task, error) {
	tasks, err := e.TaskProv.ListTasks(ctx, e.Config.TaskProvider.ProjectID, "to-do")
	if err != nil {
		return nil, fmt.Errorf("failed to list candidate tasks: %w", err)
	}

	// Filter tasks by role label matching
	var matched []*provider.Task
	for _, task := range tasks {
		for _, label := range task.Labels {
			if label == role {
				matched = append(matched, task)
				break
			}
		}
	}

	if len(matched) == 0 {
		return nil, nil // No eligible tasks available
	}

	// Sort deterministically: Priority DESC, Ref ASC
	priorityRank := map[provider.Priority]int{
		provider.PriorityUrgent: 4,
		provider.PriorityHigh:   3,
		provider.PriorityMedium: 2,
		provider.PriorityLow:    1,
	}

	sort.SliceStable(matched, func(i, j int) bool {
		pi := priorityRank[matched[i].Priority]
		pj := priorityRank[matched[j].Priority]
		if pi != pj {
			return pi > pj
		}
		return matched[i].Ref < matched[j].Ref
	})

	return matched[0], nil
}

// RunPulse executes one orchestration sweep pass
func (e *Engine) RunPulse(ctx context.Context, role string) (*provider.Task, error) {
	task, err := e.SelectNextTask(ctx, role)
	if err != nil {
		return nil, fmt.Errorf("pulse sweep failed: %w", err)
	}
	if task == nil {
		return nil, nil
	}

	if err := e.TaskProv.ClaimTask(ctx, task.ID, role); err != nil {
		return nil, fmt.Errorf("failed to claim task %s: %w", task.Ref, err)
	}

	return task, nil
}
