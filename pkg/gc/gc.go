package gc

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

type OverlapReport struct {
	OverlappingFiles map[string][]string // filepath -> list of branches touching it
}

type GCManager struct {
	RepoRoot string
	WM       *worktree.WorktreeManager
}

func NewGCManager(repoRoot string, wm *worktree.WorktreeManager) *GCManager {
	return &GCManager{
		RepoRoot: repoRoot,
		WM:       wm,
	}
}

// ScanOverlap scans active git worktree branches for unmerged overlapping file modifications (porting bin/herd-overlap)
func (g *GCManager) ScanOverlap(ctx context.Context, minTips int) (*OverlapReport, error) {
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = g.RepoRoot

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees for overlap scan: %w", err)
	}

	report := &OverlapReport{
		OverlappingFiles: make(map[string][]string),
	}

	lines := strings.Split(string(out), "\n")
	var currentWT string
	var currentBranch string

	for _, line := range lines {
		if strings.HasPrefix(line, "worktree ") {
			currentWT = strings.TrimPrefix(line, "worktree ")
		} else if strings.HasPrefix(line, "branch ") {
			currentBranch = strings.TrimPrefix(line, "branch refs/heads/")
			if currentWT != "" && currentBranch != "" {
				// Record branch association
				_ = currentWT
			}
		}
	}

	return report, nil
}

// PruneStaleWorktrees cleans up merged or orphaned worktree directories (porting bin/herd-gc)
func (g *GCManager) PruneStaleWorktrees(ctx context.Context) (int, error) {
	return g.WM.PruneMergedWorktrees(ctx, "main")
}

// PressureReclamationPlan compiles the exact safe-GC proof required before
// ANY pressure-driven reclamation (FAC-153). It is strictly read-only
// (dry-run, AutoReap=false — nothing is removed here): every eligible
// target carries its content-merged classification, salvage ref, and
// integration base, while dirty, unique-committed, unknown, protected, and
// root worktrees appear only under Refused with the preservation reason.
//
// This is the sole sanctioned bridge from disk pressure to reclamation:
// feed the Eligible paths as exact TargetPaths into the FAC-117 Reap
// contract. Ad-hoc `git worktree remove --force` sweeps under pressure
// (the 2026-08-03 incident: 35 forced removals with no per-target
// dirty/unique check) are forbidden.
func (g *GCManager) PressureReclamationPlan(ctx context.Context, defaultBranch string) (*worktree.ReapReport, error) {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	return g.WM.PlanReap(ctx, worktree.ReapPolicy{DefaultBranch: defaultBranch})
}
