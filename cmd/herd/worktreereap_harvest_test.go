package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/resources"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// harvestFixture builds the exact lifecycle the receipt exists to describe:
// a staging worktree on a temp branch carrying TWO commits, squash-merged
// onto main, main then advancing again, and the staging worktree detached at
// the merge commit by the installation step. Neither staged commit survives
// by object name and the detached head is an ancestor of main only until
// main advances -- which it does here.
type harvestFixture struct {
	root         string
	staging      string
	baseSHA      string
	candidateSHA string
	headSHA      string
	mergeSHA     string
	receipt      herdr.HarvestRetirementReceipt
	identity     string
}

func harvestFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newHarvestFixture(t *testing.T) harvestFixture {
	t.Helper()
	root := t.TempDir()
	f := harvestFixture{root: root, identity: "test-identity"}

	// Hermetic census population (FAC-215): the act-time owner census must
	// not depend on the host's process permissions. Runner daemons and other
	// same-owner processes with cleared dumpable make the kernel refuse the
	// private /proc metadata read, which fail-closes every retirement on
	// such hosts — the CI34615968238 failure. The fixture seals the census
	// to this test process; the per-pid reference probes stay real, so a
	// census regression still fails the retirement flow here.
	originalCensusInspector := reapCensusInspector
	reapCensusInspector = func() resources.ProcessInspector {
		pid := os.Getpid()
		return resources.NewSealedPopulationInspector([]int{pid}, map[int]int{pid: os.Getuid()})
	}
	t.Cleanup(func() { reapCensusInspector = originalCensusInspector })

	harvestFixtureGit(t, root, "init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o644)
	harvestFixtureGit(t, root, "add", ".")
	harvestFixtureGit(t, root, "commit", "-qm", "base")
	f.baseSHA = harvestFixtureGit(t, root, "rev-parse", "HEAD")

	// The producer's lifecycle: attached staging worktree on a temp branch,
	// two real commits, kept after success for the coordinator to push.
	f.staging = filepath.Join(root, ".herd", "worktrees", "harvest-stage")
	harvestFixtureGit(t, root, "worktree", "add", "-q", "-b", "temp/harvest-stage", f.staging)
	os.WriteFile(filepath.Join(f.staging, "one.txt"), []byte("first\n"), 0o644)
	harvestFixtureGit(t, f.staging, "add", ".")
	harvestFixtureGit(t, f.staging, "commit", "-qm", "first of two")
	os.WriteFile(filepath.Join(f.staging, "two.txt"), []byte("second\n"), 0o644)
	harvestFixtureGit(t, f.staging, "add", ".")
	harvestFixtureGit(t, f.staging, "commit", "-qm", "second of two")
	f.candidateSHA = harvestFixtureGit(t, f.staging, "rev-parse", "HEAD")
	f.headSHA = f.candidateSHA

	harvestFixtureGit(t, root, "merge", "--squash", "-q", "temp/harvest-stage")
	harvestFixtureGit(t, root, "commit", "-qm", "squash harvest-stage")
	f.mergeSHA = harvestFixtureGit(t, root, "rev-parse", "HEAD")
	harvestFixtureGit(t, root, "commit", "--allow-empty", "-qm", "trunk advances after harvest")

	// Installation detaches the staging surface at the merge commit.
	harvestFixtureGit(t, f.staging, "checkout", "-q", "--detach", f.mergeSHA)

	// The producer's binding: a live generation marker in the registration's
	// private admin dir, and a receipt bound to it, recorded with the same
	// identity the consumer will authenticate.
	marker, registrationID, err := herdr.MintHarvestGenerationMarker(f.staging, ".herd/worktrees/harvest-stage", time.Now())
	if err != nil {
		t.Fatalf("mint generation marker: %v", err)
	}
	f.receipt = herdr.NewHarvestRetirementReceipt(time.Now(), herdr.HarvestRetirementReceipt{
		Repository:     f.identity,
		Worktree:       ".herd/worktrees/harvest-stage",
		TempBranch:     "temp/harvest-stage",
		Lane:           "harvest-stage",
		BaseSHA:        f.baseSHA,
		CandidateSHA:   f.candidateSHA,
		HeadSHA:        f.headSHA,
		RegistrationID: registrationID,
		Generation:     marker.Generation,
	})
	registry := herdr.HarvestRetirementRegistry{Path: herdr.HarvestRetirementReceiptsPath(root)}
	if err := registry.Record(f.receipt); err != nil {
		t.Fatalf("record receipt: %v", err)
	}

	origIdentity, origAuthorizer := reapHarvestIdentity, reapHarvestAuthorizer
	reapHarvestIdentity = func(string) (string, error) { return f.identity, nil }
	reapHarvestAuthorizer = herdr.AuthorizeHarvestRetirement
	t.Cleanup(func() { reapHarvestIdentity, reapHarvestAuthorizer = origIdentity, origAuthorizer })
	return f
}

func TestHarvestHelpSurfaceDocumentsTargetAndByPR(t *testing.T) {
	usage := subcommandUsage["worktree-reap"]
	if !strings.Contains(usage, "--target") {
		t.Fatal("worktree-reap help omits the existing --target flag")
	}
	if !strings.Contains(usage, "repeatable") {
		t.Fatal("worktree-reap help does not say --target is repeatable")
	}
	if !strings.Contains(usage, "--by-pr") {
		t.Fatal("worktree-reap help omits the existing --by-pr flag")
	}
}

func TestUnreceiptedDetachedStaysWithThePool(t *testing.T) {
	f := newHarvestFixture(t)
	// A second detached surface with no marker and no receipt: a pool slot.
	pool := filepath.Join(f.root, ".herd", "worktrees", "pool-slot")
	harvestFixtureGit(t, f.root, "worktree", "add", "-q", "--detach", pool, f.mergeSHA)

	entries, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	landed, kept := classifyReapEntries(f.root, "main", false, entries)
	for _, l := range landed {
		if reapPulseSamePath(l.Path, pool) {
			t.Fatalf("unreceipted detached surfaces must never be classified removable: %v", landed)
		}
	}
	found := false
	for _, k := range kept {
		if reapPulseSamePath(k.Path, pool) {
			found = true
			if k.Class != "detached" {
				t.Fatalf("pool slot must keep its historical class, got %q", k.Class)
			}
		}
	}
	if !found {
		t.Fatalf("pool slot missing from kept set: %v", kept)
	}
}

func TestStaleReceiptCannotQualifyARecreatedSurface(t *testing.T) {
	f := newHarvestFixture(t)
	// Recreate the surface at the same path from the same base: a fresh
	// registration with a FRESH marker (the old receipt names the dead one).
	// The temp branch survives worktree removal, so the recreation is
	// detached, exactly like any later unrelated registration at that path.
	harvestFixtureGit(t, f.root, "worktree", "remove", f.staging)
	harvestFixtureGit(t, f.root, "worktree", "add", "-q", "--detach", f.staging, f.baseSHA)
	if _, _, err := herdr.MintHarvestGenerationMarker(f.staging, ".herd/worktrees/harvest-stage", time.Now()); err != nil {
		t.Fatal(err)
	}

	entries, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	landed, _ := classifyReapEntries(f.root, "main", false, entries)
	for _, l := range landed {
		if reapPulseSamePath(l.Path, f.staging) {
			t.Fatalf("a receipt bound to a dead generation must not retire the recreated surface: %v", landed)
		}
	}
}

func TestDirtyIgnoredContentHoldsAQualifiedHarvestSurface(t *testing.T) {
	f := newHarvestFixture(t)
	os.WriteFile(filepath.Join(f.staging, "evidence.dat"), []byte("generated\n"), 0o644)
	// .gitignore would hide it from ordinary status; the retirement boundary
	// must not be hidden by it.
	os.WriteFile(filepath.Join(f.staging, ".gitignore"), []byte("*.dat\n"), 0o644)

	entries, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	landed, kept := classifyReapEntries(f.root, "main", false, entries)
	for _, l := range landed {
		if reapPulseSamePath(l.Path, f.staging) {
			t.Fatalf("ignored content is content; the surface must be kept: %v", landed)
		}
	}
	keptForDirt := false
	for _, k := range kept {
		if reapPulseSamePath(k.Path, f.staging) && k.Class == "dirty" {
			keptForDirt = true
		}
	}
	if !keptForDirt {
		t.Fatalf("qualified harvest surface with ignored content must be kept as dirty: %v", kept)
	}
}

func TestUnlandedHarvestSurfaceIsKept(t *testing.T) {
	f := newHarvestFixture(t)
	// Same staging lifecycle, but the work never landed: reset main to the
	// base so the reviewed content is not on it.
	harvestFixtureGit(t, f.root, "reset", "-q", "--hard", f.baseSHA)
	harvestFixtureGit(t, f.staging, "checkout", "-q", "--detach", f.candidateSHA)

	entries, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	landed, kept := classifyReapEntries(f.root, "main", false, entries)
	keptUnlanded := false
	for _, k := range kept {
		if reapPulseSamePath(k.Path, f.staging) && k.Class == "unmerged" {
			keptUnlanded = true
		}
	}
	if len(landed) != 0 || !keptUnlanded {
		t.Fatalf("an unlanded harvest surface must be kept as unmerged, not classified removable: landed=%v kept=%v", landed, kept)
	}
}

func TestRevertedHarvestIsKeptAtTheCurrentTip(t *testing.T) {
	f := newHarvestFixture(t)
	// The landing happened, then trunk reverted it. The historical proof
	// still holds at the merge point; the CURRENT tip does not contain the
	// content, so removal would destroy the only copy.
	squash := f.mergeSHA
	harvestFixtureGit(t, f.root, "revert", "--no-edit", squash)

	entries, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	landed, kept := classifyReapEntries(f.root, "main", false, entries)
	keptUnmerged := false
	for _, k := range kept {
		if reapPulseSamePath(k.Path, f.staging) && k.Class == "unmerged" {
			keptUnmerged = true
		}
	}
	if len(landed) != 0 || !keptUnmerged {
		t.Fatalf("a reverted harvest must be kept as unmerged at the current tip: landed=%v kept=%v", landed, kept)
	}
}

func TestQualifiedDetachedHarvestRetiresWithReadback(t *testing.T) {
	f := newHarvestFixture(t)

	entries, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	landed, kept := classifyReapEntries(f.root, "main", false, entries)
	if len(landed) != 1 || !reapPulseSamePath(landed[0].Path, f.staging) {
		t.Fatalf("the qualified detached harvest is the one removable surface: landed=%v kept=%v", landed, kept)
	}
	if landed[0].harvest == nil {
		t.Fatal("a receipt-qualified detached row must carry its receipt into the act")
	}
	if landed[0].Class != "landed" {
		t.Fatalf("expected class landed, got %q (%s)", landed[0].Class, landed[0].Reason)
	}

	// Pin the marker location before the act so the post-removal assertion
	// observes a real location instead of a read that must fail through the
	// removed path regardless of whether the marker survived.
	pinnedDir, err := herdr.HarvestRegistrationDir(f.staging)
	if err != nil {
		t.Fatalf("resolve registration dir: %v", err)
	}
	pinnedMarker := filepath.Join(pinnedDir, herdr.HarvestGenerationMarkerFile)

	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	retired, failed := retireLandedWithInspector(f.root, landed, clean)
	if len(retired) != 1 || len(failed) != 0 {
		t.Fatalf("qualified detached harvest must retire cleanly: retired=%v failed=%v", retired, failed)
	}

	// Registration-set readback: the surface is gone from git's registration
	// set, and the generation marker died with the registration, so the
	// receipt can never authorize a later registration at the same path.
	if worktreeExists(f.staging) {
		t.Fatal("worktree survived harvest retirement")
	}
	after, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after {
		if filepath.Clean(e.Path) == filepath.Clean(f.staging) {
			t.Fatal("removed registration is still in git's registration set")
		}
	}
	if _, statErr := os.Lstat(pinnedMarker); !os.IsNotExist(statErr) {
		t.Fatalf("generation marker must die with its registration at the pinned location %s: %v", pinnedMarker, statErr)
	}
	// The journal is append-only evidence; retirement consumes nothing.
	all, err := (herdr.HarvestRetirementRegistry{Path: herdr.HarvestRetirementReceiptsPath(f.root)}).All()
	if err != nil || len(all) != 1 {
		t.Fatalf("receipt journal must survive retirement untouched: %v %v", all, err)
	}
}

// The post-removal marker readback must observe a PINNED private admin
// location, never re-resolve the registration through the removed worktree
// path: after a successful `git worktree remove` the path is gone, so any
// git-backed resolution through it fails unconditionally and a surviving
// marker would be certified as if it had died. This test performs the REAL
// removal through the native runner and then simulates the half-removal git's
// success can mask: the worktree is deleted, the registration is unlisted,
// but the private admin directory and its generation marker survive on disk.
// The act must refuse and name the survivor.
func TestActRefusesHarvestRetirementWhenGenerationMarkerSurvivesRemoval(t *testing.T) {
	f := newHarvestFixture(t)

	// Pin the exact admin directory and the marker bytes BEFORE the act, the
	// way the fence itself must.
	pinnedDir, err := herdr.HarvestRegistrationDir(f.staging)
	if err != nil {
		t.Fatalf("resolve registration dir: %v", err)
	}
	pinnedMarker := filepath.Join(pinnedDir, herdr.HarvestGenerationMarkerFile)
	originalMarker, err := os.ReadFile(pinnedMarker)
	if err != nil {
		t.Fatalf("read marker before the act: %v", err)
	}

	row := reapRow{Path: f.staging, Head: f.mergeSHA, Base: "main", harvest: &f.receipt}
	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})

	// The native runner stays native: every git call runs for real and its raw
	// result passes through untouched. Only after a real successful removal is
	// the surviving admin state restored on disk. The removal verb is found by
	// scanning, never at a fixed argv position.
	native := runReapGit
	run := func(root string, args ...string) ([]byte, error) {
		out, runErr := native(root, args...)
		if runErr == nil {
			for i := 1; i < len(args); i++ {
				if args[i] == "remove" && args[i-1] == "worktree" {
					if mkErr := os.MkdirAll(pinnedDir, 0o700); mkErr != nil {
						t.Errorf("simulate surviving admin dir: %v", mkErr)
					}
					if wErr := os.WriteFile(pinnedMarker, originalMarker, 0o600); wErr != nil {
						t.Errorf("simulate surviving marker: %v", wErr)
					}
					break
				}
			}
		}
		return out, runErr
	}

	err = retireLandedOneWithInspectorCensus(f.root, row, run, clean, nil)
	if err == nil {
		t.Fatal("a generation marker that survived a successful removal must refuse the retirement, not certify it")
	}
	if !strings.Contains(err.Error(), "marker survived") {
		t.Fatalf("refusal must name the surviving marker, got: %v", err)
	}

	// Fixture preconditions: the removal really happened and the registration
	// really is unlisted -- only the marker survived.
	if worktreeExists(f.staging) {
		t.Fatal("fixture precondition: the worktree removal did not happen")
	}
	after, listErr := reapRegistrationLister(f.root)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if _, still := exactWorktreeEntry(after, f.staging); still {
		t.Fatal("fixture precondition: the registration is still listed")
	}
	if _, statErr := os.Lstat(pinnedMarker); statErr != nil {
		t.Fatalf("fixture precondition: the marker did not survive: %v", statErr)
	}
}

func TestPulseAdmitsOnlyReceiptQualifiedDetached(t *testing.T) {
	f := newHarvestFixture(t)
	pool := filepath.Join(f.root, ".herd", "worktrees", "pool-slot")
	harvestFixtureGit(t, f.root, "worktree", "add", "-q", "--detach", pool, f.mergeSHA)

	registrations, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	candidates := reapHarvestCandidatesForBeat(f.root, registrations, context.Background())
	eligible := reapPulseEligible(f.root, registrations, "", candidates)
	var detachedEligible int
	for _, e := range eligible {
		if e.Detached {
			detachedEligible++
		}
	}
	if detachedEligible != 1 {
		t.Fatalf("exactly the receipt-qualified detached surface may enter the bounded selection: %d", detachedEligible)
	}

	// And the beat itself, with the pulse's own budget untouched, retires
	// exactly the qualified surface.
	report, err := runReapPulseTick(context.Background(), f.root, "main", true)
	if err != nil {
		t.Fatalf("pulse beat failed: %v", err)
	}
	if report.Retired != 1 || report.Failed != 0 {
		t.Fatalf("expected one retirement, got %+v", report)
	}
	if worktreeExists(f.staging) {
		t.Fatal("qualified detached harvest survived the beat")
	}
	if !worktreeExists(pool) {
		t.Fatal("the pool slot must never be touched by the reap beat")
	}
}

// TestPulseBeatSpendsNoQualificationOnUnrelatedPoolSlots pins the bounded
// qualification contract of the reap beat: repository identity and the
// receipt journal are bulk-loaded ONCE per beat, and every detached surface
// absent from that in-memory candidacy index is skipped without a single Git
// probe, marker read, status call, or authorizer invocation -- no matter how
// many unrelated pool slots the fleet holds. The pre-fix implementation
// qualified EVERY detached registration (one identity subprocess, one full
// journal re-read, and one marker resolution each), which made every
// 60-second beat a full-fleet Git scan on a fleet the size of ours.
func TestPulseBeatSpendsNoQualificationOnUnrelatedPoolSlots(t *testing.T) {
	f := newHarvestFixture(t)
	const unrelated = 12 // more than the whole 8-entry inspection window
	for i := 1; i <= unrelated; i++ {
		pool := filepath.Join(f.root, ".herd", "worktrees", fmt.Sprintf("pool-slot-%02d", i))
		harvestFixtureGit(t, f.root, "worktree", "add", "-q", "--detach", pool, f.mergeSHA)
	}

	origIdentity, origAuthorizer, origStatus := reapHarvestIdentity, reapHarvestAuthorizer, reapStatusRunner
	t.Cleanup(func() {
		reapHarvestIdentity, reapHarvestAuthorizer, reapStatusRunner = origIdentity, origAuthorizer, origStatus
	})

	identityCalls := 0
	reapHarvestIdentity = func(root string) (string, error) {
		identityCalls++
		return origIdentity(root)
	}
	authorizerProbes := map[string]int{}
	reapHarvestAuthorizer = func(req herdr.HarvestRetirementRequest) (herdr.HarvestRetirementReceipt, error) {
		authorizerProbes[req.WorktreePath]++
		return origAuthorizer(req)
	}
	statusProbes := map[string]int{}
	reapStatusRunner = func(dir string, args ...string) (string, error) {
		statusProbes[dir]++
		return origStatus(dir, args...)
	}

	report, err := runReapPulseTick(context.Background(), f.root, "main", true)
	if err != nil {
		t.Fatalf("pulse beat failed: %v", err)
	}
	if report.Retired != 1 || report.Failed != 0 {
		t.Fatalf("the receipt-qualified surface must still retire: %+v", report)
	}
	// Identity probes must be bounded by one bulk load per beat plus one
	// act-grade qualification per ELIGIBLE entry plus one rebind per act in
	// the retire budget -- never by the number of detached registrations the
	// fleet holds. The pre-fix code paid one identity subprocess PER DETACHED
	// PATH (15 here, for 13 detached surfaces) before the window was even cut.
	if max := 1 + report.Eligible + reapPulseRetireBudget; identityCalls > max {
		t.Fatalf("repository identity was probed %d times for one beat holding %d unrelated detached slots (eligible=%d, budget=%d); identity and journal must be bulk-loaded once and spent only inside the window", identityCalls, unrelated, report.Eligible, reapPulseRetireBudget)
	}
	for i := 1; i <= unrelated; i++ {
		pool := filepath.Join(f.root, ".herd", "worktrees", fmt.Sprintf("pool-slot-%02d", i))
		if n := statusProbes[pool]; n != 0 {
			t.Fatalf("unrelated pool slot %s was status-probed %d time(s); it must never enter the window", pool, n)
		}
	}
}

// writeHarvestPoolState writes a durable pool state file the way the pool
// adapters do, at a repo-relative pool root, so ownership fixtures use the
// exact record format the native adapters read.
func writeHarvestPoolState(t *testing.T, root, poolRel string, slots ...worktree.PoolSlot) {
	t.Helper()
	dir := filepath.Join(root, filepath.Clean(poolRel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(struct {
		Version int                 `json:"version"`
		Slots   []worktree.PoolSlot `json:"slots"`
	}{Version: 1, Slots: slots})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pool.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A generation receipt proves who CREATED a registration; it proves nothing
// about who holds the surface NOW. A durable review-pool slot lease whose
// record names the harvest path must hold the surface: kept by
// classification (as a keep, not a beat failure) and refused by the act
// fence, which re-runs the hold immediately before removal.
func TestActHoldsHarvestSurfaceBoundByDurablePoolLease(t *testing.T) {
	f := newHarvestFixture(t)
	writeHarvestPoolState(t, f.root, ".herd/pool", worktree.PoolSlot{
		Name: "pool-01", Path: f.staging, Purpose: "review", LeaseID: "pool-01-leased", LeasedAt: time.Now().UTC(),
	})

	entries, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	landed, kept := classifyReapEntries(f.root, "main", false, entries)
	for _, l := range landed {
		if reapPulseSamePath(l.Path, f.staging) {
			t.Fatalf("a durable pool lease must hold the surface before landing is even asked: %v", landed)
		}
	}
	held := false
	for _, k := range kept {
		if reapPulseSamePath(k.Path, f.staging) && k.Class == "held" {
			held = true
		}
	}
	if !held {
		t.Fatalf("leased harvest surface must be kept as held: %v", kept)
	}

	// And the act itself refuses, independent of classification evidence.
	row := reapRow{Path: f.staging, Head: f.mergeSHA, Base: "main", harvest: &f.receipt}
	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	retired, failed := retireLandedWithInspector(f.root, []reapRow{row}, clean)
	if len(retired) != 0 || len(failed) != 1 {
		t.Fatalf("act must refuse a durably leased surface: retired=%v failed=%v", retired, failed)
	}
	if !worktreeExists(f.staging) {
		t.Fatal("the durable lease must keep the surface on disk")
	}
	if !strings.Contains(failed[0]["error"], "lease") {
		t.Fatalf("the refusal must name the lease that holds the surface: %v", failed[0]["error"])
	}
}

// A pool slot record without a live lease is still the pool's OWN inventory,
// reclaimed only by the pool's evidence-fenced retirement authority. The
// reaper must not race it even when a harvest receipt names the same path.
func TestActHoldsHarvestSurfaceBoundByReleasedPoolInventory(t *testing.T) {
	f := newHarvestFixture(t)
	writeHarvestPoolState(t, f.root, ".herd/pool", worktree.PoolSlot{
		Name: "pool-01", Path: f.staging,
	})

	entries, err := reapRegistrationLister(f.root)
	if err != nil {
		t.Fatal(err)
	}
	landed, _ := classifyReapEntries(f.root, "main", false, entries)
	for _, l := range landed {
		if reapPulseSamePath(l.Path, f.staging) {
			t.Fatalf("pool inventory must hold the surface: %v", landed)
		}
	}
	row := reapRow{Path: f.staging, Head: f.mergeSHA, Base: "main", harvest: &f.receipt}
	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	retired, failed := retireLandedWithInspector(f.root, []reapRow{row}, clean)
	if len(retired) != 0 || len(failed) != 1 {
		t.Fatalf("act must refuse surface held as pool inventory: retired=%v failed=%v", retired, failed)
	}
	if !worktreeExists(f.staging) {
		t.Fatal("pool inventory must keep the surface on disk")
	}
	if !strings.Contains(failed[0]["error"], "inventory") {
		t.Fatalf("the refusal must name the pool inventory that holds the surface: %v", failed[0]["error"])
	}
}

// `herd review --pool-root` accepts any directory, so the durable pool roots
// that can hold a surface are not just the default one: every pool root a
// review retirement manifest ever named is consulted too.
func TestActHoldsHarvestSurfaceBoundByManifestNamedPoolLease(t *testing.T) {
	f := newHarvestFixture(t)
	writeHarvestPoolState(t, f.root, ".herd/pool-custom", worktree.PoolSlot{
		Name: "pool-01", Path: f.staging, Purpose: "review", LeaseID: "pool-01-leased", LeasedAt: time.Now().UTC(),
	})
	sha := strings.Repeat("a", 40)
	manifest := herdr.NewReviewRetirementManifest(time.Now(), herdr.ReviewRetirementManifest{
		Repository: f.identity, TaskRef: "FAC-X", TaskID: "task-x", CandidateSHA: sha, BaseSHA: sha,
		Branch: "review/x", Worktree: ".herd/pool-custom/pool-01", Pool: ".herd/pool-custom", Slot: "pool-01",
		Workspace: "ws", TabID: "t1", PaneID: "p1", TerminalID: "term1", SessionID: "s1",
		Reviewer: "r", ReviewerFamily: "fam", ReviewerModel: "m", PromptArtifact: ".herd/prompt.md",
		Generation: "1", Nonce: "nonce-1", LeaseGeneration: 7,
	})
	if err := (herdr.ReviewRetirementRegistry{Path: herdr.ReviewRetirementRegistryPath(f.root)}).Record(manifest); err != nil {
		t.Fatalf("record review retirement manifest: %v", err)
	}

	row := reapRow{Path: f.staging, Head: f.mergeSHA, Base: "main", harvest: &f.receipt}
	clean := reapProcessInspectorFunc(func(context.Context, string) (resources.ProcessUsage, error) {
		return resources.ProcessUsage{}, nil
	})
	retired, failed := retireLandedWithInspector(f.root, []reapRow{row}, clean)
	if len(retired) != 0 || len(failed) != 1 {
		t.Fatalf("act must refuse a surface held by a manifest-named pool lease: retired=%v failed=%v", retired, failed)
	}
	if !worktreeExists(f.staging) {
		t.Fatal("the manifest-named pool lease must keep the surface on disk")
	}
	if !strings.Contains(failed[0]["error"], "lease") {
		t.Fatalf("the refusal must name the lease that holds the surface: %v", failed[0]["error"])
	}
}

// A durable hold is a KEEP, not a beat failure: a scheduled beat that runs
// while a review holds the surface must stay healthy and simply retire
// nothing, the same disposition any other protected surface gets.
func TestPulseKeepsPoolLeasedHarvestSurfaceWithoutFailing(t *testing.T) {
	f := newHarvestFixture(t)
	writeHarvestPoolState(t, f.root, ".herd/pool", worktree.PoolSlot{
		Name: "pool-01", Path: f.staging, Purpose: "review", LeaseID: "pool-01-leased", LeasedAt: time.Now().UTC(),
	})

	report, err := runReapPulseTick(context.Background(), f.root, "main", true)
	if err != nil {
		t.Fatalf("pulse beat failed: %v", err)
	}
	if report.Retired != 0 || report.Failed != 0 {
		t.Fatalf("a durable hold must be a keep, not a beat failure: %+v", report)
	}
	if !worktreeExists(f.staging) {
		t.Fatal("the leased surface must survive the beat")
	}
}
