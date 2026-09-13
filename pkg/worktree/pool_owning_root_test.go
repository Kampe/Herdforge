package worktree

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
// path components, and the lease stays held.
//
// The escaped target is a REAL registered worktree of THIS repository, with
// its own HEAD and an untracked marker. That is the whole point: an
// unregistered scratch directory would be refused by the registration guard
// instead, so the test would still go red with containment removed but for the
// wrong reason, and nothing outside the pool would ever have been at risk. The
// state assertions come first, because the destruction is the finding.
func TestReleaseRefusesAStoredPathOutsideThePoolRootAndKeepsTheLease(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	lease, err := f.pool().Lease(ctx, "review-escape")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside-worktree")
	owningGit(t, f.repo, "worktree", "add", "--detach", outside, f.decoyRef)
	marker := filepath.Join(outside, "marker.txt")
	if err := os.WriteFile(marker, []byte("outside the pool\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.corruptStoredPath(t, outside)

	err = f.pool().Release(ctx, lease.LeaseID)

	if head := owningGit(t, outside, "rev-parse", "HEAD"); head != f.decoyRef {
		t.Fatalf("containment did not protect the outside worktree: HEAD %s, want %s", head, f.decoyRef)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("containment did not protect the outside worktree's untracked marker: %v", statErr)
	}
	if err == nil {
		t.Fatal("a registered worktree outside the pool root was reset")
	}
	if !strings.Contains(err.Error(), "outside the pool root") {
		t.Fatalf("err = %v, want the containment refusal", err)
	}
	if got := f.slot(t); got.LeaseID != lease.LeaseID {
		t.Fatalf("a refused release cleared the lease: %+v", got)
	}
}

// CORRUPT RECORD, refusal: a real git checkout UNDER the pool root that this
// repository does not register as one of its worktrees is not ours to reset.
//
// The target is a SEPARATE repository, not a plain directory: `git -C` in a
// plain nested directory discovers the enclosing repository and operates on
// that instead, so a non-git target proves nothing about the registration
// boundary. This one owns its own HEAD, its own main, and an untracked marker,
// so removing the guard is destructive in a way the test can see.
func TestReleaseRefusesAnUnregisteredPathAndKeepsTheLease(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	lease, err := f.pool().Lease(ctx, "review-unregistered")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	stranger := filepath.Join(f.poolDir, "foreign-checkout")
	if err := os.MkdirAll(stranger, 0o755); err != nil {
		t.Fatal(err)
	}
	owningGit(t, stranger, "init", "-q", "-b", "main")
	for _, kv := range [][2]string{
		{"user.email", "t@example.invalid"}, {"user.name", "t"},
		{"commit.gpgsign", "false"}, {"gc.auto", "0"},
	} {
		owningGit(t, stranger, "config", kv[0], kv[1])
	}
	if err := os.WriteFile(filepath.Join(stranger, "a.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owningGit(t, stranger, "add", "a.txt")
	owningGit(t, stranger, "commit", "-q", "-m", "foreign first")
	strangerHead := owningGit(t, stranger, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(stranger, "a.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owningGit(t, stranger, "add", "a.txt")
	owningGit(t, stranger, "commit", "-q", "-m", "foreign second")
	// HEAD sits behind its own main, so a `reset --hard main` MOVES it.
	owningGit(t, stranger, "checkout", "-q", "--detach", strangerHead)
	strangerMarker := filepath.Join(stranger, "marker.txt")
	if err := os.WriteFile(strangerMarker, []byte("someone else's repository\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.corruptStoredPath(t, stranger)

	err = f.pool().Release(ctx, lease.LeaseID)

	if head := owningGit(t, stranger, "rev-parse", "HEAD"); head != strangerHead {
		t.Fatalf("the registration guard did not protect the foreign checkout: HEAD %s, want %s", head, strangerHead)
	}
	if _, statErr := os.Stat(strangerMarker); statErr != nil {
		t.Fatalf("the registration guard did not protect the foreign checkout's untracked marker: %v", statErr)
	}
	if err == nil {
		t.Fatal("a checkout this repository does not register as a worktree was reset")
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

// RELATIVE CONSTRUCTOR. NewPool(".", ".herd/pool") is in live use, and the
// review found that anchoring only the slot path left the ANCHOR itself
// moving: statePath, lockPath and containment all resolved against whatever
// directory the process had reached. With a matching decoy state under the
// caller, the checks validated the decoy and the reset landed there.
//
// The roots are captured while the caller's directory is still the one the
// spellings were written against, so a later move cannot redirect them.
func TestRelativeConstructorAnchorsBeforeTheCallerMoves(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()

	// Built with RELATIVE spellings from inside the repository, exactly as
	// idlepool and the retirement authority build theirs.
	t.Chdir(f.repo)
	rel := NewPool(".", filepath.Join(".herd", "pool"), 1)
	rel.DefaultBase = "main"
	lease, err := rel.Lease(ctx, "review-relative")
	if err != nil {
		t.Fatalf("lease through a relative constructor: %v", err)
	}
	owningGit(t, f.slotPath, "checkout", "-q", "--detach", f.decoyRef)

	// A complete decoy pool under the caller: same relative spelling, its own
	// state file, its own registered worktree. Before the fix this is what the
	// relative roots resolved to.
	decoyPool := filepath.Join(f.foreign, ".herd", "pool")
	if err := os.MkdirAll(decoyPool, 0o755); err != nil {
		t.Fatal(err)
	}
	decoyState, err := os.ReadFile(filepath.Join(f.poolDir, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyPool, "pool.json"), decoyState, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(f.foreign)
	if err := rel.Release(ctx, lease.LeaseID); err != nil {
		t.Fatalf("release after the caller moved: %v", err)
	}

	if head := owningGit(t, f.slotPath, "rev-parse", "HEAD"); head != f.mainRef {
		t.Fatalf("the owning slot was not the one released: HEAD %s, want %s", head, f.mainRef)
	}
	f.decoyIntact(t)
	if got := f.slot(t); got.LeaseID != "" {
		t.Fatalf("the owning pool state was not updated: %+v", got)
	}
}

// FIXED CLOCK. Pool.Now is an exposed deterministic seam, so a later
// assignment could receive the same public lease id AND the same generation as
// a retired one. The retired holder then released its slot's new owner.
//
// This drives the real public paths repeatedly, and across a rebuilt Pool
// object, so the guarantee cannot come from in-memory state.
func TestFixedClockMintsAUniqueIncarnationForEveryAssignment(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	frozen := time.Unix(0, 1_700_000_000_000_000_000).UTC()

	fixed := func() *Pool {
		p := f.pool()
		p.Now = func() time.Time { return frozen }
		return p
	}

	seen := map[string]bool{}
	var previous []string
	for cycle := 0; cycle < 3; cycle++ {
		// A REBUILT pool each cycle: the high-water mark must come back from
		// the state file, not from memory.
		p := fixed()
		lease, err := p.Lease(ctx, "review-fixed")
		if err != nil {
			t.Fatalf("cycle %d lease: %v", cycle, err)
		}
		if seen[lease.LeaseID] {
			t.Fatalf("cycle %d reissued the retired lease identity %q under a fixed clock", cycle, lease.LeaseID)
		}
		seen[lease.LeaseID] = true

		// Every previously retired identity must be refused by the ORDINARY
		// path while this owner holds the slot.
		for _, old := range previous {
			if err := fixed().Release(ctx, old); err == nil {
				t.Fatalf("cycle %d: retired identity %q released the new owner through Release", cycle, old)
			}
		}
		// ...and by the EXACT path, which compares id AND generation.
		for _, old := range previous {
			if err := fixed().ReleaseExact(ctx, "pool-01", old, lease.LeasedAt.UnixNano(), f.slotPath); err == nil {
				t.Fatalf("cycle %d: retired identity %q released the new owner through ReleaseExact", cycle, old)
			}
		}
		if got := f.slot(t); got.LeaseID != lease.LeaseID {
			t.Fatalf("cycle %d: a retired identity disturbed the owner: %+v", cycle, got)
		}
		previous = append(previous, lease.LeaseID)

		if err := fixed().Release(ctx, lease.LeaseID); err != nil {
			t.Fatalf("cycle %d release: %v", cycle, err)
		}
	}
}

// The generation must also survive a slot being removed and recreated under
// the same name, or GC followed by Ensure resurrects a retired identity.
func TestARecreatedSlotDoesNotResurrectARetiredIdentity(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()
	frozen := time.Unix(0, 1_700_000_000_000_000_000).UTC()
	fixed := func() *Pool {
		p := f.pool()
		p.Now = func() time.Time { return frozen }
		return p
	}

	first, err := fixed().Lease(ctx, "review-recreate")
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := fixed().Release(ctx, first.LeaseID); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Remove the slot RECORD, as GC does, then let Ensure rebuild it.
	state, err := fixed().readState()
	if err != nil {
		t.Fatal(err)
	}
	state.Slots = nil
	if err := fixed().writeState(state); err != nil {
		t.Fatal(err)
	}
	owningGit(t, f.repo, "worktree", "remove", "--force", f.slotPath)
	rebuilt := fixed()
	if err := rebuilt.Ensure(ctx); err != nil {
		t.Fatalf("ensure after removal: %v", err)
	}

	second, err := fixed().Lease(ctx, "review-recreated")
	if err != nil {
		t.Fatalf("lease after recreation: %v", err)
	}
	if second.LeaseID == first.LeaseID {
		t.Fatalf("a recreated slot resurrected the retired identity %q", first.LeaseID)
	}
	if err := fixed().Release(ctx, first.LeaseID); err == nil {
		t.Fatal("the retired identity released the recreated slot's owner")
	}
}

// writeLegacyState rewrites the record in the version-1 shape that every pool
// created before this change still has on disk: no last_assigned_generation.
// This is TEST-OWNED state reconstructed to reach the legacy path, and it is
// the only thing these two tests corrupt.
func writeLegacyState(t *testing.T, f *owningFixture, slot PoolSlot) {
	t.Helper()
	legacy := struct {
		Version int        `json:"version"`
		Slots   []PoolSlot `json:"slots"`
	}{Version: 1, Slots: []PoolSlot{slot}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "last_assigned_generation") {
		t.Fatalf("the legacy fixture still carries the new field: %s", raw)
	}
	if err := os.WriteFile(filepath.Join(f.poolDir, "pool.json"), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// LEGACY STATE, released slot. The prior schema retained
// last_release_generation, so the high-water mark must be derived from it
// before any allocation runs. Without that, a fixed or rolled-back clock
// reissues exactly the retired identity and its holder can release the new
// owner.
func TestLegacyStateDoesNotReissueAReleasedGeneration(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()

	retiredGen := int64(1_700_000_000_000_000_000)
	retiredID := leaseIdentity("pool-01", time.Unix(0, retiredGen).UTC())
	writeLegacyState(t, f, PoolSlot{
		Name:                  "pool-01",
		Path:                  filepath.Join(".herd", "pool", "pool-01"),
		LastReleaseLeaseID:    retiredID,
		LastReleaseGeneration: retiredGen,
		LastReleasePath:       filepath.Join(".herd", "pool", "pool-01"),
	})

	// A clock rolled back BEHIND the retained generation is the worst case.
	rolledBack := func() *Pool {
		p := f.pool()
		p.Now = func() time.Time { return time.Unix(0, retiredGen-5).UTC() }
		return p
	}

	lease, err := rolledBack().Lease(ctx, "review-legacy")
	if err != nil {
		t.Fatalf("lease from legacy state: %v", err)
	}
	if lease.LeaseID == retiredID {
		t.Fatalf("reissued the retired lease identity from legacy state: %q", retiredID)
	}
	if lease.LeasedAt.UnixNano() <= retiredGen {
		t.Fatalf("legacy state issued generation %d, which is not above the retained %d", lease.LeasedAt.UnixNano(), retiredGen)
	}

	// The retired identity must be refused by BOTH release paths.
	if err := rolledBack().Release(ctx, retiredID); err == nil {
		t.Fatal("the retired legacy identity released the new owner through Release")
	}
	if err := rolledBack().ReleaseExact(ctx, "pool-01", retiredID, retiredGen, f.slotPath); err == nil {
		t.Fatal("the retired legacy identity released the new owner through ReleaseExact")
	}
	if got := f.slot(t); got.LeaseID != lease.LeaseID {
		t.Fatalf("a retired legacy identity disturbed the owner: %+v", got)
	}

	// The derived mark must be durable: a rebuilt Pool reading the record back
	// must not fall to zero again.
	if err := rolledBack().Release(ctx, lease.LeaseID); err != nil {
		t.Fatalf("release: %v", err)
	}
	next, err := rolledBack().Lease(ctx, "review-legacy-again")
	if err != nil {
		t.Fatalf("second lease: %v", err)
	}
	if next.LeaseID == lease.LeaseID || next.LeaseID == retiredID {
		t.Fatalf("the derived high-water mark did not persist: %q after %q", next.LeaseID, lease.LeaseID)
	}
}

// LEGACY STATE, still-held slot. Here the only retained evidence is the active
// LeasedAt, and dead-holder reclaim is what frees it. The mark must be derived
// from that active value at read time, before reclaim rewrites it.
func TestLegacyHeldSlotDoesNotReissueItsOwnGenerationAfterReclaim(t *testing.T) {
	f := newOwningFixture(t)
	ctx := context.Background()

	heldGen := int64(1_700_000_000_000_000_000)
	heldID := leaseIdentity("pool-01", time.Unix(0, heldGen).UTC())
	writeLegacyState(t, f, PoolSlot{
		Name:     "pool-01",
		Path:     filepath.Join(".herd", "pool", "pool-01"),
		Purpose:  "review-legacy-held",
		LeaseID:  heldID,
		LeasedAt: time.Unix(0, heldGen).UTC(),
	})

	fixed := func() *Pool {
		p := f.pool()
		p.Now = func() time.Time { return time.Unix(0, heldGen).UTC() }
		p.HolderLive = func(string) bool { return false }
		return p
	}

	freed, err := fixed().ReclaimDead(ctx)
	if err != nil {
		t.Fatalf("reclaim legacy held slot: %v", err)
	}
	if len(freed) != 1 {
		t.Fatalf("freed = %v, want the legacy slot", freed)
	}

	next, err := fixed().Lease(ctx, "review-after-reclaim")
	if err != nil {
		t.Fatalf("lease after reclaim: %v", err)
	}
	if next.LeaseID == heldID {
		t.Fatalf("reclaim reissued the legacy identity %q under a fixed clock", heldID)
	}
	if err := fixed().Release(ctx, heldID); err == nil {
		t.Fatal("the retired legacy identity released the slot reclaim handed on")
	}
}

// STRUCT LITERAL. A Pool assembled without NewPool has no construction moment
// to anchor a relative spelling against, and its first call may well be a READ.
// It must refuse rather than read ./pool.json from wherever the caller stands.
func TestStructLiteralPoolRefusesRelativeRootsOnItsFirstStateRead(t *testing.T) {
	f := newOwningFixture(t)

	// A complete decoy pool under the caller, so a refusal cannot be mistaken
	// for "there was nothing to read".
	decoyPool := filepath.Join(f.foreign, ".herd", "pool")
	if err := os.MkdirAll(decoyPool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyPool, "pool.json"),
		[]byte(`{"version":1,"slots":[{"name":"decoy-01","path":"decoy"}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(f.foreign)
	relative := &Pool{RepoRoot: ".", Root: filepath.Join(".herd", "pool")}
	slots, err := relative.Slots()
	if err == nil {
		t.Fatalf("a struct-literal pool with relative roots read state instead of refusing: %+v", slots)
	}
	if !strings.Contains(err.Error(), "must use absolute roots") {
		t.Fatalf("err = %v, want the explicit struct-literal refusal", err)
	}
	for _, s := range slots {
		if s.Name == "decoy-01" {
			t.Fatal("the caller's decoy state was returned")
		}
	}

	// The supported struct-literal spelling still works and reads the OWNING
	// pool, from the same foreign directory.
	absolute := &Pool{RepoRoot: f.repo, Root: f.poolDir}
	owned, err := absolute.Slots()
	if err != nil {
		t.Fatalf("an absolute struct-literal pool was refused: %v", err)
	}
	if len(owned) != 1 || owned[0].Name != "pool-01" {
		t.Fatalf("absolute struct-literal pool read the wrong state: %+v", owned)
	}
}
