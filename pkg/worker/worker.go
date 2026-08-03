package worker

import (
	"context"
	"fmt"

	"github.com/Kampe/Herdforge/pkg/control"
	"github.com/Kampe/Herdforge/pkg/harness"
	"github.com/Kampe/Herdforge/pkg/provider"
)

type WorkerLane struct {
	LaneID      string
	Role        string
	HarnessCfg  *harness.HarnessConfig
	ActiveTask  *provider.Task
	WorktreeDir string
}

// ReceiveControlOnce is the standing-lane production receive entrypoint. The
// processor is task-scoped and idempotency-aware; a lane cannot consume mail
// through a label-only or coordinator-owned shortcut.
func (s *LaneSupervisor) ReceiveControlOnce(ctx context.Context, laneID string, loop *control.RecipientLoop) error {
	if s == nil || s.Lanes[laneID] == nil {
		return fmt.Errorf("lane %s not found", laneID)
	}
	if loop == nil {
		return control.ErrProcessorUnavailable
	}
	return loop.RunOnce(ctx)
}

type LaneSupervisor struct {
	RepoRoot string
	Lanes    map[string]*WorkerLane
}

func NewLaneSupervisor(repoRoot string) *LaneSupervisor {
	return &LaneSupervisor{
		RepoRoot: repoRoot,
		Lanes:    make(map[string]*WorkerLane),
	}
}

// SpinLane initializes and starts a new worker lane in a dedicated worktree (porting bin/herd-spin & bin/herd-up)
func (s *LaneSupervisor) SpinLane(ctx context.Context, laneID, role, harnessName, worktreeDir string) (*WorkerLane, error) {
	cfg := harness.GetHarnessConfig(harnessName)

	lane := &WorkerLane{
		LaneID:      laneID,
		Role:        role,
		HarnessCfg:  cfg,
		WorktreeDir: worktreeDir,
	}

	s.Lanes[laneID] = lane
	return lane, nil
}

// ShutdownLane gracefully terminates a worker lane
func (s *LaneSupervisor) ShutdownLane(laneID string) error {
	lane, exists := s.Lanes[laneID]
	if !exists {
		return fmt.Errorf("lane %s not found", laneID)
	}
	_ = lane
	delete(s.Lanes, laneID)
	return nil
}
