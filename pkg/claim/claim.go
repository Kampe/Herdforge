package claim

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

type ClaimRecord struct {
	TaskRef     string    `json:"task_ref"`
	WorkerID    string    `json:"worker_id"`
	ClaimedAt   time.Time `json:"claimed_at"`
	WorktreePath string   `json:"worktree_path"`
}

type ClaimManager struct {
	mu          sync.Mutex
	activeClaims map[string]*ClaimRecord
}

func NewClaimManager() *ClaimManager {
	return &ClaimManager{
		activeClaims: make(map[string]*ClaimRecord),
	}
}

// ClaimTask atomically claims a task for a worker, preventing double-allocation
func (c *ClaimManager) ClaimTask(ctx context.Context, p provider.TaskProvider, taskRef, workerID, worktreePath string) (*ClaimRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, claimed := c.activeClaims[taskRef]; claimed {
		return nil, fmt.Errorf("task %s already claimed by worker %s at %s", taskRef, existing.WorkerID, existing.ClaimedAt.Format(time.RFC3339))
	}

	rec := &ClaimRecord{
		TaskRef:      taskRef,
		WorkerID:     workerID,
		ClaimedAt:    time.Now(),
		WorktreePath: worktreePath,
	}

	c.activeClaims[taskRef] = rec
	return rec, nil
}

// ReleaseClaim releases an active claim lock
func (c *ClaimManager) ReleaseClaim(taskRef string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.activeClaims, taskRef)
}

// IsClaimed checks if a task ref is currently locked by a worker
func (c *ClaimManager) IsClaimed(taskRef string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, claimed := c.activeClaims[taskRef]
	return claimed
}
