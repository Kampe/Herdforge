// Package resetsafe provides the fail-closed preserve-and-reset operation for
// feature worktrees. It deliberately has no worktree removal or pruning path.
package resetsafe

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// Options controls reset-safe's diagnostic output. Nil writers use the
// process stdout/stderr, which preserves the CLI's script-compatible output.
type Options struct {
	Stdout io.Writer
	Stderr io.Writer
}

func (o Options) stdout() io.Writer {
	if o.Stdout != nil {
		return o.Stdout
	}
	return os.Stdout
}

func (o Options) stderr() io.Writer {
	if o.Stderr != nil {
		return o.Stderr
	}
	return os.Stderr
}

// WorktreePlan is the checked plan and, after Run, the result of the reset.
type WorktreePlan struct {
	Worktree       string
	Branch         string
	ShortSHA       string
	Unique         []string
	PreserveBranch string
	Pushed         bool
	ResetSHA       string

	repoRoot string
	opts     Options
}

// Open verifies that worktree names an existing directory.
func Open(worktree string) error {
	info, err := os.Stat(worktree)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("herd-reset-safe: %s does not exist", worktree)
		}
		return fmt.Errorf("herd-reset-safe: cannot inspect %s: %w", worktree, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("herd-reset-safe: %s does not exist", worktree)
	}
	return nil
}

// NewWorktree performs the local worktree gates that do not inspect the
// repository's patch history.
func NewWorktree(ctx context.Context, worktree string) (string, string, error) {
	branch, err := gitOutput(ctx, worktree, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("herd-reset-safe: %s is not a git worktree", worktree)
	}
	branch = strings.TrimSpace(branch)
	if branch == "main" || branch == "master" {
		return "", "", fmt.Errorf("herd-reset-safe: refusing on '%s' — this is for feature-branch worktrees, never the shared main checkout", branch)
	}
	short, err := gitOutput(ctx, worktree, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("herd-reset-safe: %s has no readable HEAD: %w", worktree, err)
	}
	return branch, strings.TrimSpace(short), nil
}

// New runs the usage-independent safety gates and creates an immutable plan.
// It does not create refs or reset anything.
func New(ctx context.Context, repoRoot, worktreePath string, opts Options) (*WorktreePlan, error) {
	if err := Open(worktreePath); err != nil {
		return nil, err
	}
	branch, short, err := NewWorktree(ctx, worktreePath)
	if err != nil {
		return nil, err
	}
	if dirty, err := dirtyFiles(ctx, worktreePath); err != nil {
		return nil, fmt.Errorf("herd-reset-safe: cannot inspect uncommitted changes: %w", err)
	} else if len(dirty) > 0 {
		return nil, fmt.Errorf("herd-reset-safe: %s has uncommitted changes, refusing:\n  %s\nherd-reset-safe: commit or stash first, then re-run", worktreePath, strings.Join(dirty, "\n  "))
	}

	u, err := harvest.NewHarvester(repoRoot).UnmergedFor(ctx, worktreePath)
	if err != nil {
		return nil, fmt.Errorf("herd-reset-safe: cannot verify unmerged work: %w", err)
	}
	plan := &WorktreePlan{Worktree: worktreePath, Branch: branch, ShortSHA: short, repoRoot: repoRoot, opts: opts}
	if u != nil && len(u.Unmerged) > 0 {
		plan.Unique = append([]string(nil), u.Unmerged...)
		plan.PreserveBranch = "harvest/" + strings.ReplaceAll(branch, "/", "-") + "-" + short
	}
	return plan, nil
}

// Run preserves unique commits, attempts their remote durability, and then
// resets only the planned target worktree to origin/main.
func (p *WorktreePlan) Run(ctx context.Context) (*WorktreePlan, error) {
	if p == nil {
		return nil, fmt.Errorf("herd-reset-safe: nil worktree plan")
	}
	if len(p.Unique) == 0 {
		fmt.Fprintf(p.opts.stdout(), "herd-reset-safe: %s (%s) has no unmerged work, safe to reset\n", p.Worktree, p.Branch)
	} else {
		fmt.Fprintf(p.opts.stdout(), "herd-reset-safe: %s has %d unmerged commit(s), preserving to %s before reset:\n", p.Worktree, len(p.Unique), p.PreserveBranch)
		for _, sha := range p.Unique {
			fmt.Fprintf(p.opts.stdout(), "  %s\n", sha)
		}
		if err := gitRun(ctx, p.Worktree, "branch", p.PreserveBranch, "HEAD"); err != nil {
			return nil, fmt.Errorf("herd-reset-safe: could not stage %s: %w", p.PreserveBranch, err)
		}
		if err := gitRun(ctx, p.Worktree, "push", "origin", p.PreserveBranch); err != nil {
			fmt.Fprintf(p.opts.stderr(), "herd-reset-safe: WARN could not push %s — it still exists locally at %s as a branch ref; do not delete this worktree until it's recovered\n", p.PreserveBranch, p.Worktree)
		} else {
			p.Pushed = true
			fmt.Fprintf(p.opts.stdout(), "herd-reset-safe: pushed %s. Recover with: git cherry-pick <sha>  OR  git merge %s\n", p.PreserveBranch, p.PreserveBranch)
		}
	}
	if err := gitRun(ctx, p.Worktree, "reset", "--hard", "origin/main"); err != nil {
		return nil, fmt.Errorf("herd-reset-safe: reset failed: %w", err)
	}
	resetSHA, err := gitOutput(ctx, p.Worktree, "rev-parse", "--short", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("herd-reset-safe: reset completed but could not read HEAD: %w", err)
	}
	p.ResetSHA = strings.TrimSpace(resetSHA)
	fmt.Fprintf(p.opts.stdout(), "herd-reset-safe: %s reset to origin/main (%s)\n", p.Worktree, p.ResetSHA)
	return p, nil
}

func dirtyFiles(ctx context.Context, worktree string) ([]string, error) {
	out, err := gitOutput(ctx, worktree, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var dirty []string
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" || line == "?? TASK-PACKET.md" {
			continue
		}
		if len(line) >= 3 {
			dirty = append(dirty, line)
		} else {
			dirty = append(dirty, line)
		}
	}
	return dirty, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", filepath.Clean(dir)}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func gitRun(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", filepath.Clean(dir)}, args...)...)
	return cmd.Run()
}
