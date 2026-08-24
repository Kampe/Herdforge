package worktree

import (
	"context"
	"os/exec"
	"testing"
)

// newReclaimPool builds a real 2-slot pool on a throwaway repo so reclamation
// is exercised against actual git worktrees, not a mock.
func newReclaimPool(t *testing.T) *Pool {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "base")

	p := NewPool(repo, t.TempDir()+"/pool", 2)
	p.DefaultBase = "main"
	if err := p.Ensure(context.Background()); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return p
}

// FAC-591: a launch that died after leasing left an ownerless lease that no
// command could free — GC refuses while anything is leased and Release needs a
// lease id nobody recorded. The pool wedged at "no available clean slots" while
// reporting itself saturated with zero live reviewers, repeatedly, each time
// needing a human to unstick it.
func TestLeaseReclaimsDeadHoldersWhenPoolLooksFull(t *testing.T) {
	p := newReclaimPool(t)
	ctx := context.Background()

	a, err := p.Lease(ctx, "review-dead-1")
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	if _, err := p.Lease(ctx, "review-dead-2"); err != nil {
		t.Fatalf("second lease: %v", err)
	}
	// Pool is now full with two holders. Without reclamation this must fail.
	if _, err := p.Lease(ctx, "review-new"); err == nil {
		t.Fatal("a full pool with no reclaimer must refuse a third lease")
	}

	// Both holders are dead. The next lease must reclaim rather than refuse.
	p.HolderLive = func(string) bool { return false }
	got, err := p.Lease(ctx, "review-new")
	if err != nil {
		t.Fatalf("lease after reclamation must succeed, got %v", err)
	}
	if got.LeaseID == a.LeaseID {
		t.Error("reclaimed slot must be issued under a NEW lease id")
	}
	if got.Purpose != "review-new" {
		t.Errorf("purpose = %q want review-new", got.Purpose)
	}
}

// The dangerous direction: a live reviewer's slot must never be reclaimed, or
// its worktree is reset out from under it mid-review and its verdict describes
// a commit nobody asked about.
func TestLeaseNeverReclaimsALiveHolder(t *testing.T) {
	p := newReclaimPool(t)
	ctx := context.Background()
	if _, err := p.Lease(ctx, "review-live-1"); err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	if _, err := p.Lease(ctx, "review-live-2"); err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	p.HolderLive = func(string) bool { return true }
	if _, err := p.Lease(ctx, "review-new"); err == nil {
		t.Fatal("a pool held entirely by LIVE reviewers must still refuse, not steal a slot")
	}
}

// Mixed: reclaim only the dead one, leave the live one alone.
func TestReclaimDeadFreesOnlyDeadHolders(t *testing.T) {
	p := newReclaimPool(t)
	ctx := context.Background()
	if _, err := p.Lease(ctx, "review-live"); err != nil {
		t.Fatalf("lease live: %v", err)
	}
	if _, err := p.Lease(ctx, "review-dead"); err != nil {
		t.Fatalf("lease dead: %v", err)
	}
	p.HolderLive = func(purpose string) bool { return purpose == "review-live" }

	freed, err := p.ReclaimDead(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(freed) != 1 {
		t.Fatalf("expected exactly one slot freed, got %v", freed)
	}
	slots, err := p.Slots()
	if err != nil {
		t.Fatalf("slots: %v", err)
	}
	var live, empty int
	for _, s := range slots {
		switch s.Purpose {
		case "review-live":
			live++
		case "":
			empty++
		default:
			t.Errorf("unexpected surviving purpose %q", s.Purpose)
		}
	}
	if live != 1 || empty != 1 {
		t.Errorf("want 1 live + 1 free, got live=%d free=%d", live, empty)
	}
}

// An empty purpose can never be attributed to a holder, so it can never be
// proven live and must be reclaimable — that is exactly the ownerless lease
// that wedged the real pool.
func TestReclaimFreesOwnerlessLease(t *testing.T) {
	p := newReclaimPool(t)
	ctx := context.Background()
	if _, err := p.Lease(ctx, "review-x"); err != nil {
		t.Fatalf("lease: %v", err)
	}
	// Simulate the ownerless lease seen in production: held, no purpose.
	if err := p.withLock(func() error {
		st, err := p.readState()
		if err != nil {
			return err
		}
		for i := range st.Slots {
			if st.Slots[i].LeaseID != "" {
				st.Slots[i].Purpose = ""
			}
		}
		return p.writeState(st)
	}); err != nil {
		t.Fatalf("seed ownerless lease: %v", err)
	}

	// Even a HolderLive that claims everything is alive must not save it: an
	// unattributable lease has no holder to be alive.
	p.HolderLive = func(string) bool { return true }
	freed, err := p.ReclaimDead(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(freed) != 1 {
		t.Fatalf("an ownerless lease must be reclaimable, freed=%v", freed)
	}
}

// With no predicate injected, behaviour must be exactly as before: no
// reclamation, no surprise slot theft in callers that never opted in.
func TestNoReclaimWithoutPredicate(t *testing.T) {
	p := newReclaimPool(t)
	ctx := context.Background()
	if _, err := p.Lease(ctx, "a"); err != nil {
		t.Fatalf("lease a: %v", err)
	}
	if _, err := p.Lease(ctx, "b"); err != nil {
		t.Fatalf("lease b: %v", err)
	}
	freed, err := p.ReclaimDead(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if len(freed) != 0 {
		t.Errorf("no predicate means no reclamation, got %v", freed)
	}
}
