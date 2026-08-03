package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/preflight"
)

// execCommandContext is a variable so tests can mock; defaults to exec.CommandContext
var execCommandContext = exec.CommandContext

// DefaultBranch is used when the caller does not specify one.
const DefaultBranch = "main"

// AnchorRefPrefix is the durable refs namespace protecting task bases (FAC-121).
const AnchorRefPrefix = "refs/herd/anchors/"

type WorktreeManager struct {
	RepoRoot    string
	WorktreeDir string
}

func NewWorktreeManager(repoRoot string) *WorktreeManager {
	return &WorktreeManager{
		RepoRoot:    repoRoot,
		WorktreeDir: filepath.Join(repoRoot, ".herd", "worktrees"),
	}
}

func NewWorktreePool(repoRoot string, worktreeDir string) *WorktreeManager {
	return &WorktreeManager{
		RepoRoot:    repoRoot,
		WorktreeDir: worktreeDir,
	}
}

// WorktreeInfo describes a task worktree after creation or reattach.
// Branch is always the actual Git branch (never a fictional packet name).
// BaseSHA is the fetched immutable origin/<default> commit used as the base.
// AnchorRef is the durable ref protecting that base (and tip after anchor commit).
type WorktreeInfo struct {
	Path      string
	Branch    string
	Commit    string // worktree HEAD (after FAC-106 anchor commit when created)
	BaseSHA   string // immutable origin base used at creation
	AnchorRef string // refs/herd/anchors/<task>
}

// guardDiskPressure fails closed before any worktree mutation when the repo,
// pool, or temp volume is under critical disk pressure (FAC-153).
func (w *WorktreeManager) guardDiskPressure(operation string) error {
	return preflight.CheckDiskPressure(operation, w.RepoRoot, w.WorktreeDir, os.TempDir())
}

func (w *WorktreeManager) CreateWorktree(ctx context.Context, branch string, targetDir string) error {
	if err := w.guardDiskPressure("worktree_create"); err != nil {
		return err
	}
	cmd := execCommandContext(ctx, "git", "worktree", "add", "-b", branch, targetDir, "HEAD")
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create worktree: %v, output: %s", err, string(output))
	}
	return nil
}

func (w *WorktreeManager) RemoveWorktree(ctx context.Context, targetDir string) error {
	cmd := execCommandContext(ctx, "git", "worktree", "remove", "--force", targetDir)
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to remove worktree: %v, output: %s", err, string(output))
	}
	return nil
}

// ListWorktrees runs git worktree list and returns structured worktree information
func (w *WorktreeManager) ListWorktrees(ctx context.Context) ([]*WorktreeInfo, error) {
	cmd := execCommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = w.RepoRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w, output: %s", err, string(output))
	}

	var res []*WorktreeInfo
	lines := strings.Split(string(output), "\n")
	var current *WorktreeInfo

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if current != nil {
				res = append(res, current)
				current = nil
			}
			continue
		}

		if strings.HasPrefix(line, "worktree ") {
			current = &WorktreeInfo{Path: strings.TrimPrefix(line, "worktree ")}
		} else if strings.HasPrefix(line, "HEAD ") && current != nil {
			current.Commit = strings.TrimPrefix(line, "HEAD ")
		} else if strings.HasPrefix(line, "branch ") && current != nil {
			current.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		}
	}

	if current != nil {
		res = append(res, current)
	}

	return res, nil
}

// RejectSharedRoot fails closed when a candidate worktree path is the shared
// repository checkout (FAC-121). Write-capable agents must never start there.
func RejectSharedRoot(repoRoot, worktreePath string) error {
	if repoRoot == "" || worktreePath == "" {
		return fmt.Errorf("shared-root denial: empty path")
	}
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("shared-root denial: resolve repo root: %w", err)
	}
	absWT, err := filepath.Abs(worktreePath)
	if err != nil {
		return fmt.Errorf("shared-root denial: resolve worktree: %w", err)
	}
	// Clean and eval symlinks when possible so . / .. and link aliases match.
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}
	if resolved, err := filepath.EvalSymlinks(absWT); err == nil {
		absWT = resolved
	}
	if absRoot == absWT {
		return fmt.Errorf("shared-root denial: worktree path %q is the repository root; write-capable agents must not start there", worktreePath)
	}
	return nil
}

// AnchorRefFor returns the durable anchor ref for a task.
func AnchorRefFor(taskRef string) string {
	return AnchorRefPrefix + strings.ToLower(taskRef)
}

// TaskBranch returns the canonical Git branch name for a task ref.
// This is the only branch name CreateTaskWorktree creates or reattaches;
// callers must record this value rather than inventing a packet alias.
func TaskBranch(taskRef string) string {
	return fmt.Sprintf("herd/%s", strings.ToLower(taskRef))
}

// ResolveImmutableBase fetches origin/<defaultBranch> once and returns its SHA.
// It never uses the local checkout HEAD, so a dirty or behind local main cannot
// poison task bases (FAC-121).
func (w *WorktreeManager) ResolveImmutableBase(ctx context.Context, defaultBranch string) (string, error) {
	if defaultBranch == "" {
		defaultBranch = DefaultBranch
	}
	// Fetch may fail offline; if origin/<branch> already exists we still pin it.
	fetch := execCommandContext(ctx, "git", "fetch", "--quiet", "origin", defaultBranch)
	fetch.Dir = w.RepoRoot
	fetchOut, fetchErr := fetch.CombinedOutput()

	ref := "origin/" + defaultBranch
	sha, err := w.revParse(ctx, ref)
	if err != nil {
		if fetchErr != nil {
			return "", fmt.Errorf("failed to resolve immutable base %s after fetch error: fetch=%v (%s); rev-parse=%w",
				ref, fetchErr, strings.TrimSpace(string(fetchOut)), err)
		}
		return "", fmt.Errorf("failed to resolve immutable base %s: %w", ref, err)
	}
	return sha, nil
}

func (w *WorktreeManager) revParse(ctx context.Context, rev string) (string, error) {
	cmd := execCommandContext(ctx, "git", "rev-parse", "--verify", rev)
	cmd.Dir = w.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %v (%s)", rev, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (w *WorktreeManager) updateRef(ctx context.Context, ref, sha string) error {
	cmd := execCommandContext(ctx, "git", "update-ref", ref, sha)
	cmd.Dir = w.RepoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git update-ref %s: %v (%s)", ref, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (w *WorktreeManager) branchExists(ctx context.Context, branch string) bool {
	_, err := w.revParse(ctx, "refs/heads/"+branch)
	return err == nil
}

func (w *WorktreeManager) currentBranchAt(ctx context.Context, dir string) (string, error) {
	cmd := execCommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --abbrev-ref HEAD in %s: %v (%s)", dir, err, strings.TrimSpace(string(out)))
	}
	b := strings.TrimSpace(string(out))
	if b == "" || b == "HEAD" {
		return "", fmt.Errorf("worktree %s is detached HEAD; expected a branch", dir)
	}
	return b, nil
}

func (w *WorktreeManager) headAt(ctx context.Context, dir string) (string, error) {
	cmd := execCommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD in %s: %v (%s)", dir, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// CreateTaskWorktree spins up an isolated ephemeral worktree for a task ref
// from a fetched immutable origin/<defaultBranch> base (FAC-121).
//
// Guarantees:
//   - base is origin's tip, not local HEAD
//   - recorded Branch is the actual Git branch name
//   - a durable anchor ref protects the base before return
//   - shared-root paths are rejected
//   - existing worktree is reattached; branch-without-worktree is reattached
func (w *WorktreeManager) CreateTaskWorktree(ctx context.Context, taskRef string) (*WorktreeInfo, error) {
	return w.CreateTaskWorktreeFrom(ctx, taskRef, DefaultBranch)
}

// CreateTaskWorktreeFrom is CreateTaskWorktree with an explicit default branch.
func (w *WorktreeManager) CreateTaskWorktreeFrom(ctx context.Context, taskRef, defaultBranch string) (*WorktreeInfo, error) {
	if strings.TrimSpace(taskRef) == "" {
		return nil, fmt.Errorf("task ref is required")
	}
	// Fail closed before ANY mutation — including the durable anchor ref
	// write below — when disk headroom is critical (FAC-153).
	if err := w.guardDiskPressure("worktree_create"); err != nil {
		return nil, err
	}
	branch := TaskBranch(taskRef)
	targetPath := filepath.Join(w.WorktreeDir, strings.ToLower(taskRef))
	anchorRef := AnchorRefFor(taskRef)

	if err := RejectSharedRoot(w.RepoRoot, targetPath); err != nil {
		return nil, err
	}

	baseSHA, err := w.ResolveImmutableBase(ctx, defaultBranch)
	if err != nil {
		return nil, err
	}

	// Pin durable anchor at the immutable base before any worktree mutation.
	if err := w.updateRef(ctx, anchorRef, baseSHA); err != nil {
		return nil, fmt.Errorf("failed to create durable anchor ref %s: %w", anchorRef, err)
	}
	// Verify the anchor actually points at the base.
	got, err := w.revParse(ctx, anchorRef)
	if err != nil || got != baseSHA {
		return nil, fmt.Errorf("anchor ref %s verification failed: got %q want %q err=%v", anchorRef, got, baseSHA, err)
	}

	// Existing worktree path: reattach, preserve actual Git branch.
	if _, err := os.Stat(filepath.Join(targetPath, ".git")); err == nil {
		return w.attachExisting(ctx, targetPath, branch, baseSHA, anchorRef)
	}
	// Also match by listed path (some checkouts use a bare .git file).
	if wtList, listErr := w.ListWorktrees(ctx); listErr == nil {
		for _, wt := range wtList {
			if wt.Path == targetPath {
				return w.attachExisting(ctx, targetPath, branch, baseSHA, anchorRef)
			}
		}
	}

	if err := os.MkdirAll(w.WorktreeDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create worktree root directory: %w", err)
	}

	// Containment gate (FAC-152): refuse to create a new registered worktree
	// nested inside another registered worktree — the shape observed at
	// pkg/dispatch/.herd/worktrees/fac-1 nested inside the FAC-64 task
	// worktree. Runs once, before either git worktree add path below.
	registered, err := w.ListWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to verify worktree containment: %w", err)
	}
	if err := RejectContainedDestination(w.RepoRoot, targetPath, registered); err != nil {
		return nil, err
	}

	// Branch exists without worktree: reattach with git worktree add (no -b).
	if w.branchExists(ctx, branch) {
		cmd := execCommandContext(ctx, "git", "worktree", "add", targetPath, branch)
		cmd.Dir = w.RepoRoot
		if output, err := cmd.CombinedOutput(); err != nil {
			// If path already registered, fall through to list recovery.
			if wtList, listErr := w.ListWorktrees(ctx); listErr == nil {
				for _, wt := range wtList {
					if wt.Path == targetPath || wt.Branch == branch {
						return w.attachExisting(ctx, wt.Path, branch, baseSHA, anchorRef)
					}
				}
			}
			return nil, fmt.Errorf("failed to reattach worktree for existing branch %s: %v, output: %s", branch, err, string(output))
		}
		return w.attachExisting(ctx, targetPath, branch, baseSHA, anchorRef)
	}

	// Fresh: create branch from immutable base SHA (never local HEAD).
	cmd := execCommandContext(ctx, "git", "worktree", "add", "-b", branch, targetPath, baseSHA)
	cmd.Dir = w.RepoRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		if wtList, listErr := w.ListWorktrees(ctx); listErr == nil {
			for _, wt := range wtList {
				if wt.Path == targetPath || wt.Branch == branch {
					return w.attachExisting(ctx, targetPath, branch, baseSHA, anchorRef)
				}
			}
		}
		return nil, fmt.Errorf("failed to create git worktree: %v, output: %s", err, string(output))
	}

	// Anchor commit (FAC-106): ensure branch has a restorable tip independent of
	// the working tree. Best-effort relative to reap safety; creation already
	// succeeded from the immutable base.
	anchor := execCommandContext(ctx, "git", "-C", targetPath,
		"commit", "--allow-empty", "-q", "-m",
		fmt.Sprintf("chore: anchor %s worktree (FAC-106 reap-safe)", strings.ToUpper(taskRef)))
	_ = anchor.Run()

	info, err := w.attachExisting(ctx, targetPath, branch, baseSHA, anchorRef)
	if err != nil {
		return nil, err
	}
	// Advance durable anchor to the post-FAC-106 tip so cleanup cannot drop
	// the only restorable commit before the agent lands real work.
	if info.Commit != "" {
		_ = w.updateRef(ctx, anchorRef, info.Commit)
		info.AnchorRef = anchorRef
	}
	return info, nil
}

// attachExisting reads the real Git branch/HEAD at path and returns WorktreeInfo.
// The recorded Branch is always what Git reports when available; the expected
// branch name is used only as a fallback when the worktree is mid-setup.
func (w *WorktreeManager) attachExisting(ctx context.Context, path, expectedBranch, baseSHA, anchorRef string) (*WorktreeInfo, error) {
	if err := RejectSharedRoot(w.RepoRoot, path); err != nil {
		return nil, err
	}
	branch := expectedBranch
	if actual, err := w.currentBranchAt(ctx, path); err == nil && actual != "" {
		branch = actual
	}
	commit, _ := w.headAt(ctx, path)
	return &WorktreeInfo{
		Path:      path,
		Branch:    branch,
		Commit:    commit,
		BaseSHA:   baseSHA,
		AnchorRef: anchorRef,
	}, nil
}

