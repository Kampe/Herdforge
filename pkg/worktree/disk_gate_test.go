package worktree

import (
	"context"
	"os"
	"os/exec"
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

func TestCreateWorktreeProbesActualTargetBeforeGit(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := resources.ResolveExistingPath(target)
	if err != nil {
		t.Fatal(err)
	}
	backend := resources.StatFSFunc(func(path string) (resources.Capacity, error) {
		if path == resolvedTarget {
			return resources.Capacity{FilesystemID: "low-target", TotalBytes: 1000, FreeBytes: 1, TotalInodes: 100, FreeInodes: 90}, nil
		}
		return resources.Capacity{FilesystemID: "healthy", TotalBytes: 1000, FreeBytes: 900, TotalInodes: 100, FreeInodes: 90}, nil
	})
	wm := NewWorktreeManager(root)
	wm.DiskAdmission = resources.NewCapacityGate(backend, resources.DiskPolicy{ReserveBytes: 100, ReservePercent: 1, ReserveInodes: 1})
	gitCalls := 0
	old := execCommandContext
	defer func() { execCommandContext = old }()
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gitCalls++
		return exec.CommandContext(ctx, "true")
	}
	if err := wm.CreateWorktree(context.Background(), "target-branch", target); err == nil {
		t.Fatal("expected low target volume denial")
	}
	if gitCalls != 0 {
		t.Fatalf("git callback count = %d, want zero", gitCalls)
	}
}
