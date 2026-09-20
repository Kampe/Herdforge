package worktree

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLeaseFromForeignCallerReturnsOwningPath(t *testing.T) {
	assertOwningLeasePath(t, false)
}

func TestReclaimedLeaseFromForeignCallerReturnsOwningPath(t *testing.T) {
	assertOwningLeasePath(t, true)
}

// The historical relative state is written by Ensure, not fabricated by this
// test. The caller also has a registered decoy at that exact relative path, so
// handing a stored path to git -C can succeed while pinning the wrong worktree.
func assertOwningLeasePath(t *testing.T, reclaim bool) {
	t.Helper()
	f := newOwningFixture(t)
	ctx := context.Background()
	storedPath := f.storedPath(t)
	// Capture relative constructor roots before changing cwd. The reclaim
	// case instead reopens the same historical state with absolute roots.
	t.Chdir(f.repo)
	p := NewPool(".", filepath.Join(".herd", "pool"), 1)
	p.DefaultBase = "main"
	previousLease := ""
	if reclaim {
		old, err := p.Lease(ctx, "review-dead")
		if err != nil {
			t.Fatalf("initial lease: %v", err)
		}
		previousLease = old.LeaseID
		p = f.pool()
		p.HolderLive = func(string) bool { return false }
	}
	// A third commit makes both a successful intended pin and an accidental
	// decoy pin observable; neither worktree already has this HEAD.
	candidate := owningGit(t, f.repo, "commit-tree", f.decoyRef+"^{tree}", "-p", f.decoyRef, "-m", "review candidate")
	t.Chdir(f.foreign)
	lease, err := p.Lease(ctx, "review-current")
	if err != nil {
		t.Fatalf("lease from foreign caller: %v", err)
	}
	if lease.LeaseID == previousLease {
		t.Fatal("returned lease did not acquire a fresh identity")
	}
	// Match the review CLI's execution contract, including its foreign cwd.
	owningGit(t, ".", "-C", lease.Path, "reset", "--hard", candidate)
	if head := owningGit(t, f.slotPath, "rev-parse", "HEAD"); head != candidate {
		t.Fatalf("returned lease path pinned the wrong worktree: owning HEAD %s, want %s", head, candidate)
	}
	f.decoyIntact(t)
	if !filepath.IsAbs(lease.Path) || lease.Path != p.repoPath(storedPath) {
		t.Fatalf("returned lease path is not owner-rooted: %q", lease.Path)
	}
	if got := f.storedPath(t); got != storedPath {
		t.Fatalf("leasing rewrote the persisted path: got %q, want %q", got, storedPath)
	}
	if reclaim {
		if got := f.slot(t); got.LastReleaseLeaseID != previousLease || got.LastReleasePath != storedPath {
			t.Fatalf("reclaim lost the historical release identity or path: %+v", got)
		}
	}
	if err := p.ReleaseExact(ctx, lease.Name, lease.LeaseID, lease.LeasedAt.UnixNano(), lease.Path); err != nil {
		t.Fatalf("release using returned runtime path: %v", err)
	}
	if got := f.slot(t); got.Path != storedPath || got.LastReleasePath != storedPath || got.LastReleaseLeaseID != lease.LeaseID {
		t.Fatalf("release rewrote persisted path or lease evidence: %+v", got)
	}
}
