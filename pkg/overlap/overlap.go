// Package overlap surfaces files that more than one unmerged branch is
// editing, before those branches collide at merge. It is the Go port of
// bin/herd-overlap, deliberately tokenized to the same "deliberately dumb,
// file-level" contract: two branches touching one file is usually fine; two
// branches touching one file for a week, while neither can see the other, is
// two people solving the same problem twice. The output is a prompt to go
// look, never a verdict.
package overlap

import (
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
)

// DefaultExclusions are append-only registries that collide by construction:
// everyone adds a row or a case, and the matrix/dedup gates already handle the
// mechanics. Listing them buries the real signal, which is two branches
// editing the same LOGIC.
var DefaultExclusions = []string{
	"docs/testing/MASTER-TEST-PLAN.md",
	"pnpm-lock.yaml",
	"docs/BUILD-STATUS.md",
	"docs/QUALITY.md",
	"docs/API-SURFACE-AUDIT.md",
	"docs/TOOLING-INVENTORY.md",
	"bin/herd-selftest",
	"docs/prompts/orchestrator.md",
	"AGENTS.md",
	"CLAUDE.md",
}

// Overlap performs the unmerged-branch convergence census against a local git
// root. It never uses pkg/harvest or pkg/worktree: this is a local tips
// census, not a worktree walk.
type Overlap struct {
	RepoRoot string
}

// NewOverlap returns an Overlap rooted at repoRoot.
func NewOverlap(repoRoot string) *Overlap {
	return &Overlap{RepoRoot: repoRoot}
}

// FileOverlap is one file edited by minBranches or more distinct unmerged
// tips. Branches preserves first-seen order per tip (git for-each-ref order),
// so results are deterministic across runs.
type FileOverlap struct {
	File     string
	Branches []string
}

// MarshalJSON emits the reference CLI's file-mode JSON contract:
// {"file":"<path>","branches":<count>,"owners":[...]}.
func (fo FileOverlap) MarshalJSON() ([]byte, error) {
	type wire struct {
		File     string   `json:"file"`
		Branches int      `json:"branches"`
		Owners   []string `json:"owners"`
	}
	return json.Marshal(wire{File: fo.File, Branches: len(fo.Branches), Owners: fo.Branches})
}

// branches lists refs/heads/ in git's deterministic refname order.
func (o *Overlap) branches(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	cmd.Dir = o.RepoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// aheadCount is commits on branch not reaching base (git rev-list --count
// base..branch). A trailing-non-zero base (missing origin/main) yields an
// error here; callers treat it as "not ahead", matching the zsh reference's
// `|| print 0` guard.
func (o *Overlap) aheadCount(ctx context.Context, base, branch string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--count", base+".."+branch)
	cmd.Dir = o.RepoRoot
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	return parseCount(strings.TrimSpace(string(out))), nil
}

func parseCount(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// unmergedTips returns the ordered list of branch names participating in the
// census: refs/heads minus main/master, those still ahead of base, de-duped
// by distinct tip SHA so park-branch triplicates (-current/-<short>/-<full>)
// collapse to one card. Order follows git for-each-ref so results are
// deterministic across runs.
func (o *Overlap) unmergedTips(ctx context.Context, base string) ([]string, error) {
	branches, err := o.branches(ctx)
	if err != nil {
		return nil, err
	}
	seenTip := make(map[string]bool)
	var tips []string
	for _, b := range branches {
		if b == "main" || b == "master" {
			continue
		}
		// Only branches carrying work not already on main. A content-merged
		// branch is not competing with anyone.
		ahead, err := o.aheadCount(ctx, base, b)
		if err != nil {
			continue
		}
		if ahead <= 0 {
			continue
		}
		tipCmd := exec.CommandContext(ctx, "git", "rev-parse", b)
		tipCmd.Dir = o.RepoRoot
		tipOut, tipErr := tipCmd.Output()
		if tipErr != nil {
			continue
		}
		tip := strings.TrimSpace(string(tipOut))
		if tip == "" || seenTip[tip] {
			continue
		}
		seenTip[tip] = true
		tips = append(tips, b)
	}
	return tips, nil
}

// diffNameOnly is files changed on branch relative to base (3-dot merge-base
// diff); like the reference it swallows a failed diff for a branch and returns
// no files.
func (o *Overlap) diffNameOnly(ctx context.Context, base, branch string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", base+"..."+branch)
	cmd.Dir = o.RepoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// FileOverlaps returns the files touched by >= minBranches distinct partition
// tips that are still ahead of mainRef, plus the count of distinct tips
// scanned. exclusions == nil selects DefaultExclusions; a non-nil slice is
// used verbatim. The returned slice is ranked by descending owner count then
// file name, so the worst convergence reads first.
func (o *Overlap) FileOverlaps(ctx context.Context, mainRef string, minBranches int, exclusions []string) ([]FileOverlap, int, error) {
	if exclusions == nil {
		exclusions = DefaultExclusions
	}
	excluded := make(map[string]struct{}, len(exclusions))
	for _, e := range exclusions {
		excluded[e] = struct{}{}
	}

	tips, err := o.unmergedTips(ctx, mainRef)
	if err != nil {
		return nil, 0, err
	}

	touched := make(map[string][]string)
	var order []string

	// Park branches triplicate: the same tip is written as `-current`,
	// `-<short-sha>` AND `-<full-sha>`, so one card can present as three
	// competing branches and manufacture an overlap that is really one piece
	// of work. unmergedTips already de-dupes by distinct TIP.
	for _, b := range tips {
		files, err := o.diffNameOnly(ctx, mainRef, b)
		if err != nil {
			continue
		}
		for _, f := range files {
			if _, skip := excluded[f]; skip {
				continue
			}
			if _, ok := touched[f]; !ok {
				order = append(order, f)
			}
			touched[f] = append(touched[f], b)
		}
	}

	var hot []FileOverlap
	for _, f := range order {
		if len(touched[f]) >= minBranches {
			hot = append(hot, FileOverlap{File: f, Branches: touched[f]})
		}
	}
	// Rank by branch count so the worst convergence reads first; ties sort
	// by path so output is deterministic.
	sort.SliceStable(hot, func(i, j int) bool {
		if len(hot[i].Branches) != len(hot[j].Branches) {
			return len(hot[i].Branches) > len(hot[j].Branches)
		}
		return hot[i].File < hot[j].File
	})
	return hot, len(tips), nil
}
