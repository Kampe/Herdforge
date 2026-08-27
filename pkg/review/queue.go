package review

import (
	"github.com/Kampe/Herdforge/pkg/refname"
	"sort"
	"strings"
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
	if marker := "#" + refname.StandingBranchPrefix; strings.Contains(branch, marker) {
		return branch[strings.Index(branch, marker)+len(marker):]
	}
	if strings.HasPrefix(branch, refname.StandingBranchPrefix) {
		return strings.TrimPrefix(branch, refname.StandingBranchPrefix)
	}
	return branch
}
