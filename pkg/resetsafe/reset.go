// Package resetsafe provides the fail-closed preserve-and-reset operation for
// feature worktrees. It deliberately has no worktree removal or pruning path.
package resetsafe

import (
	"github.com/Kampe/Herdforge/pkg/refname"
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

	repoRoot  string
	opts      Options
	authority planAuthority
}

type planAuthority struct {
	repoRoot       string
	worktree       string
	branch         string
	head           string
	unique         []string
	preserveBranch string
	originMain     string
}

var gitRunFn = gitRun

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
	if branch == "HEAD" {
		return "", "", fmt.Errorf("herd-reset-safe: refusing on detached HEAD — a feature branch is required for preservation")
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
	canonicalRoot, err := canonicalExistingPath(repoRoot, "repo root")
	if err != nil {
		return nil, err
	}
	canonicalWorktree, err := canonicalExistingPath(worktreePath, "worktree")
	if err != nil {
		return nil, err
	}
	if err := sameRepository(ctx, canonicalRoot, canonicalWorktree); err != nil {
		return nil, err
	}
	branch, short, err := NewWorktree(ctx, canonicalWorktree)
	if err != nil {
		return nil, err
	}
	// Match the binding script's order: refresh the target's remote-tracking
	// ref before dirty/cherry planning. Fetch is deliberately best-effort;
	// strict local cherry evidence below still governs when it is unavailable.
	_ = gitRun(ctx, canonicalWorktree, "fetch", "origin", "main")
	if dirty, err := dirtyFiles(ctx, canonicalWorktree); err != nil {
		return nil, fmt.Errorf("herd-reset-safe: cannot inspect uncommitted changes: %w", err)
	} else if len(dirty) > 0 {
		return nil, fmt.Errorf("herd-reset-safe: %s has uncommitted changes, refusing:\n  %s\nherd-reset-safe: commit or stash first, then re-run", worktreePath, strings.Join(dirty, "\n  "))
	}

	u, err := harvest.NewHarvester(canonicalRoot).UnmergedForStrictLocal(ctx, canonicalWorktree)
	if err != nil {
		return nil, fmt.Errorf("herd-reset-safe: cannot verify unmerged work: %w", err)
	}
	head, err := gitOutput(ctx, canonicalWorktree, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("herd-reset-safe: cannot bind HEAD: %w", err)
	}
	originMain, err := gitOutput(ctx, canonicalWorktree, "rev-parse", "origin/main")
	if err != nil {
		return nil, fmt.Errorf("herd-reset-safe: cannot bind origin/main: %w", err)
	}
	plan := &WorktreePlan{Worktree: worktreePath, Branch: branch, ShortSHA: short, repoRoot: repoRoot, opts: opts}
	plan.authority = planAuthority{
		repoRoot:   canonicalRoot,
		worktree:   canonicalWorktree,
		branch:     branch,
		head:       strings.TrimSpace(head),
		originMain: strings.TrimSpace(originMain),
	}
	if u != nil && len(u.Unmerged) > 0 {
		plan.Unique = append([]string(nil), u.Unmerged...)
		plan.PreserveBranch = "harvest/" + refname.PublishSafeSegment(branch) + "-" + short
		plan.authority.unique = append([]string(nil), u.Unmerged...)
		plan.authority.preserveBranch = plan.PreserveBranch
	}
	return plan, nil
}

// Run preserves unique commits, attempts their remote durability, and then
// resets only the planned target worktree to origin/main.
func (p *WorktreePlan) Run(ctx context.Context) (*WorktreePlan, error) {
	if p == nil {
		return nil, fmt.Errorf("herd-reset-safe: nil worktree plan")
	}
	a := p.authority
	if err := revalidate(ctx, a); err != nil {
		return nil, err
	}
	if len(a.unique) == 0 {
		fmt.Fprintf(p.opts.stdout(), "herd-reset-safe: %s (%s) has no unmerged work, safe to reset\n", p.Worktree, a.branch)
	} else {
		fmt.Fprintf(p.opts.stdout(), "herd-reset-safe: %s has %d unmerged commit(s), preserving to %s before reset:\n", p.Worktree, len(a.unique), a.preserveBranch)
		for _, sha := range a.unique {
			fmt.Fprintf(p.opts.stdout(), "  %s\n", sha)
		}
		if err := gitRunFn(ctx, a.worktree, "branch", a.preserveBranch, "HEAD"); err != nil {
			return nil, fmt.Errorf("herd-reset-safe: could not stage %s: %w", a.preserveBranch, err)
		}
		if err := gitRunFn(ctx, a.worktree, "push", "origin", a.preserveBranch); err != nil {
			fmt.Fprintf(p.opts.stderr(), "herd-reset-safe: WARN could not push %s — it still exists locally at %s as a branch ref; do not delete this worktree until it's recovered\n", a.preserveBranch, p.Worktree)
		} else {
			p.Pushed = true
			fmt.Fprintf(p.opts.stdout(), "herd-reset-safe: pushed %s. Recover with: git cherry-pick <sha>  OR  git merge %s\n", a.preserveBranch, a.preserveBranch)
		}
	}
	if err := revalidate(ctx, a); err != nil {
		return nil, err
	}
	if len(a.unique) > 0 {
		preserveSHA, err := gitOutput(ctx, a.worktree, "show-ref", "--hash", "--verify", "refs/heads/"+a.preserveBranch)
		if err != nil || strings.TrimSpace(preserveSHA) != a.head {
			return nil, fmt.Errorf("herd-reset-safe: preserve ref changed")
		}
	}
	if err := gitRunFn(ctx, a.worktree, "reset", "--hard", "origin/main"); err != nil {
		return nil, fmt.Errorf("herd-reset-safe: reset failed: %w", err)
	}
	resetSHA, err := gitOutput(ctx, a.worktree, "rev-parse", "--short", "HEAD")
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
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func sameRepository(ctx context.Context, repoRoot, worktreePath string) error {
	rootCommon, err := gitOutput(ctx, repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("herd-reset-safe: repo root is not a git repository")
	}
	worktreeCommon, err := gitOutput(ctx, worktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("herd-reset-safe: %s is not owned by repo root", worktreePath)
	}
	rootCommon, err = canonicalCommonDir(rootCommon, "repo root")
	if err != nil {
		return err
	}
	worktreeCommon, err = canonicalCommonDir(worktreeCommon, "worktree")
	if err != nil {
		return err
	}
	if filepath.Clean(rootCommon) != filepath.Clean(worktreeCommon) {
		return fmt.Errorf("herd-reset-safe: %s is not owned by repo root", worktreePath)
	}
	return nil
}

func canonicalExistingPath(path, owner string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("herd-reset-safe: cannot resolve %s path %q: %w", owner, path, err)
	}
	canonical = filepath.Clean(canonical)
	if canonical == "" || canonical == "." {
		return "", fmt.Errorf("herd-reset-safe: %s path is empty after canonicalization", owner)
	}
	return canonical, nil
}

func revalidate(ctx context.Context, a planAuthority) error {
	if err := Open(a.worktree); err != nil {
		return err
	}
	canonicalWorktree, err := canonicalExistingPath(a.worktree, "worktree")
	if err != nil || canonicalWorktree != a.worktree {
		return fmt.Errorf("herd-reset-safe: planned worktree changed")
	}
	if err := sameRepository(ctx, a.repoRoot, a.worktree); err != nil {
		return err
	}
	branch, _, err := NewWorktree(ctx, a.worktree)
	if err != nil || branch != a.branch {
		return fmt.Errorf("herd-reset-safe: planned branch changed")
	}
	head, err := gitOutput(ctx, a.worktree, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != a.head {
		return fmt.Errorf("herd-reset-safe: planned HEAD changed")
	}
	if dirty, err := dirtyFiles(ctx, a.worktree); err != nil {
		return fmt.Errorf("herd-reset-safe: cannot revalidate uncommitted changes: %w", err)
	} else if len(dirty) > 0 {
		return fmt.Errorf("herd-reset-safe: planned worktree has uncommitted changes, refusing")
	}
	originMain, err := gitOutput(ctx, a.worktree, "rev-parse", "origin/main")
	if err != nil || strings.TrimSpace(originMain) != a.originMain {
		return fmt.Errorf("herd-reset-safe: planned origin/main changed")
	}
	return nil
}

func canonicalCommonDir(raw, owner string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("herd-reset-safe: %s git common dir is empty", owner)
	}
	canonical, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", fmt.Errorf("herd-reset-safe: cannot resolve %s git common dir %q: %w", owner, raw, err)
	}
	canonical = filepath.Clean(canonical)
	if canonical == "." || canonical == "" {
		return "", fmt.Errorf("herd-reset-safe: %s git common dir is empty after canonicalization", owner)
	}
	return canonical, nil
}

