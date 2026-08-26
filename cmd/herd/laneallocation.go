package main

import (
	"sort"
	"strings"
)

// LaneAllocation is how many task surfaces one lane is holding.
type LaneAllocation struct {
	Lane      string   `json:"lane"`
	Home      string   `json:"home,omitempty"`
	TaskPaths []string `json:"task_paths"`
	Excess    int      `json:"excess"`
}

// laneAllocations reports lanes holding MORE THAN ONE mutable task worktree.
//
// FAC-676: the contract is one standing lane = one mutable task worktree plus
// its resident home. Nothing enforced or even measured it, so lanes accumulated
// surfaces silently. Measured on the live repository, 12 lanes over-allocated:
// perf-cost-guard held 15 worktrees, docs-custodian 10, defi-crusader 8.
//
// That is the accumulation MECHANISM, distinct from the retirement leak FAC-672
// closed. Retirement removes surfaces whose work landed; this names lanes taking
// new surfaces faster than they finish the old ones. Fixing only the first means
// sweeping forever.
//
// It REPORTS and never deletes. A lane's extra worktrees may hold real unmerged
// work -- 105 of them do -- and an enforcement that reclaimed them would destroy
// exactly what the operator excluded. The right response is to merge or close
// them, which is a decision about work, not about capacity.
//
// The resident home is excluded from the count: a lane is entitled to it, and
// counting it would report every healthy lane as over-allocated by one.
func laneAllocations(entries []worktreeEntry, lanes []string) []LaneAllocation {
	byLane := map[string]*LaneAllocation{}
	for _, e := range entries {
		if e.IsMain || e.Detached || strings.TrimSpace(e.Branch) == "" {
			continue
		}
		lane := laneOwning(e.Branch, e.Path, lanes)
		if lane == "" {
			continue
		}
		a := byLane[lane]
		if a == nil {
			a = &LaneAllocation{Lane: lane}
			byLane[lane] = a
		}
		if isResidentHome(e.Branch, e.Path) {
			// Entitled, and not a task surface. Counting it would report every
			// healthy lane as over-allocated by one.
			if a.Home == "" {
				a.Home = e.Path
			}
			continue
		}
		a.TaskPaths = append(a.TaskPaths, e.Path)
	}
	var out []LaneAllocation
	for _, a := range byLane {
		if len(a.TaskPaths) <= 1 {
			continue // one task worktree is the contract, not a violation
		}
		sort.Strings(a.TaskPaths)
		a.Excess = len(a.TaskPaths) - 1
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Excess != out[j].Excess {
			return out[i].Excess > out[j].Excess
		}
		return out[i].Lane < out[j].Lane
	})
	return out
}

// laneOwning attributes a worktree to a lane by longest match, so a lane whose
// name prefixes another cannot claim the other's surfaces -- the same boundary
// rule FAC-660 needed for agent identity.
func laneOwning(branch, path string, lanes []string) string {
	b := strings.ToLower(branch)
	p := strings.ToLower(path)
	best := ""
	for _, l := range lanes {
		l = strings.ToLower(strings.TrimSpace(l))
		if l == "" {
			continue
		}
		if !strings.Contains(b, l) && !strings.Contains(p, l) {
			continue
		}
		if len(l) > len(best) {
			best = l
		}
	}
	return best
}
