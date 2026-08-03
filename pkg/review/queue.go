package review

import "sort"

// queuePin is ledger-owned queue recovery evidence. The enqueue row may omit
// branch; the later record row is authoritative for reconstructing it.
type queuePin struct{ sha, branch string }

func queuePins(s LedgerSnapshot, pass map[string]string, veto map[string]bool) []queuePin {
	_ = pass
	records := map[string]string{}
	for _, row := range s.Rows {
		if row.Event == string(EventRecord) && row.Branch != "" {
			records[row.SHA] = row.Branch
		}
	}
	consumed := map[string]bool{}
	for _, row := range s.Queue {
		if row.Event == string(EventConsumed) {
			consumed[row.SHA] = true
		}
	}
	result := map[string]queuePin{}
	for _, row := range s.Queue {
		if row.Event != string(EventEnqueue) || consumed[row.SHA] || veto[row.SHA] {
			continue
		}
		branch := row.Branch
		if branch == "" {
			branch = records[row.SHA]
		}
		result[row.SHA] = queuePin{row.SHA, branch}
	}
	out := make([]queuePin, 0, len(result))
	for _, pin := range result {
		out = append(out, pin)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].sha < out[j].sha })
	return out
}
