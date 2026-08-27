package review

import (
	"strings"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

// tipIndexAfter returns the index of the first tip to probe AFTER afterSHA
// (FAC-605 resume cursor). Empty afterSHA or a cursor no longer present in
// the tip set means start at 0 (full restart).
func tipIndexAfter(tips []harvest.UnmergedWork, afterSHA string) int {
	afterSHA = strings.TrimSpace(afterSHA)
	if afterSHA == "" {
		return 0
	}
	for i, u := range tips {
		if len(u.Unmerged) > 0 && u.Unmerged[0] == afterSHA {
			return i + 1
		}
	}
	return 0
}

// buildTipSet is the SINGLE source of truth for what review-scan iterates.
//
// FAC-562: the tip set is worktree SHAs PLUS queued ledger pins. A budget
// planner that counted only the worktree half reported 170 tips while the scan
// traversed 402 (170 worktree + 232 queued). Two independent constructions of
// the same set is exactly how that denominator drifted, so both the scan and
// the budget planner now call this.
//
// Order matters and is preserved: worktree tips first, then queued pins, each
// deduplicated by SHA across the whole set.
func buildTipSet(unmerged []harvest.UnmergedWork, queued []queuePin) ([]harvest.UnmergedWork, map[string]string) {
	queueLanes := map[string]string{}
	seen := map[string]bool{}
	tips := make([]harvest.UnmergedWork, 0, len(unmerged)+len(queued))
	for _, u := range unmerged {
		for _, sha := range u.Unmerged {
			if sha == "" || seen[sha] {
				continue
			}
			seen[sha] = true
			tips = append(tips, harvest.UnmergedWork{
				WorktreePath: u.WorktreePath, Branch: u.Branch, Unmerged: []string{sha},
			})
		}
	}
	for _, q := range queued {
		queueLanes[q.sha] = q.lane
		if q.sha == "" || seen[q.sha] {
			continue
		}
		seen[q.sha] = true
		tips = append(tips, harvest.UnmergedWork{Branch: q.branch, Unmerged: []string{q.sha}})
	}
	return tips, queueLanes
}

// PlanTipCount returns exactly how many tips a Scan over unmerged will iterate,
// including queued ledger pins. Callers budget on this, never on the worktree
// count: those differ by the entire queue.
func (d *Drain) PlanTipCount(unmerged []harvest.UnmergedWork) (int, error) {
	if d.Ledger == nil {
		d.Ledger = OpenLedger(d.LedgerPath)
	}
	snap, err := d.Ledger.Snapshot()
	if err != nil {
		return 0, err
	}
	pass := d.Ledger.passMap(snap.Rows)
	tips, _ := buildTipSet(unmerged, queuePins(snap, pass, snap.Vetoed()))
	return len(tips), nil
}

// shortSHA abbreviates an object name for operator-facing labels.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
