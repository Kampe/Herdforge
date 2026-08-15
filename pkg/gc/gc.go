package gc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/overlap"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

var errGlobalAutoReapDisabled = errors.New("global auto-reap disabled")

// OverlapReport is the result of an unmerged-branch convergence scan.
// OverlappingFiles maps each hot filepath to the list of branch names
// touching it (first-seen order per tip). ScannedTips is the count of
// distinct unmerged tips that participated in the census. BaseRef is the
// integration point the tips were measured against.
type OverlapReport struct {
	OverlappingFiles map[string][]string // filepath -> list of branches touching it
	ScannedTips      int                 // distinct unmerged tips scanned
	BaseRef          string              // base ref used for the ahead-of check
}

type GCManager struct {
	RepoRoot      string
	WM            *worktree.WorktreeManager
	HoldReader    lifecycle.HoldReader
	HoldAuthority *lifecycle.HoldAuthority
}

func NewCanonicalGCManager(repoRoot string, wm *worktree.WorktreeManager) (*GCManager, error) {
	path := lifecycle.CanonicalStatePath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	authority, err := lifecycle.NewHoldAuthority(path)
	if err != nil {
		return nil, err
	}
	return &GCManager{RepoRoot: repoRoot, WM: wm, HoldReader: authority, HoldAuthority: authority}, nil
}

func (g *GCManager) Close() error {
	if g.HoldAuthority != nil {
		return g.HoldAuthority.Close()
	}
	return nil
}

func NewGCManager(repoRoot string, wm *worktree.WorktreeManager) *GCManager {
	return &GCManager{
		RepoRoot: repoRoot,
		WM:       wm,
	}
}

// ScanOverlap surfaces files that minTips or more distinct unmerged branches
// are editing, before those branches collide at merge. It delegates to the
// authoritative pkg/overlap engine (the Go port of bin/herd-overlap) so the
// census, tip de-duplication, exclusion registry, and deterministic ranking
// are shared with the herd overlap CLI.
//
// The scan measures branches ahead of a base ref, not worktree directories.
// A detached-HEAD worktree carries no branch ref and therefore cannot
// contribute a phantom overlap. A missing worktree (pruned directory) is
// irrelevant: the census reads refs/heads, not the worktree registry.
//
// FAC-299: the previous stub parsed `git worktree list --porcelain`, discarded
// the data, ignored minTips, and always returned an empty report. This
// implementation never returns empty success on a failure — a missing base
// ref, a non-git directory, or an engine error is propagated as a non-nil
// error.
func (g *GCManager) ScanOverlap(ctx context.Context, minTips int) (*OverlapReport, error) {
	if g == nil {
		return nil, errors.New("gc: nil manager")
	}
	if minTips < 1 {
		return nil, fmt.Errorf("gc: minTips must be >= 1, got %d", minTips)
	}

	baseRef := resolveOverlapBaseRef(ctx, g.RepoRoot)
	if baseRef == "" {
		return nil, fmt.Errorf("gc: no base ref found for overlap scan; set HERD_OVERLAP_MAIN_REF or fetch origin/main")
	}

	engine := overlap.NewOverlap(g.RepoRoot)
	hots, scanned, err := engine.FileOverlaps(ctx, baseRef, minTips, nil)
	if err != nil {
		return nil, fmt.Errorf("gc: overlap scan against %s: %w", baseRef, err)
	}

	report := &OverlapReport{
		OverlappingFiles: make(map[string][]string, len(hots)),
		ScannedTips:      scanned,
		BaseRef:          baseRef,
	}
	for _, h := range hots {
		report.OverlappingFiles[h.File] = h.Branches
	}
	return report, nil
}

// resolveOverlapBaseRef finds the integration point that unmerged branches are
// ahead of. The CLI uses origin/main; a library function must also work against
// a local-only repo, so the chain falls back through common defaults. Returns
// "" when no candidate ref exists, which the caller treats as a hard error.
func resolveOverlapBaseRef(ctx context.Context, repoRoot string) string {
	if v := os.Getenv("HERD_OVERLAP_MAIN_REF"); v != "" && refExists(ctx, repoRoot, v) {
		return v
	}
	for _, ref := range []string{"origin/main", "origin/master", "main", "master"} {
		if refExists(ctx, repoRoot, ref) {
			return ref
		}
	}
	return ""
}

func refExists(ctx context.Context, repoRoot, ref string) bool {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = repoRoot
	return cmd.Run() == nil
}

// PruneStaleWorktrees cleans up merged or orphaned worktree directories (porting bin/herd-gc)
func (g *GCManager) PruneStaleWorktrees(ctx context.Context) (int, error) {
	// This package-level entry point has no way to supply exact targets,
	// lease/session fencing, integration/board evidence, or an explicit action
	// policy. It is intentionally report-only and therefore cannot remove a
	// worktree from the caller's repository.
	if g == nil || g.WM == nil {
		return 0, fmt.Errorf("gc: worktree manager is required")
	}
	if g.HoldReader == nil {
		return 0, fmt.Errorf("gc: durable hold authority is required")
	}
	return 0, fmt.Errorf("gc: %w; exact targets and action evidence are required", errGlobalAutoReapDisabled)
}
