package harvest

import (
	"context"
	"os/exec"
	"strings"
)

// ContentMerged reports whether a commit's patch is already represented by
// the integration ref. It is intentionally based on git's cherry-equivalence
// logic, not reachability, because rebase merges create new object IDs.
func ContentMerged(ctx context.Context, repoRoot, mainRef, sha string) (bool, error) {
	if mainRef == "" {
		mainRef = "origin/main"
	}
	cmd := exec.CommandContext(ctx, "git", "log", "--cherry-pick", "--right-only", "--no-merges", mainRef+"..."+sha, "--oneline")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "", nil
}

// UnmergedFor is the ONE authoritative single-target "does this worktree
// have genuinely unmerged work" check (port of bin/herd-unmerged). Uses
// git cherry (patch-equivalence against origin/main), never rev-list
// --count.
//
// Why: this repo merges via rebase, replaying each commit onto main with a
// NEW sha; rev-list --count origin/main..branch only compares reachability,
// so an already-merged commit still counts "ahead" — a coordinator reap
// directive built on rev-list held and re-churned already-merged branches
// (2026-07-21 incident). git cherry compares patch content, so it correctly
// reports 0 unique commits once the patch is upstream, however it got there.
//
// Semantics: rev-parse failure → error (not a worktree); main/master/HEAD →
// (nil, nil); transient fetch failure ignored; git cherry failure → (nil,
// nil) matching the zsh `|| true`; no unique commits → (nil, nil).
func (h *Harvester) UnmergedFor(ctx context.Context, worktreePath string) (*UnmergedWork, error) {
	return h.checkUnmerged(ctx, worktreePath)
}
