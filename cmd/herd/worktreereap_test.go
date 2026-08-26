package main

import "testing"

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
