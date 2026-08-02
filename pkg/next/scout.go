package next

import (
	"context"
	"sort"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-114: the scout-planner. It ranks the claimable to-do queue so the forge
// (and the operator) never hand-pick or idle on "what's next". A card is
// CLAIMABLE when it is to-do and none of its blocking dependencies are still
// open. The queue is ordered Priority DESC, Ref ASC — the same invariant the
// dispatcher claims by — so the highest-leverage unblocked work is always at
// the head.

// ScoutRow is one ranked claimable card.
type ScoutRow struct {
	Ref      string
	Title    string
	Priority provider.Priority
	Blocked  bool     // true when held back by an open dependency
	BlockedBy []string // open refs blocking this one (empty when claimable)
}

// Blockers maps a ref to the refs that block it (its open dependencies).
// The provider layer supplies this; an empty map means no dependency data.
type Blockers map[string][]string

// ScoutQueue returns the claimable to-do cards ranked for dispatch, plus the
// blocked ones (so the planner can surface what's waiting on what). openRefs
// is the set of refs not yet done; a blocker still in openRefs holds its
// dependent back.
func ScoutQueue(ctx context.Context, tp provider.TaskProvider, projectID string, blockers Blockers, openRefs map[string]bool) (claimable, blocked []ScoutRow, err error) {
	todo, err := tp.ListTasks(ctx, projectID, "to-do")
	if err != nil {
		return nil, nil, err
	}

	for _, t := range todo {
		var openBlockers []string
		for _, b := range blockers[t.Ref] {
			if openRefs[b] {
				openBlockers = append(openBlockers, b)
			}
		}
		row := ScoutRow{Ref: t.Ref, Title: t.Title, Priority: t.Priority}
		if len(openBlockers) > 0 {
			row.Blocked = true
			row.BlockedBy = openBlockers
			blocked = append(blocked, row)
		} else {
			claimable = append(claimable, row)
		}
	}

	rank := func(rows []ScoutRow) {
		pr := map[provider.Priority]int{
			provider.PriorityUrgent: 4, provider.PriorityHigh: 3,
			provider.PriorityMedium: 2, provider.PriorityLow: 1,
		}
		sort.SliceStable(rows, func(i, j int) bool {
			pi, pj := pr[rows[i].Priority], pr[rows[j].Priority]
			if pi != pj {
				return pi > pj
			}
			return provider.CompareRefs(rows[i].Ref, rows[j].Ref) < 0
		})
	}
	rank(claimable)
	rank(blocked)
	return claimable, blocked, nil
}
