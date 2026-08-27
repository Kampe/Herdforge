package review

import (
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/refname"
)

// queuePin is ledger-owned queue recovery evidence. The enqueue row may omit
// branch; the later record row is authoritative for reconstructing it.
type queuePin struct{ sha, branch, lane string }

func queuePins(s LedgerSnapshot, pass map[string]string, veto map[string]bool) []queuePin {
	_ = pass
	records := map[string]string{}
	for _, row := range s.Rows {
		if row.Event == string(EventRecord) && row.Branch != "" {
			records[row.SHA] = row.Branch
		}
	}
	result := map[string]queuePin{}
	for _, row := range s.Queue {
		switch row.Event {
		case string(EventEnqueue):
			if veto[row.SHA] {
				continue
			}
			branch := row.Branch
			if branch == "" {
				branch = records[row.SHA]
			}
			lane := row.Lane
			if lane == "" {
				lane = normalizeQueueLane(branch)
			}
			result[row.SHA] = queuePin{row.SHA, branch, lane}
		case string(EventConsumed), string(EventRevoked):
			delete(result, row.SHA)
		}
	}
	out := make([]queuePin, 0, len(result))
	for _, pin := range result {
		out = append(out, pin)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sha < out[j].sha })
	return out
}

func normalizeQueueLane(branch string) string {
	branch = strings.TrimSpace(branch)
	if i := strings.Index(strings.ToLower(branch), "#"+refname.StandingBranchPrefix); i >= 0 {
		return branch[i+len("#"+refname.StandingBranchPrefix):]
	}
	if refname.IsStandingBranch(branch) {
		// Trim using the canonical prefix length; IsStandingBranch is case-insensitive.
		return strings.TrimSpace(branch)[len(refname.StandingBranchPrefix):]
	}
	return branch
}
