package verifier

import (
	"context"
	"os/exec"
	"strings"
)

// FAC-98 / FAC-116: the worktree completion gate. An agent's work is only
// "done" when its worktree carries REAL committed work ahead of origin/main
// (not just the anchor/wip commits), builds, and tests pass. This is the gate
// that catches the whiff-and-stall pattern — an agent reporting done with zero
// commits, or code that does not build — before it ever reaches review.

// CompletionCheck is the verdict for one worktree.
type CompletionCheck struct {
	Passed     bool
	HasCommits bool
	Builds     bool
	TestsPass  bool
	Reasons    []string // human-readable failures, each carrying the fix
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// hasRealCommits reports whether the worktree branch has commits ahead of
// origin/main whose subjects are not just the reap-safe anchor or a wip
// checkpoint — i.e. the agent actually produced work.
func hasRealCommits(worktree string) bool {
	out, err := gitOut(worktree, "log", "--format=%s", "origin/main..HEAD")
	if err != nil || out == "" {
		return false
	}
	for _, subj := range strings.Split(out, "\n") {
		s := strings.TrimSpace(subj)
		if s == "" {
			continue
		}
		low := strings.ToLower(s)
		if strings.HasPrefix(low, "chore: anchor") || strings.HasPrefix(low, "wip:") {
			continue
		}
		return true
	}
	return false
}

// CheckCompletion runs the full gate against a worktree: real commits, build,
// and tests. buildCmd/testCmd are the shell commands (e.g. "go build ./...",
// "go test ./..."); an empty command skips that stage as passed.
func (v *Verifier) CheckCompletion(ctx context.Context, worktree, buildCmd, testCmd string) *CompletionCheck {
	c := &CompletionCheck{}

	c.HasCommits = hasRealCommits(worktree)
	if !c.HasCommits {
		c.Reasons = append(c.Reasons,
			"no real commits ahead of origin/main (only anchor/wip) — the agent did not produce work; re-dispatch or finish it")
	}

	c.Builds = runShell(ctx, worktree, buildCmd)
	if !c.Builds {
		c.Reasons = append(c.Reasons, "build failed ("+buildCmd+") — fix compile errors before this can complete")
	}

	c.TestsPass = runShell(ctx, worktree, testCmd)
	if !c.TestsPass {
		c.Reasons = append(c.Reasons, "tests failed ("+testCmd+") — fix the failing tests before this can complete")
	}

	c.Passed = c.HasCommits && c.Builds && c.TestsPass
	return c
}

// runShell runs a non-shell argv string in dir and reports success. Empty or
// malformed commands fail closed instead of silently skipping a gate.
func runShell(ctx context.Context, dir, command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	argv, err := parseArgv(command)
	if err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	return cmd.Run() == nil
}
