package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

func TestWorktreeRemovalRefusesLiveLease(t *testing.T) {
	for _, tc := range []struct {
		name       string
		task       string
		leasePath  string
		leaseStart time.Time
		leaseTTL   time.Duration
		wantRefuse bool
	}{
		{name: "live lease", task: "FAC-421", wantRefuse: true, leaseStart: time.Now(), leaseTTL: time.Hour},
		{name: "expired lease", task: "FAC-422", wantRefuse: false, leaseStart: time.Now().Add(-2 * time.Hour), leaseTTL: time.Hour},
		{name: "different worktree", task: "FAC-423", wantRefuse: false, leasePath: ".herd/worktrees/fac-423", leaseStart: time.Now(), leaseTTL: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
				t.Fatal(err)
			}
			store, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			target := filepath.Join(root, ".herd", "worktrees", "fac-421")
			leasePath := tc.leasePath
			if leasePath == "" {
				leasePath = target
			}
			if _, err := store.Acquire(context.Background(), claim.LeaseKey{
				Repo: "herdforge", Provider: "kaneo", Project: "project", TaskRef: tc.task,
			}, "worker", "builder", leasePath, tc.leaseStart, tc.leaseTTL); err != nil {
				t.Fatal(err)
			}
			if leasePath != target {
				// FAC-453: removal now also requires the target path to have
				// *some* lease history (active or released), not just "no
				// active claim." This case is about an active lease on a
				// DIFFERENT path not blocking removal of target -- so target
				// itself needs its own (dispatched-and-released) history,
				// same as any real task worktree whose lane finished.
				own, err := store.Acquire(context.Background(), claim.LeaseKey{
					Repo: "herdforge", Provider: "kaneo", Project: "project", TaskRef: "FAC-421",
				}, "worker", "builder", target, time.Now(), time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.Release(context.Background(), claim.LeaseKey{
					Repo: "herdforge", Provider: "kaneo", Project: "project", TaskRef: "FAC-421",
				}, "worker", own.Generation, time.Now()); err != nil {
					t.Fatal(err)
				}
			}

			called := false
			manager := NewWorktreeManager(root)
			manager.RemoveWorktreeFunc = func(context.Context, string) error {
				called = true
				return nil
			}
			err = manager.RemoveWorktree(context.Background(), target)
			if tc.wantRefuse != (err != nil) {
				t.Fatalf("removal error = %v, want refusal=%t", err, tc.wantRefuse)
			}
			if called == tc.wantRefuse {
				t.Fatalf("removal mutation called=%t, want %t", called, !tc.wantRefuse)
			}
		})
	}
}

// TestWorktreePool_CreateAndList uses a temp-origin fixture (FAC-152):
// pointing a WorktreeManager at "." would run "git worktree list" with
// cmd.Dir="." — the developer's live repository — and is exactly the
// anti-pattern that let a prior test register a real worktree at
// pkg/dispatch/.herd/worktrees/fac-1. Every worktree op here must run
// against tmpDir so `git worktree list` in the live repo stays unchanged.
func TestWorktreePool_CreateAndList(t *testing.T) {
	tmpDir := t.TempDir()
	initRepo(t, tmpDir)
	wtDir := filepath.Join(tmpDir, "worktrees")

	pool := NewWorktreePool(tmpDir, wtDir)
	if pool.RepoRoot != tmpDir || pool.WorktreeDir != wtDir {
		t.Errorf("unexpected pool fields: %+v", pool)
	}

	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("failed to create temp worktree dir: %v", err)
	}

	ctx := context.Background()
	wtList, err := pool.ListWorktrees(ctx)
	if err != nil {
		t.Fatalf("expected clean list worktrees execution, got err: %v", err)
	}

	if len(wtList) == 0 {
		t.Errorf("expected at least main checkout worktree, got 0")
	}
	for _, wt := range wtList {
		if !isContainedIn(wt.Path, tmpDir) {
			t.Fatalf("ListWorktrees leaked a path outside the temp fixture: %+v", wt)
		}
	}
}
