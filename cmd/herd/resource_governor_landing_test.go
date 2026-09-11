package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/resources"
)

func landingRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	landingGit(t, root, "init", "-q", "-b", "main", ".")
	landingWrite(t, root, "base.txt", "base\n")
	landingGit(t, root, "add", ".")
	landingGit(t, root, "commit", "-qm", "base")
	return root
}

func landingGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := testgit.Command(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func landingWrite(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func landingSHA(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(landingGit(t, dir, "rev-parse", rev))
}

// landingSquashedLane builds the shape the governor must recognize: a lane
// with TWO commits, squashed onto main as one commit, after which main
// advances again. Neither commit survives by object name, and the lane tip is
// not an ancestor of the base.
func landingSquashedLane(t *testing.T, root, name string) (lane, head string) {
	t.Helper()
	lane = filepath.Join(root, name)
	landingGit(t, root, "worktree", "add", "-q", "-b", name, lane)
	landingWrite(t, lane, name+"-a.txt", "first\n")
	landingGit(t, lane, "add", ".")
	landingGit(t, lane, "commit", "-qm", "first of two")
	landingWrite(t, lane, name+"-b.txt", "second\n")
	landingGit(t, lane, "add", ".")
	landingGit(t, lane, "commit", "-qm", "second of two")
	head = landingSHA(t, lane, "HEAD")

	landingGit(t, root, "merge", "--squash", "-q", name)
	landingGit(t, root, "commit", "-qm", "squash "+name)
	// Main advances AFTER the squash: the proof must survive a base that moved
	// on, which is the normal state of a busy trunk.
	landingWrite(t, root, name+"-later.txt", "later trunk work\n")
	landingGit(t, root, "add", ".")
	landingGit(t, root, "commit", "-qm", "trunk advances after "+name)
	return lane, head
}

func landingProbe(t *testing.T, root, lane, head string) resources.LandingProbe {
	t.Helper()
	return resources.LandingProbe{
		WorktreePath: lane,
		Branch:       filepath.Base(lane),
		HeadSHA:      head,
		BaseRef:      "main",
		BaseSHA:      landingSHA(t, root, "main"),
	}
}

// The positive case: two commits squashed onto a base that then advanced are
// LANDED, even though ancestry says otherwise. This is the whole feature.
func TestGovernorLandingProvesSquashedRangeAfterBaseAdvances(t *testing.T) {
	root := landingRepo(t)
	lane, head := landingSquashedLane(t, root, "squashed-lane")

	// The defect this replaces, asserted directly: ancestry alone still says
	// unmerged here, so a test that passed without the predicate would be
	// proving nothing.
	if ancestor := testgit.Command(root, "merge-base", "--is-ancestor", head, "main").Run(); ancestor == nil {
		t.Fatal("fixture is vacuous: the squashed lane tip is still an ancestor of main")
	}

	landed, err := governorLandingPredicate(root)(context.Background(), landingProbe(t, root, lane, head))
	if err != nil {
		t.Fatalf("squash-landed lane must not be unknown: %v", err)
	}
	if !landed {
		t.Fatal("a two-commit range squashed onto an advanced base was not recognized as landed")
	}
}

// A lane whose work was reverted is NOT landed: its content is no longer on
// the base, so it may hold the only copy and keeps its artifacts.
func TestGovernorLandingRefusesRevertedRange(t *testing.T) {
	root := landingRepo(t)
	lane, head := landingSquashedLane(t, root, "reverted-lane")
	squash := landingSHA(t, root, "HEAD~1")
	landingGit(t, root, "revert", "--no-edit", squash)

	landed, err := governorLandingPredicate(root)(context.Background(), landingProbe(t, root, lane, head))
	if err != nil {
		t.Fatalf("a reverted range is answerable, so it must not be unknown: %v", err)
	}
	if landed {
		t.Fatal("a reverted range was reported landed; its artifacts would be reclaimed while it holds the only copy")
	}
}

// A lane holding unique work beyond what landed is NOT landed, even though
// part of its range is on the base.
func TestGovernorLandingRefusesPartiallyLandedRange(t *testing.T) {
	root := landingRepo(t)
	lane, _ := landingSquashedLane(t, root, "partial-lane")
	landingWrite(t, lane, "unique.txt", "only copy of this\n")
	landingGit(t, lane, "add", ".")
	landingGit(t, lane, "commit", "-qm", "unique unmerged delta")
	head := landingSHA(t, lane, "HEAD")

	landed, err := governorLandingPredicate(root)(context.Background(), landingProbe(t, root, lane, head))
	if err != nil {
		t.Fatalf("a partially landed range is answerable: %v", err)
	}
	if landed {
		t.Fatal("a lane with a unique unmerged delta was reported landed")
	}
}

// Ancestry remains the cheap authoritative yes.
func TestGovernorLandingAcceptsPlainAncestor(t *testing.T) {
	root := landingRepo(t)
	lane := filepath.Join(root, "ancestor-lane")
	landingGit(t, root, "worktree", "add", "-q", "-b", "ancestor-lane", lane)
	head := landingSHA(t, lane, "HEAD")

	landed, err := governorLandingPredicate(root)(context.Background(), landingProbe(t, root, lane, head))
	if err != nil || !landed {
		t.Fatalf("a lane adding nothing over the base is landed: landed=%v err=%v", landed, err)
	}
}

// Unanswerable is UNKNOWN, never "not landed", and never "landed".
func TestGovernorLandingUnreadableIdentityIsUnknown(t *testing.T) {
	root := landingRepo(t)
	probe := landingProbe(t, root, filepath.Join(root, "missing"), strings.Repeat("0", 40))

	landed, err := governorLandingPredicate(root)(context.Background(), probe)
	if err == nil {
		t.Fatal("an unreadable head must be reported unknown, not silently unmerged")
	}
	if landed {
		t.Fatal("an unreadable head must never be reported landed")
	}

	empty := resources.LandingProbe{WorktreePath: root, Branch: "main", BaseRef: "main"}
	if landed, err := governorLandingPredicate(root)(context.Background(), empty); err == nil || landed {
		t.Fatalf("an unpinned probe must refuse: landed=%v err=%v", landed, err)
	}
}

// A cancelled census does not start a proof.
func TestGovernorLandingCancelledContextRefusesBeforeProving(t *testing.T) {
	root := landingRepo(t)
	lane, head := landingSquashedLane(t, root, "cancelled-lane")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	landed, err := governorLandingPredicate(root)(ctx, landingProbe(t, root, lane, head))
	if err == nil || landed {
		t.Fatalf("a cancelled probe must refuse: landed=%v err=%v", landed, err)
	}
}
