package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

func TestCreateTaskWorktreeDiskDenialPrecedesAllMutation(t *testing.T) {
	root := t.TempDir()
	pool := filepath.Join(root, "pool")
	calls := 0
	wm := NewWorktreePool(root, pool)
	wm.DiskAdmission = resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		calls++
		return resources.DiskDecision{State: resources.DiskBlocked, Evidence: resources.DiskEvidence{Reason: resources.DiskReasonBelowThreshold}}
	})
	if _, err := wm.CreateTaskWorktree(context.Background(), "FAC-153"); err == nil {
		t.Fatal("expected disk denial")
	}
	if calls != 1 {
		t.Fatalf("admission calls = %d, want one", calls)
	}
	if _, err := os.Stat(pool); !os.IsNotExist(err) {
		t.Fatalf("pool was created before denial: %v", err)
	}
}
