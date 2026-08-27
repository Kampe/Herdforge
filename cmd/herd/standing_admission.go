package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/preflight"
)

// readmitStandingWorktree refreshes an existing lane only when doing so cannot
// discard lane work. A clean branch with no unique commits may fast-forward;
// every other stale state refuses with the measured origin/main distance.
func readmitStandingWorktree(lane, path string) error {
	if out, err := exec.Command("git", "-C", path, "fetch", "-q", "origin", "main").CombinedOutput(); err != nil {
		return fmt.Errorf("re-admit standing worktree %s: fetch origin/main: %v: %s", lane, err, strings.TrimSpace(string(out)))
	}
	ahead, behind, err := standingWorktreeDistance(path)
	if err != nil {
		return fmt.Errorf("re-admit standing worktree %s: measure origin/main distance: %w", lane, err)
	}
	if out, err := exec.Command("git", "-C", path, "status", "--porcelain", "--untracked-files=all").Output(); err != nil {
		return fmt.Errorf("re-admit standing worktree %s: inspect worktree status: %w", lane, err)
	} else if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("re-admit standing worktree %s refused: dirty worktree (ahead=%d behind=%d versus origin/main)", lane, ahead, behind)
	}
	if ahead != 0 {
		return fmt.Errorf("re-admit standing worktree %s refused: unmerged lane commits (ahead=%d behind=%d versus origin/main)", lane, ahead, behind)
	}
	if behind == 0 {
		return nil
	}
	if out, err := exec.Command("git", "-C", path, "merge", "--ff-only", "origin/main").CombinedOutput(); err != nil {
		return fmt.Errorf("re-admit standing worktree %s refused: fast-forward origin/main (ahead=%d behind=%d): %v: %s", lane, ahead, behind, err, strings.TrimSpace(string(out)))
	}
	ahead, behind, err = standingWorktreeDistance(path)
	if err != nil {
		return fmt.Errorf("re-admit standing worktree %s: verify origin/main distance: %w", lane, err)
	}
	if ahead != 0 || behind != 0 {
		return fmt.Errorf("re-admit standing worktree %s refused: refresh left lane at ahead=%d behind=%d versus origin/main", lane, ahead, behind)
	}
	return nil
}

func standingWorktreeDistance(path string) (ahead, behind int, err error) {
	return preflight.RefDistance(path, "HEAD", "origin/main")
}
