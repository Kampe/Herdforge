package worktree

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-764. Release and reclaimDeadLocked handed the STORED slot.Path to
// `git -C`, and git resolves a -C argument against the process's own working
// directory. A slot path persisted RELATIVE — which Ensure does whenever the
// pool was created through a relative pool root — therefore named a different
// directory for every caller.
//
// From the owning repository it worked. From anywhere else it either failed
// with "cannot change to", or, wherever the same relative path happened to
// exist under the caller, silently reset SOMEONE ELSE'S worktree. These tests
// run every destructive path from a FOREIGN working directory with a real
// decoy sitting at the caller-relative path, so the dangerous direction is
// reachable rather than merely described.

type owningFixture struct {
	repo     string
	poolDir  string
	slotPath string
	foreign  string
	decoy    string
	decoyRef string
	mainRef  string
}

func owningGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newOwningFixture builds a repository whose pool slot path is persisted
// relative, plus a foreign caller directory holding a decoy at exactly the
// path that relative slot names when resolved against the caller.
func newOwningFixture(t *testing.T) *owningFixture {
	t.Helper()
	f := &owningFixture{}
	f.repo = t.TempDir()
	owningGit(t, f.repo, "init", "-q", "-b", "main")
	for _, kv := range [][2]string{
		{"user.email", "t@example.invalid"}, {"user.name", "t"},
		{"commit.gpgsign", "false"}, {"tag.gpgsign", "false"}, {"gc.auto", "0"},
	} {
		owningGit(t, f.repo, "config", kv[0], kv[1])
	}
	if err := os.WriteFile(filepath.Join(f.repo, ".gitignore"), []byte("native.db\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owningGit(t, f.repo, "add", ".gitignore")
	owningGit(t, f.repo, "commit", "-q", "-m", "base")
	f.mainRef = owningGit(t, f.repo, "rev-parse", "HEAD")
	// A second commit gives the decoy a HEAD that differs from the slot's, so
	// resetting the decoy is observable rather than a no-op.
	if err := os.WriteFile(filepath.Join(f.repo, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owningGit(t, f.repo, "add", "other.txt")
	owningGit(t, f.repo, "commit", "-q", "-m", "second")
	f.decoyRef = owningGit(t, f.repo, "rev-parse", "HEAD")
	owningGit(t, f.repo, "checkout", "-q", "main")
	owningGit(t, f.repo, "reset", "-q", "--hard", f.mainRef)

	f.poolDir = filepath.Join(f.repo, ".herd", "pool")
	f.slotPath = filepath.Join(f.poolDir, "pool-01")

	// Create the pool from INSIDE the repository with a RELATIVE pool root.
	// Ensure stores filepath.Join(p.Root, name), so this is what persists a
	// relative slot path — the product writes the state, not the test.
	func() {
		t.Chdir(f.repo)
		create := NewPool(f.repo, filepath.Join(".herd", "pool"), 1)
		create.DefaultBase = "main"
		if err := create.Ensure(context.Background()); err != nil {
			t.Fatalf("ensure: %v", err)
		}
	}()
	if got := f.storedPath(t); got != filepath.Join(".herd", "pool", "pool-01") {
		t.Fatalf("fixture did not persist a relative slot path, got %q; the defect is unreachable", got)
	}

	// The foreign caller, with a real decoy worktree at the caller-relative
	// path and an untracked marker that `clean -fd` would delete.
	f.foreign = t.TempDir()
	f.decoy = filepath.Join(f.foreign, ".herd", "pool", "pool-01")
	if err := os.MkdirAll(filepath.Dir(f.decoy), 0o755); err != nil {
		t.Fatal(err)
	}
	owningGit(t, f.repo, "worktree", "add", "--detach", f.decoy, f.decoyRef)
	if err := os.WriteFile(filepath.Join(f.decoy, "marker.txt"), []byte("someone else's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// pool returns a Pool shaped the way the CLI builds one: repository root and
// pool directory both absolute, while the stored slot path stays relative.
func (f *owningFixture) pool() *Pool {
	p := NewPool(f.repo, f.poolDir, 1)
	p.DefaultBase = "main"
	return p
}

func (f *owningFixture) storedPath(t *testing.T) string {
	t.Helper()
	slots, err := f.pool().Slots()
	if err != nil {
		t.Fatalf("slots: %v", err)
	}
	if len(slots) != 1 {
		t.Fatalf("want exactly one slot, got %d", len(slots))
	}
	return slots[0].Path
}

func (f *owningFixture) slot(t *testing.T) PoolSlot {
	t.Helper()
	slots, err := f.pool().Slots()
	if err != nil {
		t.Fatalf("slots: %v", err)
	}
	return slots[0]
}

// decoyIntact is the assertion the whole fixture exists for.
func (f *owningFixture) decoyIntact(t *testing.T) {
	t.Helper()
	if head := owningGit(t, f.decoy, "rev-parse", "HEAD"); head != f.decoyRef {
		t.Fatalf("the caller-relative decoy was reset: HEAD %s, want %s", head, f.decoyRef)
	}
	if _, err := os.Stat(filepath.Join(f.decoy, "marker.txt")); err != nil {
		t.Fatalf("the caller-relative decoy's untracked marker was destroyed: %v", err)
	}
}

// corruptStoredPath rewrites TEST-OWNED pool state so a refusal path can be
// reached. It is used only by the explicit corrupt-record refusal tests, and
// never to manufacture a passing positive.
func (f *owningFixture) corruptStoredPath(t *testing.T, to string) {
	t.Helper()
	statePath := filepath.Join(f.poolDir, "pool.json")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state poolState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Slots) != 1 {
		t.Fatalf("want one slot, got %d", len(state.Slots))
	}
	state.Slots[0].Path = to
	out, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// POSITIVE: released from a foreign caller, the OWNING slot is the one reset.
func TestReleaseFromAForeignCallerResetsTheOwningSlot(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	lease, err := f.pool().Lease(ctx, "review-owning")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	// Ignored evidence the card requires preserved: native databases and
	// receipts live in released slots and `clean -fd` must not take them.
	evidence := filepath.Join(f.slotPath, "native.db")
	if err := os.WriteFile(evidence, []byte("retained evidence\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Move the owning slot off base so a real reset is observable.
	owningGit(t, f.slotPath, "checkout", "-q", "--detach", f.decoyRef)

	t.Chdir(f.foreign)
	if err := f.pool().Release(ctx, lease.LeaseID); err != nil {
		t.Fatalf("release from a foreign caller: %v", err)
	}

	if head := owningGit(t, f.slotPath, "rev-parse", "HEAD"); head != f.mainRef {
		t.Fatalf("owning slot HEAD = %s, want the base %s", head, f.mainRef)
	}
	if got := f.slot(t); got.LeaseID != "" || got.LastReleaseLeaseID != lease.LeaseID {
		t.Fatalf("owning slot was not recorded as released: %+v", got)
	}
	if _, err := os.Stat(evidence); err != nil {
		t.Fatalf("ignored evidence in the released slot was destroyed: %v", err)
	}
	f.decoyIntact(t)
}

// POSITIVE: the same anchoring in the unattended dead-holder reclaim.
func TestReclaimFromAForeignCallerResetsTheOwningSlot(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	p := f.pool()
	if _, err := p.Lease(ctx, "review-dead"); err != nil {
		t.Fatalf("lease: %v", err)
	}
	owningGit(t, f.slotPath, "checkout", "-q", "--detach", f.decoyRef)
	p.HolderLive = func(string) bool { return false }

	t.Chdir(f.foreign)
	freed, err := p.ReclaimDead(ctx)
	if err != nil {
		t.Fatalf("reclaim from a foreign caller: %v", err)
	}
	if len(freed) != 1 {
		t.Fatalf("freed = %v, want exactly the dead slot", freed)
	}
	if head := owningGit(t, f.slotPath, "rev-parse", "HEAD"); head != f.mainRef {
		t.Fatalf("reclaimed slot HEAD = %s, want the base %s", head, f.mainRef)
	}
	f.decoyIntact(t)
}

// A reassigned slot gets a fresh identity, and the retired id cannot free it.
func TestAReassignedSlotRefusesTheRetiredLeaseIdentity(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	p := f.pool()
	first, err := p.Lease(ctx, "review-first")
	if err != nil {
		t.Fatalf("lease first: %v", err)
	}
	if err := p.Release(ctx, first.LeaseID); err != nil {
		t.Fatalf("release first: %v", err)
	}
	second, err := p.Lease(ctx, "review-second")
	if err != nil {
		t.Fatalf("lease second: %v", err)
	}
	if second.LeaseID == first.LeaseID {
		t.Fatalf("a reassigned slot reused the retired lease identity %q", first.LeaseID)
	}
	if err := p.Release(ctx, first.LeaseID); err == nil {
		t.Fatal("the retired lease identity released its slot's new owner")
	} else if !strings.Contains(err.Error(), "lease not found") {
		t.Fatalf("err = %v, want lease not found", err)
	}
	if got := f.slot(t); got.LeaseID != second.LeaseID {
		t.Fatalf("the new owner's lease was disturbed: %+v", got)
	}
}

// An unknown lease frees nothing and disturbs nothing.
func TestReleaseRefusesAnUnknownLeaseAndLeavesTheOwnerHeld(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	p := f.pool()
	held, err := p.Lease(ctx, "review-held")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := p.Release(ctx, "pool-01-000000000000000000"); err == nil {
		t.Fatal("an unknown lease id released a slot")
	}
	if got := f.slot(t); got.LeaseID != held.LeaseID {
		t.Fatalf("an unknown lease id disturbed the owner: %+v", got)
	}
}

// CORRUPT RECORD, refusal: a stored path escaping the pool root is refused by
// path components, and the lease stays held. The state is corrupted here
// deliberately and only to reach this refusal.
func TestReleaseRefusesAStoredPathOutsideThePoolRootAndKeepsTheLease(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	lease, err := f.pool().Lease(ctx, "review-escape")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	escape := t.TempDir()
	marker := filepath.Join(escape, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside the pool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.corruptStoredPath(t, escape)

	err = f.pool().Release(ctx, lease.LeaseID)
	if err == nil {
		t.Fatal("a slot path outside the pool root was reset")
	}
	if !strings.Contains(err.Error(), "outside the pool root") {
		t.Fatalf("err = %v, want the containment refusal", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("the refused path was touched anyway: %v", statErr)
	}
	if got := f.slot(t); got.LeaseID != lease.LeaseID {
		t.Fatalf("a refused release cleared the lease: %+v", got)
	}
}

// CORRUPT RECORD, refusal: a directory inside the pool root that git does not
// register as a worktree is not ours to reset.
func TestReleaseRefusesAnUnregisteredPathAndKeepsTheLease(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	lease, err := f.pool().Lease(ctx, "review-unregistered")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	stranger := filepath.Join(f.poolDir, "not-a-worktree")
	if err := os.MkdirAll(stranger, 0o755); err != nil {
		t.Fatal(err)
	}
	f.corruptStoredPath(t, stranger)

	err = f.pool().Release(ctx, lease.LeaseID)
	if err == nil {
		t.Fatal("a directory git does not register as a worktree was reset")
	}
	if !strings.Contains(err.Error(), "not a registered worktree") {
		t.Fatalf("err = %v, want the registration refusal", err)
	}
	if got := f.slot(t); got.LeaseID != lease.LeaseID {
		t.Fatalf("a refused release cleared the lease: %+v", got)
	}
}

// A failed release keeps the lease: the slot is still owned, so the operator
// can retry rather than discovering an unleased slot that was never reset.
func TestReleaseKeepsTheLeaseWhenTheSlotIsGone(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	lease, err := f.pool().Lease(ctx, "review-vanished")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	owningGit(t, f.repo, "worktree", "remove", "--force", f.slotPath)

	if err := f.pool().Release(ctx, lease.LeaseID); err == nil {
		t.Fatal("a vanished slot reported a successful release")
	}
	if got := f.slot(t); got.LeaseID != lease.LeaseID {
		t.Fatalf("a failed release cleared the lease: %+v", got)
	}
}
