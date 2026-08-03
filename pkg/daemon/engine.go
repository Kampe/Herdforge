package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
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
	// Deps is the FAC-159 relation store; when nil, derived from TaskProv.
	Deps deps.RelationStore

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

// SelectNextTask sorts candidate tasks deterministically by Priority DESC, Ticket Ref ASC.
// FAC-159: dependency-graph gate filters after role match and before return — no side effects.
// selectionRevisions is populated for the returned task so RunPulse can re-bind at claim.
func (e *Engine) SelectNextTask(ctx context.Context, role string) (*provider.Task, error) {
	task, _, err := e.selectNextTaskWithRevision(ctx, role)
	return task, err
}

func (e *Engine) selectNextTaskWithRevision(ctx context.Context, role string) (*provider.Task, string, error) {
	// While BLOCKED, refuse to select/claim — stay responsive without board spam
	// until beginRecovery (ForgeLoop tick) moves us to recovering.
	if e.health.isBlocked() {
		return nil, "", fmt.Errorf("select next task: %s", e.ProviderStatus())
	}

	tasks, err := e.listTasksBound(ctx, e.Config.TaskProvider.ProjectID, "to-do")
	if err != nil {
		return nil, "", formatProviderStepError("failed to list candidate tasks", err)
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
		return nil, "", nil // No eligible tasks available
	}

	// Sort deterministically: Priority DESC, Ref ASC — BEFORE dependency filter
	// so post-filter order remains priority DESC / ticket ASC.
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

	// FAC-159: filter by authoritative dependency gate (read-only).
	store := e.depsStore()
	desiredByRef := map[string]*deps.Provenance{}
	for _, t := range matched {
		if t == nil {
			continue
		}
		p, perr := deps.ExtractProvenanceFromText(t.Description)
		if perr != nil {
			// Fail closed on malformed structured provenance — never guess.
			return nil, "", fmt.Errorf("select next task: provenance %s: %w", t.Ref, perr)
		}
		// Missing provenance → per-card BLOCKED inside SelectEligibleRefs
		// (not invent empty OK). Only Present records are attached.
		if p != nil && p.Present {
			desiredByRef[t.Ref] = p
		}
	}
	eligible, revisions, _, gerr := deps.SelectEligibleRefs(ctx, store, deps.EntryPulse, matched, desiredByRef)
	if gerr != nil {
		// Capability / hard store failures are fail-closed (not "no candidates").
		return nil, "", fmt.Errorf("select next task: dependency gate: %w", gerr)
	}
	if len(eligible) == 0 {
		return nil, "", nil
	}
	// eligible preserves input order (already priority DESC / ref ASC).
	head := eligible[0]
	return head, revisions[head.Ref], nil
}

func (e *Engine) depsStore() deps.RelationStore {
	if e.Deps != nil {
		return e.Deps
	}
	return deps.StoreFor(e.TaskProv, e.Config.TaskProvider.ProjectID)
}

// RunPulse executes one orchestration sweep pass, recording to the SQLite store.
// FAC-159: re-validates dependency graph immediately before claim (TOCTOU close).
func (e *Engine) RunPulse(ctx context.Context, role string) (*provider.Task, error) {
	// BLOCKED: do not claim more work; surface status and stay responsive.
	if e.health.isBlocked() {
		return nil, fmt.Errorf("pulse sweep refused: %s", e.ProviderStatus())
	}

	task, selectionRev, err := e.selectNextTaskWithRevision(ctx, role)
	if err != nil {
		return nil, fmt.Errorf("pulse sweep failed: %w", err)
	}
	if task == nil {
		return nil, nil
	}

	// Fenced claim: bind selection revision, claim, post-claim re-validate.
	// Atomic graph+claim is unavailable; post-claim drift triggers compensation
	// (status back to to-do) so TOCTOU cannot leave a false-ready card.
	desired, _ := deps.ExtractProvenanceFromText(task.Description)
	_, gerr := deps.FencedClaim(
		ctx,
		e.depsStore(),
		deps.Ref(task.Ref),
		deps.TaskID(task.ID),
		desired,
		selectionRev,
		func(cctx context.Context) error {
			return e.claimTaskBound(cctx, task.ID, role)
		},
		func(cctx context.Context, taskID deps.TaskID, reason string) error {
			// Best-effort compensate: reverse claim to to-do.
			return e.TaskProv.UpdateStatus(cctx, string(taskID), provider.StatusToDo)
		},
	)
	if gerr != nil {
		return nil, fmt.Errorf("pulse claim blocked (dependency fence): %w", gerr)
	}

	if e.Store != nil {
		if _, err := e.Store.RecordPulse(task.Ref, task.ID, role); err != nil {
			return nil, fmt.Errorf("record pulse: %w", err)
		}
	}

	return task, nil
}
