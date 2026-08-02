// Package lost ports bin/herd-lost: find work that exists on a branch
// anywhere in the repo (local refs/heads/ AND remote refs/remotes/origin/)
// but is NOT on origin/main.
//
// Two rules this package follows, both learned the hard way:
//
// SUBJECTS, NOT PATCH-IDS. `git cherry` marks a commit '+' (unique) whenever
// its patch-id differs, and patch-id incorporates surrounding CONTEXT. A
// branch that predates a rebase therefore reports carried-forward work as
// unique. That false signal cost four lanes a verification cycle on
// 2026-07-24. We compare commit SUBJECTS against origin/main instead,
// because a subject survives a rebase and a patch-id does not.
//
// A BRANCH WITH A LIVE WORKTREE IS NOT LOST. Standing lanes commit locally
// and hold pins for coordinator harvest by design, so their branches
// routinely sit unmerged. Those have an owner; they are reported separately,
// never as lost.
package lost

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

// ErrNoMain means origin/main is absent — usage-level failure (exit 2).
var ErrNoMain = errors.New("herd-lost: origin/main not found")

// neverMerged are infra branches that are INTENTIONALLY never merged to
// main. Not lost work; must never be offered for deletion.
var neverMerged = map[string]bool{"herd/allocators": true}

// BranchRow is one classified branch.
type BranchRow struct {
	Label        string // display name (origin/x for remote-only)
	Branch       string // branch key (origin- stripped)
	Total        int    // subjects examined (capped at Limit)
	Unmerged     int    // subjects not found on origin/main
	FirstMissing string // first unmerged subject
	Age          string // YYYY-MM-DD of the branch tip commit
}

// LostReport is the full classification.
type LostReport struct {
	Lost       []BranchRow
	Parked     []BranchRow
	Owned      []BranchRow
	Superseded []string
	LostTotal  int // unmerged subjects across Lost rows
}

// Finder walks the refs.
type Finder struct {
	repoRoot string
	Limit    int
	Fetch    bool
}

// NewFinder uses the zsh defaults: limit 60, fetch first.
func NewFinder(repoRoot string) *Finder {
	return &Finder{repoRoot: repoRoot, Limit: 60, Fetch: true}
}

func (f *Finder) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = f.repoRoot
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func (f *Finder) gitLines(ctx context.Context, args ...string) ([]string, error) {
	out, err := f.git(ctx, args...)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// Find classifies every branch. Precedence per branch (the exact zsh
// decision tree): 0 subjects → skip; fully superseded → owned-if-live else
// superseded; unmerged → owned if live worktree, parked if park/ prefix,
// else LOST.
func (f *Finder) Find(ctx context.Context) (*LostReport, error) {
	if f.Fetch {
		_, _ = f.git(ctx, "fetch", "origin", "--quiet")
	}
	if _, err := f.git(ctx, "rev-parse", "--verify", "-q", "origin/main"); err != nil {
		return nil, ErrNoMain
	}

	// Owned set: branches checked out by a live worktree.
	owned := map[string]bool{}
	wts, err := worktree.NewWorktreeManager(f.repoRoot).ListWorktrees(ctx)
	if err != nil {
		return nil, fmt.Errorf("herd-lost: %w", err)
	}
	for _, wt := range wts {
		if wt.Branch != "" {
			owned[wt.Branch] = true
		}
	}

	// Subjects already on origin/main.
	onMain := map[string]bool{}
	subjects, err := f.gitLines(ctx, "log", "origin/main", "--format=%s")
	if err != nil {
		return nil, fmt.Errorf("herd-lost: reading origin/main log: %w", err)
	}
	for _, s := range subjects {
		onMain[s] = true
	}

	// Enumerate: local heads first, then remote origin/* skipping main/HEAD
	// and any remote whose local twin exists (each work reported once).
	type ref struct{ label, branch, refname string }
	var refs []ref
	local := map[string]bool{}
	heads, _ := f.gitLines(ctx, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	for _, b := range heads {
		if b == "main" || b == "master" {
			continue
		}
		local[b] = true
		refs = append(refs, ref{label: b, branch: b, refname: b})
	}
	remotes, _ := f.gitLines(ctx, "for-each-ref", "--format=%(refname:short)", "refs/remotes/origin/")
	for _, r := range remotes {
		b := strings.TrimPrefix(r, "origin/")
		if b == "main" || b == "HEAD" || local[b] {
			continue
		}
		refs = append(refs, ref{label: r, branch: b, refname: r})
	}

	report := &LostReport{}
	for _, rf := range refs {
		if neverMerged[rf.branch] {
			continue // live infrastructure, by design never on main
		}
		ahead, err := f.gitLines(ctx, "log", "--format=%s", "origin/main.."+rf.refname)
		if err != nil {
			continue // unreadable ref is not this tool's failure
		}
		if len(ahead) == 0 {
			continue // nothing to say
		}
		if len(ahead) > f.Limit {
			ahead = ahead[:f.Limit]
		}
		row := BranchRow{Label: rf.label, Branch: rf.branch, Total: len(ahead)}
		for _, s := range ahead {
			if !onMain[s] {
				row.Unmerged++
				if row.FirstMissing == "" {
					row.FirstMissing = s
				}
			}
		}
		row.Age, _ = f.git(ctx, "log", "-1", "--format=%cs", rf.refname)

		switch {
		case row.Unmerged == 0 && owned[rf.branch]:
			report.Owned = append(report.Owned, row) // ownership checked FIRST
		case row.Unmerged == 0:
			report.Superseded = append(report.Superseded, rf.label)
		case owned[rf.branch]:
			report.Owned = append(report.Owned, row)
		case strings.HasPrefix(rf.branch, "park/") || strings.HasPrefix(rf.branch, "parked/"):
			report.Parked = append(report.Parked, row)
		default:
			report.Lost = append(report.Lost, row)
			report.LostTotal += row.Unmerged
		}
	}
	return report, nil
}
