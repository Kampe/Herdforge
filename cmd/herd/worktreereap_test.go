package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// FAC-672: a standing lane's RESIDENT HOME tracks the base and therefore has no
// unique commits, which makes it look exactly like a landed task worktree.
// Caught in dry run before any --apply: the coordinator's own home
// (standing/orchestrator) was classified removable. A reaper that takes out the
// coordinator is worse than one that reclaims nothing, and "no unique commits"
// is precisely the signal that cannot tell the two apart on its own.
func TestResidentHomesAreNeverClassifiedRemovable(t *testing.T) {
	for _, c := range []struct{ branch, path string }{
		{"standing/orchestrator", "/x/chainseer-orchestrator"},
		{"standing/docs-custodian", "/x/wt/docs"},
		{"main", "/x/repo"},
		{"master", "/x/repo"},
		{"feat/thing", "/x/review-harvest-supervisor"},
		{"feat/thing", "/x/herd-smith"},
	} {
		if !isResidentHome(c.branch, c.path) {
			t.Errorf("branch=%q path=%q must be protected as a resident home", c.branch, c.path)
		}
	}
}

// An ordinary task worktree must remain reapable, or the leak this exists to
// close simply continues.
func TestOrdinaryTaskWorktreesRemainReapable(t *testing.T) {
	for _, c := range []struct{ branch, path string }{
		{"fix/cha-2086-gating-finality", "/x/.herd/worktrees/cha-2086"},
		{"harvest/cha-2078", "/x/.herd/worktrees/harvest-cha-2078"},
		{"feat/whatever", "/x/worktrees/whatever"},
	} {
		if isResidentHome(c.branch, c.path) {
			t.Errorf("branch=%q path=%q is a task worktree and must stay reapable", c.branch, c.path)
		}
	}
}

// commitsAhead must never report an unanswerable comparison as zero: zero means
// "nothing unique here", which is the signal that authorises removal.
func TestUnanswerableComparisonIsNotZero(t *testing.T) {
	got := commitsAhead(t.TempDir(), "origin/main", "no-such-branch")
	if got == 0 {
		t.Fatal("a failed comparison must not read as 'no unique commits'; that would authorise deleting unmerged work")
	}
	if got != -1 {
		t.Errorf("expected -1 for unknown, got %d", got)
	}
}

// FAC-673: retiring on PR closure ALONE would destroy abandoned work. A closed
// PR whose patch never landed is not finished work, and its worktree may hold
// the only copy. Both halves are required: closed AND verifiably landed.
func TestPatchIsInBaseUsesIdentityNotAncestry(t *testing.T) {
	root := t.TempDir()
	run := func(a ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", root}, a...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", a, err, out)
		}
	}
	run("init", "-q", "-b", "main", ".")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("base\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "base")

	// A branch whose change is NOT in main: must not read as landed, or the
	// reaper would delete the only copy of unmerged work.
	run("checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("work\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "feature work")
	run("checkout", "-q", "main")
	if patchIsInBase(root, "feature", "main") {
		t.Fatal("unlanded work must NEVER report as in base; that authorises deleting it")
	}

	// Cherry-pick it onto main: a DIFFERENT sha, same patch. Ancestry says no,
	// patch identity says yes -- and patch identity is the truthful answer.
	run("cherry-pick", "feature")
	// Whether ancestry still reports the branch as ahead depends on how git
	// collapses the graph, and that is incidental. The claim under test is the
	// behaviour: a landing that changed the SHA must still be RECOGNISED as
	// landed, so the reaper does not strand work that genuinely merged.
	if !patchIsInBase(root, "feature", "main") {
		t.Error("a rebased or cherry-picked landing must be recognised by patch identity")
	}
}

// With no recorded PR this is simply not the function's business, and it must
// say so rather than declining with a reason that reads like a problem.
func TestPRClosureCheckIsSilentWithoutARecordedPR(t *testing.T) {
	ok, why := prClosedAndLanded(t.TempDir(), "feature", "main")
	if ok {
		t.Fatal("no recorded PR cannot authorise retirement")
	}
	if why != "" {
		t.Errorf("absence of a PR is not a decline reason, got %q", why)
	}
}

// FAC-676: the contract is one standing lane = one mutable task worktree plus
// its resident home. Nothing enforced or even measured it, so lanes accumulated
// surfaces silently: 10 lanes over-allocated on the live repository, with
// perf-cost-guard holding 15. That is the accumulation MECHANISM, distinct from
// the retirement leak -- fixing only retirement means sweeping forever.
func TestLaneAllocationNamesOverAllocatedLanes(t *testing.T) {
	lanes := []string{"docs-custodian", "qa-sentinel"}
	entries := []worktreeEntry{
		{Path: "/x/wt/docs-custodian", Branch: "standing/docs-custodian"}, // home: entitled
		{Path: "/x/a", Branch: "harvest/docs-custodian-1"},
		{Path: "/x/b", Branch: "harvest/docs-custodian-2"},
		{Path: "/x/c", Branch: "fix/qa-sentinel-only-one"},
	}
	got := laneAllocations(entries, lanes)
	if len(got) != 1 {
		t.Fatalf("only the over-allocated lane must be reported, got %+v", got)
	}
	if got[0].Lane != "docs-custodian" {
		t.Errorf("wrong lane: %q", got[0].Lane)
	}
	// The home is entitled and must NOT count, or every healthy lane reads as
	// over-allocated by one.
	if len(got[0].TaskPaths) != 2 || got[0].Excess != 1 {
		t.Errorf("home must be excluded from the task count: %+v", got[0])
	}
	if got[0].Home == "" {
		t.Error("the home should still be identified, just not counted")
	}
}

// A lane at exactly one task worktree is meeting the contract, not violating it.
func TestLaneAtTheContractIsNotReported(t *testing.T) {
	got := laneAllocations([]worktreeEntry{
		{Path: "/x/home", Branch: "standing/qa-sentinel"},
		{Path: "/x/one", Branch: "fix/qa-sentinel-task"},
	}, []string{"qa-sentinel"})
	if len(got) != 0 {
		t.Fatalf("one task worktree plus a home is the contract: %+v", got)
	}
}

// A lane whose name prefixes another must not claim the other's surfaces --
// the same boundary rule agent identity needed in FAC-660.
func TestLaneAttributionPrefersTheLongestLaneName(t *testing.T) {
	lanes := []string{"review", "review-harvest"}
	if got := laneOwning("fix/review-harvest-thing", "/x/p", lanes); got != "review-harvest" {
		t.Errorf("longest lane must win, got %q", got)
	}
	if got := laneOwning("fix/review-thing", "/x/p", lanes); got != "review" {
		t.Errorf("short lane must still match its own, got %q", got)
	}
	if got := laneOwning("fix/unrelated", "/x/p", lanes); got != "" {
		t.Errorf("an unowned surface must be claimed by nobody, got %q", got)
	}
}
