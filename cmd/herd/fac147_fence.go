package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// loadClaimStack opens the canonical shared claim/fence/outbox stack.
// Callers must Close.
func loadClaimStack(tp provider.TaskProvider) (*provider.ClaimStack, error) {
	return provider.OpenCanonicalClaimStack(tp)
}

// fencedBoardStatus is the production cmd/herd path for lease-guarded
// status mutations (review → in-review, etc.). Fail-closed on claim conflict.
func fencedBoardStatus(ctx context.Context, cfg *config.Config, stack *provider.ClaimStack, task *provider.Task, role, status string) error {
	if stack == nil {
		return fmt.Errorf("claim stack required for fenced board status")
	}
	if cfg == nil {
		return fmt.Errorf("config required for fenced board status")
	}
	if task == nil {
		return fmt.Errorf("task required for fenced board status")
	}
	owner, err := provider.ProcessOwnerID()
	if err != nil || owner == "" {
		return fmt.Errorf("process owner identity: %w", err)
	}
	// Claim under durable task ownership role; session actor is ownerID.
	taskRole, err := provider.TaskOwnershipRole(task, role)
	if err != nil {
		return err
	}
	key := provider.LeaseKey(".", cfg.TaskProvider.Type, cfg.TaskProvider.ProjectID, task.Ref)
	_, err = stack.MutateStatusGuarded(ctx, key, owner, taskRole, taskRole, task.ID, status)
	return err
}

// fencedBoardDone runs approve/board-done under a live lease generation.
// Closing authority is FAC-132 DoneRequest (receipt or override).
// Fail-closed: claim conflict refuses mutation (no high+1 minting).
// Named returns so deferred Release errors replace a successful result.
func fencedBoardDone(ctx context.Context, cfg *config.Config, tp provider.TaskProvider, stack *provider.ClaimStack, task *provider.Task, req hsync.DoneRequest) (done *hsync.DoneResult, err error) {
	if stack == nil {
		return nil, fmt.Errorf("claim stack required for fenced board-done (FAC-147 fail-closed)")
	}
	if cfg == nil || tp == nil || task == nil {
		return nil, fmt.Errorf("fenced board-done requires config, provider, and task")
	}
	owner, oerr := provider.ProcessOwnerID()
	if oerr != nil || owner == "" {
		return nil, fmt.Errorf("process owner identity: %w", oerr)
	}
	taskRole, rerr := provider.TaskOwnershipRole(task, "worker")
	if rerr != nil {
		return nil, rerr
	}
	key := provider.LeaseKey(".", cfg.TaskProvider.Type, cfg.TaskProvider.ProjectID, task.Ref)
	lease, err := stack.AcquireLease(ctx, key, owner, taskRole, taskRole)
	if err != nil {
		return nil, fmt.Errorf("approve refuses mutation without live lease: %w", err)
	}
	// Short-lived CLI lease: release after board-done so subsequent commands
	// are not blocked for the full TTL. Release errors propagate fail-closed.
	defer func() {
		if rerr := stack.Manager.Release(ctx, key, owner, lease.Generation); rerr != nil {
			if err == nil {
				err = fmt.Errorf("approve: release lease after board-done: %w", rerr)
				done = nil
			}
		}
	}()
	if err = stack.CAS.AdvanceFence(ctx, task.ID, lease.Generation); err != nil {
		return nil, err
	}
	// Coordinator mint identity when a minter is attached to the stack.
	if k, ok := provider.UnwrapTaskProvider(tp).(*provider.KaneoProvider); ok && k != nil {
		if stack.Minter != nil {
			_ = provider.AttachCoordinatorMinter(k, stack.Minter)
		}
		if stack.Minter != nil {
			ctx = provider.WithMintIdentity(ctx, provider.MintIdentity{
				Repo: lease.Repo, Provider: lease.Provider, Project: lease.Project,
				TaskRef: lease.TaskRef, OwnerID: lease.OwnerID,
			})
		}
	}
	fmt.Printf("  Lease   : owner=%s gen=%d\n", owner, lease.Generation)
	if req.Ref == "" {
		req.Ref = task.Ref
	}
	if req.ProjectID == "" {
		req.ProjectID = cfg.TaskProvider.ProjectID
	}
	if req.RepoDir == "" {
		req.RepoDir = "."
	}
	done, err = hsync.BoardDoneFenced(ctx, tp, stack, key, owner, lease.Generation, req)
	return done, err
}

// resolveTaskByRef finds a board card by normalized ref (any status).
func resolveTaskByRef(ctx context.Context, tp provider.TaskProvider, projectID, ref string) (*provider.Task, error) {
	want := hsync.NormalizeRef(ref)
	tasks, err := tp.ListTasks(ctx, projectID, "")
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t != nil && strings.EqualFold(hsync.NormalizeRef(t.Ref), want) {
			return t, nil
		}
	}
	return nil, fmt.Errorf("no task with ref %s on the board", want)
}
