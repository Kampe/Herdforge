package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

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
	// Ownership is the durable cross-process launch lease (pkg/claim SQLite).
	// When nil, OpenLeaseOwnership under .herd/launch-claims.db is used.
	Ownership deps.OwnershipClaimer

	// health projects BLOCKED(provider_timeout)/recovering for the control plane.
	health              providerHealth
	claimMu             sync.Mutex
	lastClaimRef        string
	lastClaimGeneration int64
}

// LastClaimIdentity returns the task and generation most recently fenced by
// RunPulse. Launch callers use it to reissue a decision after the dependency
// and ownership claim; a zero/mismatched identity is not launchable.
func (e *Engine) LastClaimIdentity() (string, int64) {
	if e == nil {
		return "", 0
	}
	e.claimMu.Lock()
	defer e.claimMu.Unlock()
	return e.lastClaimRef, e.lastClaimGeneration
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
			selectionErr := fmt.Errorf("select next task: provenance %s: %w", t.Ref, perr)
			if evidenceErr := e.persistSelectionEvidence(nil, &deps.BlockedError{Ref: deps.Ref(t.Ref), Code: "malformed_provenance", Reason: perr.Error()}); evidenceErr != nil {
				return nil, "", errors.Join(selectionErr, evidenceErr)
			}
			return nil, "", selectionErr
		}
		// Missing provenance → per-card BLOCKED inside SelectEligibleRefs
		// (not invent empty OK). Only Present records are attached.
		if p != nil && p.Present {
			desiredByRef[t.Ref] = p
		}
	}
	eligible, revisions, blocked, gerr := deps.SelectEligibleRefs(ctx, store, deps.EntryPulse, matched, desiredByRef)
	if gerr != nil {
		// Capability / hard store failures are fail-closed, but their attention
		// evidence and any earlier soft blocks must be durable before returning.
		selectionErr := fmt.Errorf("select next task: dependency gate: %w", gerr)
		if evidenceErr := e.persistSelectionEvidence(blocked, gerr); evidenceErr != nil {
			return nil, "", errors.Join(selectionErr, evidenceErr)
		}
		return nil, "", selectionErr
	}
	if evidenceErr := e.persistSelectionEvidence(blocked, nil); evidenceErr != nil {
		return nil, "", evidenceErr
	}
	if len(eligible) == 0 {
		if len(blocked) > 0 {
			return nil, "", fmt.Errorf("select next task: all candidates blocked: %s", summarizeBlocked(blocked))
		}
		return nil, "", nil
	}
	// eligible preserves input order (already priority DESC / ref ASC).
	head := eligible[0]
	return head, revisions[head.Ref], nil
}

func (e *Engine) persistSelectionEvidence(blocked []deps.GateResult, selectionErr error) error {
	if e.Store == nil {
		if len(blocked) == 0 && selectionErr == nil {
			return nil
		}
		return fmt.Errorf("select next task: durable dependency BLOCKED evidence unavailable: store is nil")
	}
	items := make([]store.BlockedSelection, 0, len(blocked)+1)
	for _, br := range blocked {
		items = append(items, store.BlockedSelection{
			Ref: string(br.Ref), TaskID: string(br.TaskID), Entrypoint: string(br.Entrypoint),
			Code: br.Code, Reason: br.Reason, GraphRevision: br.GraphRevision, ProviderRevision: br.ProviderRevision,
		})
	}
	if selectionErr != nil {
		var be *deps.BlockedError
		if errors.As(selectionErr, &be) {
			duplicate := false
			for _, item := range items {
				if item.Ref == string(be.Ref) && item.Code == be.Code {
					duplicate = true
					break
				}
			}
			if !duplicate {
				items = append(items, store.BlockedSelection{
					Ref: string(be.Ref), Entrypoint: string(deps.EntryPulse), Code: be.Code,
					Reason: blockedErrorReason(be),
				})
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	if _, err := e.Store.RecordBlockedSelections(items); err != nil {
		return fmt.Errorf("select next task: record dependency BLOCKED evidence: %w", err)
	}
	return nil
}

func blockedErrorReason(be *deps.BlockedError) string {
	if be == nil || len(be.Details) == 0 {
		if be == nil {
			return "dependency gate blocked"
		}
		return be.Reason
	}
	return be.Reason + ": " + strings.Join(be.Details, "; ")
}

func summarizeBlocked(blocked []deps.GateResult) string {
	parts := make([]string, 0, len(blocked))
	for _, br := range blocked {
		parts = append(parts, fmt.Sprintf("%s (%s: %s)", br.Ref, br.Code, br.Reason))
	}
	return strings.Join(parts, "; ")
}

func (e *Engine) depsStore() deps.RelationStore {
	if e.Deps != nil {
		return e.Deps
	}
	return deps.StoreFor(e.TaskProv, e.Config.TaskProvider.ProjectID)
}

func (e *Engine) ownershipClaimer() (deps.OwnershipClaimer, error) {
	if e.Ownership != nil {
		return e.Ownership, nil
	}
	root := "."
	if e.Worktree != nil && e.Worktree.RepoRoot != "" {
		root = e.Worktree.RepoRoot
	}
	repo := "herd"
	providerType := "memory"
	project := ""
	if e.Config != nil {
		if e.Config.Project.Name != "" {
			repo = e.Config.Project.Name
		}
		if e.Config.TaskProvider.Type != "" {
			providerType = e.Config.TaskProvider.Type
		}
		project = e.Config.TaskProvider.ProjectID
	}
	return deps.OpenLeaseOwnership(deps.ResolveLaunchLeasePath(root), repo, providerType, project)
}

// RunPulse executes one orchestration sweep pass, recording to the SQLite store.
// FAC-159: durable claim lease (pkg/claim) + revision-fenced graph check before
// board claim; post-claim drift compensates only while owner+generation match.
func (e *Engine) RunPulse(ctx context.Context, role string) (*provider.Task, error) {
	// BLOCKED: do not claim more work; surface status and stay responsive.
	if e.health.isBlocked() {
		return nil, fmt.Errorf("pulse sweep refused: %s", e.ProviderStatus())
	}

	// Project graph fence for selection + fenced claim (one hydration / reuse).
	ctx, _ = deps.WithSnapshotFence(ctx)

	task, selectionRev, err := e.selectNextTaskWithRevision(ctx, role)
	if err != nil {
		return nil, fmt.Errorf("pulse sweep failed: %w", err)
	}
	if task == nil {
		return nil, nil
	}

	desired, perr := deps.ExtractProvenanceFromText(task.Description)
	if perr != nil {
		return nil, fmt.Errorf("pulse provenance: %w", perr)
	}
	if desired == nil || !desired.Present {
		return nil, fmt.Errorf("pulse: %w for %s", deps.ErrMissingProvenance, task.Ref)
	}
	if berr := desired.BindAndValidate(deps.Ref(task.Ref), deps.TaskID(task.ID)); berr != nil {
		return nil, fmt.Errorf("pulse provenance bind: %w", berr)
	}
	if strings.TrimSpace(selectionRev) == "" {
		return nil, fmt.Errorf("pulse: %w; empty selection revision", deps.ErrClaimFence)
	}

	// Durable cross-process lease BEFORE board status mutation (not process-local).
	own, oerr := e.ownershipClaimer()
	if oerr != nil {
		return nil, fmt.Errorf("pulse lease store: %w", oerr)
	}
	claimRole := role
	if claimRole == "" {
		claimRole = "pulse"
	}
	tok, cerr := own.ClaimExclusive(ctx, deps.TaskID(task.ID), deps.Ref(task.Ref), claimRole, selectionRev, "", "")
	if cerr != nil {
		return nil, fmt.Errorf("pulse lease claim: %w", cerr)
	}
	e.claimMu.Lock()
	e.lastClaimRef, e.lastClaimGeneration = task.Ref, tok.Generation
	e.claimMu.Unlock()

	// Fenced claim: pre/post graph check around board ClaimTask. Compensation is
	// generation-fenced — board to-do only when we still hold owner+generation.
	// Post revalidation reuses the fence snapshot + O(1) incident ListRelations
	// (no full-board re-fanout / CLI storm).
	_, gerr := deps.FencedClaim(
		ctx,
		e.depsStore(),
		deps.Ref(task.Ref),
		deps.TaskID(task.ID),
		desired,
		selectionRev,
		func(cctx context.Context) error {
			if owns, err := own.StillOwns(cctx, tok); err != nil {
				return err
			} else if !owns {
				return fmt.Errorf("%w: lost lease before board claim", deps.ErrNotOwner)
			}
			return e.claimTaskBound(cctx, task.ID, role)
		},
		func(cctx context.Context, taskID deps.TaskID, reason string) error {
			// Board reverse while still owner; release only after board OK.
			// On board failure retain lease (Recovering) — never release-first.
			owns, err := own.StillOwns(cctx, tok)
			if err != nil {
				return err
			}
			if !owns {
				return fmt.Errorf("%w: refuse board compensate (%s)", deps.ErrNotOwner, reason)
			}
			if e.TaskProv != nil {
				if boardErr := e.TaskProv.UpdateStatus(cctx, string(taskID), provider.StatusToDo); boardErr != nil {
					return fmt.Errorf("board compensate retained lease (Recovering): %w", boardErr)
				}
			}
			if rErr := own.ReleaseIfOwner(cctx, tok, reason); rErr != nil && !errors.Is(rErr, deps.ErrNotOwner) {
				return rErr
			}
			return nil
		},
	)
	if gerr != nil {
		// Post-claim path already tried board+release via compensateFn.
		// claimFn-only failures: release while still owner (no board flip yet).
		if owns, _ := own.StillOwns(ctx, tok); owns {
			if cErr := own.ReleaseIfOwner(ctx, tok, "pulse_claim_failed"); cErr != nil && !errors.Is(cErr, deps.ErrNotOwner) {
				gerr = errors.Join(gerr, cErr)
			}
		}
		return nil, fmt.Errorf("pulse claim blocked (dependency fence): %w", gerr)
	}

	if e.Store != nil {
		if _, err := e.Store.RecordPulse(task.Ref, task.ID, role); err != nil {
			return nil, fmt.Errorf("record pulse: %w", err)
		}
	}

	return task, nil
}
