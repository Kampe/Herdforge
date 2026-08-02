package gc

import (
	"context"
	"testing"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

func TestGCManager_ScanOverlapAndPrune(t *testing.T) {
	wm := worktree.NewWorktreeManager(".")
	gcm := NewGCManager(".", wm)

	report, err := gcm.ScanOverlap(context.Background(), 2)
	if err != nil || report == nil {
		t.Fatalf("expected clean overlap scan, got err: %v", err)
	}

	pruned, err := gcm.PruneStaleWorktrees(context.Background())
	if err != nil {
		t.Fatalf("expected clean prune execution, got err: %v", err)
	}
	if pruned < 0 {
		t.Errorf("unexpected negative pruned count: %d", pruned)
	}
}
