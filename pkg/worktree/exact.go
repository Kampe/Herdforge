package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// ObserveExactWorktree is read-only recovery for an integration checkout.
// nil means the destination does not exist. An unregistered directory, stale
// branch, dirty tree or unknown Git read is an error, never an absent effect.
func (w *WorktreeManager) ObserveExactWorktree(ctx context.Context, branch, target, sha string) (*WorktreeInfo, error) {
	if w == nil || len(sha) != 40 || strings.Trim(sha, "0123456789abcdef") != "" {
		return nil, fmt.Errorf("exact worktree: manager and full commit identity required")
	}
	common, err := GitCommonDir(ctx, w.RepoRoot)
	if err != nil {
		return nil, err
	}
	if !pathsEqual(filepath.Dir(common), normalizePath(w.RepoRoot)) {
		return nil, fmt.Errorf("exact worktree: manager must use the project control root")
	}
	if err := RejectSharedRoot(w.RepoRoot, target); err != nil {
		return nil, err
	}
	if pathsEqual(normalizePath(target), normalizePath(w.WorktreeDir)) || !isContainedIn(target, w.WorktreeDir) {
		return nil, fmt.Errorf("exact worktree: destination must be inside its configured pool")
	}
	if _, err := w.exactGit(ctx, w.RepoRoot, "check-ref-format", "refs/heads/"+branch); err != nil {
		return nil, err
	}
	resolved, err := w.revParse(ctx, sha+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("exact worktree: requested commit is unavailable: %w", err)
	}
	if resolved != sha {
		return nil, fmt.Errorf("exact worktree: requested object is not the exact commit")
	}
	registered, err := w.ListWorktrees(ctx)
	if err != nil {
		return nil, err
	}
	var found *WorktreeInfo
	others := make([]*WorktreeInfo, 0, len(registered))
	for _, wt := range registered {
		if pathsEqual(normalizePath(wt.Path), normalizePath(target)) {
			if found != nil {
				return nil, fmt.Errorf("exact worktree: multiple registrations name the destination")
			}
			found = wt
			continue
		}
		if wt.Branch == branch {
			return nil, fmt.Errorf("exact worktree: branch is already attached elsewhere")
		}
		others = append(others, wt)
	}
	if err := RejectContainedDestination(w.RepoRoot, target, others); err != nil {
		return nil, err
	}
	if found == nil {
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("exact worktree: destination exists without a Git registration")
		}
	} else {
		if found.Branch != branch || found.Commit != sha {
			return nil, fmt.Errorf("exact worktree: registered branch or HEAD differs from requested identity")
		}
		actualCommon, err := GitCommonDir(ctx, target)
		if err != nil || actualCommon != common {
			return nil, fmt.Errorf("exact worktree: registered path has no matching repository")
		}
		status, err := w.exactGit(ctx, target, "status", "--porcelain", gitroot.StatusUntrackedNormal)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(status) != "" {
			return nil, fmt.Errorf("exact worktree: registered destination is dirty")
		}
	}
	head, err := w.BranchHead(ctx, branch)
	if err != nil {
		return nil, err
	}
	if head != "" && head != sha {
		return nil, fmt.Errorf("exact worktree: existing branch has another commit; reset refused")
	}
	if found != nil && head != sha {
		return nil, fmt.Errorf("exact worktree: branch changed during observation")
	}
	return found, nil
}

// CreateExactWorktree creates or resumes an integration checkout without a
// new anchor commit or a branch reset. The branch is create-only CAS; a crash
// between ref creation and worktree add can safely resume at that exact ref.
func (w *WorktreeManager) CreateExactWorktree(ctx context.Context, branch, target, sha string) (*WorktreeInfo, error) {
	found, err := w.ObserveExactWorktree(ctx, branch, target, sha)
	if err != nil || found != nil {
		return found, err
	}
	if err := w.admitDisk("worktree_create", target); err != nil {
		return nil, err
	}
	head, err := w.BranchHead(ctx, branch)
	if err != nil {
		return nil, err
	}
	if head == "" {
		if _, err := w.exactGit(ctx, w.RepoRoot, "update-ref", "refs/heads/"+branch, sha, strings.Repeat("0", 40)); err != nil {
			return nil, fmt.Errorf("exact worktree: branch creation requires readback: %w", err)
		}
	} else if head != sha {
		return nil, fmt.Errorf("exact worktree: branch changed before attachment")
	}
	// Recheck path/registration after the ref mutation. Do not attach a stale
	// directory or reset a concurrently changed branch to make the add succeed.
	found, err = w.ObserveExactWorktree(ctx, branch, target, sha)
	if err != nil || found != nil {
		return found, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return nil, err
	}
	if _, err := w.exactGit(ctx, w.RepoRoot, "worktree", "add", target, branch); err != nil {
		return nil, fmt.Errorf("exact worktree: attachment requires readback: %w", err)
	}
	found, err = w.ObserveExactWorktree(ctx, branch, target, sha)
	if err == nil && found == nil {
		return nil, fmt.Errorf("exact worktree: successful add has no matching registration")
	}
	return found, err
}

// BranchHead reads one exact local branch. Empty with nil error is proven
// absence; a failed or malformed Git read is never an empty branch.
func (w *WorktreeManager) BranchHead(ctx context.Context, branch string) (string, error) {
	ref := "refs/heads/" + branch
	out, err := w.exactGit(ctx, w.RepoRoot, "for-each-ref", "--format=%(refname) %(objectname)", "--", ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return "", fmt.Errorf("exact worktree: malformed branch readback")
		}
		if fields[0] == ref {
			return fields[1], nil
		}
	}
	return "", nil
}

func (w *WorktreeManager) exactGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := execCommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("exact worktree: git %s: %w (%s)", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
