package worktree

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// NestedLane is a registered worktree found physically inside another
// registered worktree — the contamination shape observed live at
// pkg/dispatch/.herd/worktrees/fac-1, nested inside the FAC-64 task
// worktree (FAC-152). DetectNestedLanes is detection only: nothing in this
// file prunes, resets, moves, or deletes anything it finds. A probe error
// is recorded on the field rather than silently dropped, so recovery
// tooling can see why a signal is missing instead of assuming clean.
type NestedLane struct {
	Path        string
	Branch      string
	HEAD        string
	Owner       string // registered worktree path this lane is nested inside
	OwnerBranch string
	Dirty       bool
	DirtyErr    string
	UniqueSHAs  []string
	UniqueErr   string
	Evidence    string
}

// DetectNestedLanes reports every registered worktree that is physically
// contained inside another registered worktree, other than the expected
// nesting of the pool directory under the manager's own root. It is a pure
// read (git worktree list, git status, git cherry) — no removal, reset, or
// move — so a dirty nested lane like fac-1 is always preserved and surfaced
// as BLOCKED recovery work rather than silently cleaned up.
func (w *WorktreeManager) DetectNestedLanes(ctx context.Context, defaultBranch string) ([]NestedLane, error) {
	if defaultBranch == "" {
		defaultBranch = DefaultBranch
	}
	wtList, err := w.ListWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("detect nested lanes: list worktrees: %w", err)
	}

	rootN := normalizePath(w.RepoRoot)
	var nested []NestedLane
	for _, wt := range wtList {
		if wt == nil || wt.Path == "" {
			continue
		}
		if pathsEqual(normalizePath(wt.Path), rootN) {
			continue // the canonical root itself is never "nested"
		}
		for _, other := range wtList {
			if other == nil || other == wt || other.Path == "" {
				continue
			}
			if pathsEqual(normalizePath(other.Path), rootN) {
				continue // nesting under the root's own pool directory is expected
			}
			if !isContainedIn(wt.Path, other.Path) {
				continue
			}
			nested = append(nested, w.classifyNestedLane(ctx, wt, other, defaultBranch))
			break
		}
	}
	sort.Slice(nested, func(i, j int) bool { return nested[i].Path < nested[j].Path })
	return nested, nil
}

func (w *WorktreeManager) classifyNestedLane(ctx context.Context, wt, owner *WorktreeInfo, defaultBranch string) NestedLane {
	nl := NestedLane{
		Path: wt.Path, Branch: wt.Branch, HEAD: wt.Commit,
		Owner: owner.Path, OwnerBranch: owner.Branch,
	}

	dirty, derr := w.isDirty(ctx, wt.Path)
	if derr != nil {
		nl.DirtyErr = derr.Error()
	} else {
		nl.Dirty = dirty
	}

	branch := strings.TrimSpace(wt.Branch)
	if branch == "" {
		nl.UniqueErr = "branch unresolved; cannot compute unique commits"
	} else if integration, ierr := w.resolveIntegrationBase(ctx, defaultBranch); ierr != nil {
		nl.UniqueErr = ierr.Error()
	} else if unique, uerr := w.uniqueCommits(ctx, wt.Path, integration, branch); uerr != nil {
		nl.UniqueErr = uerr.Error()
	} else {
		nl.UniqueSHAs = unique
	}

	nl.Evidence = fmt.Sprintf(
		"worktree %s (branch %q) is nested inside registered worktree %s (branch %q); dirty=%v unique=%d — BLOCKED: preserve, do not prune/reset/move",
		wt.Path, wt.Branch, owner.Path, owner.Branch, nl.Dirty, len(nl.UniqueSHAs))
	return nl
}
