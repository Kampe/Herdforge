package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// FAC-632: eviction decides ownership by the pane's foreground CWD, and the
// containment check must not match a sibling directory by prefix. pool-1 must
// never be treated as living inside pool-10 or vice versa.
func TestPoolSlotContainment_DoesNotMatchSiblingByPrefix(t *testing.T) {
	slot := "/repo/.herd/pool/pool-1"
	abs, err := filepath.Abs(slot)
	if err != nil {
		t.Fatal(err)
	}
	inside := func(cwd string) bool {
		resolved, e := filepath.Abs(cwd)
		if e != nil {
			return false
		}
		return resolved == abs || strings.HasPrefix(resolved, abs+string(filepath.Separator))
	}

	if !inside("/repo/.herd/pool/pool-1") {
		t.Error("the slot itself must count as inside")
	}
	if !inside("/repo/.herd/pool/pool-1/apps/web") {
		t.Error("a subdirectory must count as inside")
	}
	if inside("/repo/.herd/pool/pool-10") {
		t.Fatal("pool-10 must NOT be treated as inside pool-1; a bare prefix check would evict the wrong reviewer")
	}
	if inside("/repo/.herd/pool/pool-2") {
		t.Error("an unrelated slot must not match")
	}
	if inside("/repo/.herd/worktrees/cha-1") {
		t.Error("a non-pool worktree must not match")
	}
}

// The name the dispatch is about to use must be spared, so re-dispatching the
// same candidate into its own slot is idempotent rather than self-destructive.
func TestAgentNameFor_MatchesReviewAgentName(t *testing.T) {
	ref, sha := "CHA-1", strings.Repeat("a", 40)
	if agentNameFor(ref, sha) != reviewAgentName(ref, sha) {
		t.Fatal("eviction must spare exactly the name this dispatch will create; a mismatch makes a re-dispatch kill itself")
	}
}
