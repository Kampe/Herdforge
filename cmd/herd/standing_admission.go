package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
	out, err := exec.Command("git", "-C", path, "rev-list", "--left-right", "--count", "HEAD...origin/main").Output()
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected git distance output %q", strings.TrimSpace(string(out)))
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse ahead %q: %w", fields[0], err)
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse behind %q: %w", fields[1], err)
	}
	return ahead, behind, nil
}
