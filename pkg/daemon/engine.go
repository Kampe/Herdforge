package daemon

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/server"
	"github.com/Kampe/Herdforge/pkg/store"
	"github.com/Kampe/Herdforge/pkg/verifier"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type Engine struct {
	Config   *config.Config
	TaskProv provider.TaskProvider
	Router   *router.ModelRouter
	Store    *store.Store
	Worktree *worktree.WorktreeManager
	Verifier *verifier.Verifier

	// health projects BLOCKED(provider_timeout)/recovering for the control plane.
	health providerHealth
}

func NewEngine(cfg *config.Config, tp provider.TaskProvider, r *router.ModelRouter, s *store.Store, wm *worktree.WorktreeManager, v *verifier.Verifier) *Engine {
	e := &Engine{
		Config:   cfg,
		TaskProv: tp,
		Router:   r,
		Store:    s,
		Worktree: wm,
		Verifier: v,
		health:   providerHealth{state: ProviderOK},
	}
	applyConfiguredDeadlines(cfg, tp)
	return e
}

// ProviderHealth returns the current board-lane health projection.
func (e *Engine) ProviderHealth() ProviderHealth {
	if e == nil {
		return ProviderHealth{State: ProviderOK}
	}
	return e.health.snapshot()
}

// ProviderStatus is the fleet label: ok | recovering | BLOCKED(provider_timeout).
func (e *Engine) ProviderStatus() string {
	return e.ProviderHealth().String()
}

// EnvControlAddr configures the production control-plane listen address
// for the forge loop (e.g. "127.0.0.1:7643"). Empty disables it.
const EnvControlAddr = "HERD_CONTROL_ADDR"

// StartControlPlane starts the production control server (live disk
// metrics + authorized exact-target reclamation) wired to this engine's
// canonical repo and worktree pool. Runtime Serve failures are surfaced
// through logf — a dead control plane never fails silently.
func (e *Engine) StartControlPlane(ctx context.Context, addr string, logf func(string)) (*server.ControlServer, error) {
	if e.Worktree == nil {
		return nil, fmt.Errorf("control plane requires a worktree manager")
	}
	defaultBranch := ""
	if e.Config != nil {
		defaultBranch = e.Config.Project.DefaultBranch
	}
	cs := server.NewProductionControlServer(addr, e.Worktree.RepoRoot, e.Worktree.WorktreeDir, defaultBranch)
	if logf != nil {
		cs.OnServeError = func(err error) { logf("control server failed: " + err.Error()) }
	}
	if err := cs.Start(ctx); err != nil {
		return nil, err
	}
	return cs, nil
}

// DiskStatus is the disk-capacity fleet label (FAC-153):
// ok | recovering | BLOCKED(disk_pressure) | BLOCKED(disk_stat_unreadable).
func (e *Engine) DiskStatus() string {
	return preflight.DefaultDiskGuard.Status()
}

// diskPaths are the volumes a claim/dispatch would mutate: temp always,
// plus canonical repo and worktree pool when wired.
func (e *Engine) diskPaths() []string {
	paths := []string{os.TempDir()}
	if e.Worktree != nil {
		paths = append(paths, e.Worktree.RepoRoot, e.Worktree.WorktreeDir)
	}
	return paths
}

func (e *Engine) deadlines() provider.Deadlines {
	return engineDeadlines(e.Config)
}

// listTasksBound is the production ListTasks path: configurable deadline + health observe.
func (e *Engine) listTasksBound(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	dls := e.deadlines()
	opCtx, cancel := provider.BoundOp(ctx, dls, provider.OpList)
	defer cancel()
	tasks, err := e.TaskProv.ListTasks(opCtx, projectID, status)
	e.health.observe(err)
	return tasks, err
}

// claimTaskBound is the production ClaimTask path.
func (e *Engine) claimTaskBound(ctx context.Context, taskID, role string) error {
	dls := e.deadlines()
	opCtx, cancel := provider.BoundOp(ctx, dls, provider.OpMutate)
	defer cancel()
	err := e.TaskProv.ClaimTask(opCtx, taskID, role)
	e.health.observe(err)
	return err
}

// SelectNextTask sorts candidate tasks deterministically by Priority DESC, Ticket Ref ASC
func (e *Engine) SelectNextTask(ctx context.Context, role string) (*provider.Task, error) {
	// While BLOCKED, refuse to select/claim — stay responsive without board spam
	// until beginRecovery (ForgeLoop tick) moves us to recovering.
	if e.health.isBlocked() {
		return nil, fmt.Errorf("select next task: %s", e.ProviderStatus())
	}

	tasks, err := e.listTasksBound(ctx, e.Config.TaskProvider.ProjectID, "to-do")
	if err != nil {
		return nil, formatProviderStepError("failed to list candidate tasks", err)
	}

	// Filter tasks by role label matching
	var matched []*provider.Task
	for _, task := range tasks {
		if len(task.Labels) == 0 {
			if role == "worker" || role == "herd-smith" || role == "" {
				matched = append(matched, task)
			}
			continue
		}
		for _, label := range task.Labels {
			if strings.EqualFold(label, role) {
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
		return provider.CompareRefs(matched[i].Ref, matched[j].Ref) < 0
	})

	return matched[0], nil
}

// RunPulse executes one orchestration sweep pass, recording to the SQLite store.
func (e *Engine) RunPulse(ctx context.Context, role string) (*provider.Task, error) {
	// BLOCKED: do not claim more work; surface status and stay responsive.
	if e.health.isBlocked() {
		return nil, fmt.Errorf("pulse sweep refused: %s", e.ProviderStatus())
	}
	// Critical disk pressure prevents a new claim before any board mutation
	// (FAC-153). A refusal is an explicit BLOCKED error, never nil,nil —
	// pressure must not read as "no work available".
	if err := preflight.CheckDiskPressure("claim", e.diskPaths()...); err != nil {
		return nil, fmt.Errorf("pulse sweep refused: %w", err)
	}

	task, err := e.SelectNextTask(ctx, role)
	if err != nil {
		return nil, fmt.Errorf("pulse sweep failed: %w", err)
	}
	if task == nil {
		return nil, nil
	}

	if err := e.claimTaskBound(ctx, task.ID, role); err != nil {
		return nil, formatProviderStepError(fmt.Sprintf("failed to claim task %s", task.Ref), err)
	}

	if e.Store != nil {
		if _, err := e.Store.RecordPulse(task.Ref, task.ID, role); err != nil {
			return nil, fmt.Errorf("record pulse: %w", err)
		}
	}

	return task, nil
}
