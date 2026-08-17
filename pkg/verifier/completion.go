package verifier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/procsignal"
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

// CompletionReceipts contains the durable evidence produced by the build and
// test legs of a managed completion check. The receipts are kept separate so
// review admission can identify the exact command evidence it is consuming.
type CompletionReceipts struct {
	Build *Receipt
	Test  *Receipt
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := runGit(dir, args...)
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
// and tests. buildCmd/testCmd are non-shell command strings (e.g. "go build
// ./...", "go test ./..."); empty or malformed commands fail closed.
// ctx must be non-nil; a nil context fails the build/test gates closed.
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

// CheckCompletionAndPersist runs the same build/test completion gate while
// persisting every terminal command outcome. Managed worktrees must use this
// path: a callback without a receipt cannot be admitted for exact-SHA review.
// Commands are parsed as argv and never passed through a shell.
func (v *Verifier) CheckCompletionAndPersist(ctx context.Context, worktree, buildCmd, testCmd string, req VerificationRequest, store ReceiptStore) (*CompletionCheck, *CompletionReceipts, error) {
	if store == nil {
		return nil, nil, errors.New("completion receipts: receipt store is required")
	}
	c := &CompletionCheck{}
	c.HasCommits = hasRealCommits(worktree)
	if !c.HasCommits {
		c.Reasons = append(c.Reasons,
			"no real commits ahead of origin/main (only anchor/wip) — the agent did not produce work; re-dispatch or finish it")
	}
	receipts := &CompletionReceipts{}
	buildReceipt, err := NewVerifier(buildCmd).VerifyAndPersist(ctx, worktree, req, store)
	if err != nil {
		return nil, nil, fmt.Errorf("persist build verification: %w", err)
	}
	receipts.Build = buildReceipt
	c.Builds = buildReceipt.Outcome == OutcomePASS
	if !c.Builds {
		c.Reasons = append(c.Reasons, "build failed ("+buildCmd+") — fix compile errors before this can complete")
	}
	testReceipt, err := NewVerifier(testCmd).VerifyAndPersist(ctx, worktree, req, store)
	if err != nil {
		return nil, nil, fmt.Errorf("persist test verification: %w", err)
	}
	receipts.Test = testReceipt
	c.TestsPass = testReceipt.Outcome == OutcomePASS
	if !c.TestsPass {
		c.Reasons = append(c.Reasons, "tests failed ("+testCmd+") — fix the failing tests before this can complete")
	}
	c.Passed = c.HasCommits && c.Builds && c.TestsPass
	return c, receipts, nil
}

// runShell runs a non-shell argv string in dir and reports success. Empty or
// malformed commands fail closed instead of silently skipping a gate.
//
// Cancellation (FAC-192): same owned process-group authority as execute
// (FAC-174 procsignal). Setpgid owns the tree; Cancel claims the live leader
// and tears down the group via the opaque procsignal entrypoint — never a raw
// unclaimed kill(-pgid) and never leader-only Process.Kill that leaves
// grandchildren (FAC-150 hang class).
func runShell(ctx context.Context, dir, command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	argv, err := parseArgv(command)
	if err != nil {
		return false
	}
	if ctx == nil {
		// Fail closed: never spawn an uncancellable child.
		return false
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	// Completion verification must not inherit host Git signing, hooks, or
	// config. A worker/coordinator gate is about the candidate's build and
	// tests, not availability of an operator's credential agent.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return procsignal.CancelSpawnedProcess(cmd.Process)
	}
	// Bound pipe drain after cancel so a grandchild holding stdout cannot
	// stall Wait indefinitely.
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.Run() == nil
}
