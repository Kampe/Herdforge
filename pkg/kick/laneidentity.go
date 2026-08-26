package kick

import "strings"

// StandingNamePrefix is the second live spelling a standing lane appears under.
// A lane launched through the standing raiser is "standing-<lane>"; one launched
// through the repository-qualified path is "forge-<lane>-<digest>".
const StandingNamePrefix = "standing-"

// LaneForAgent resolves a LIVE agent name back to the lane it belongs to, or ""
// when it belongs to none.
//
// FAC-660: roster membership was decided by exact string equality, and the two
// sides never spelled a lane the same way. StandingIDs() returns "forge-<lane>"
// with no digest, while a live agent is "forge-<lane>-<10 hex of the repository
// digest>" or "standing-<lane>". So an exact lookup could not match a running
// lane, and every consumer of the roster reported a fleet that was not there:
//
//	herd status   working=1  capacity=14   while pulse reported busy=9
//	herd attention  scanned=false state=UNKNOWN with a full fleet running
//
// Those two numbers describing one fleet at one instant is the tell. The counts
// were not wrong about the agents; they were asking the wrong question about the
// names.
//
// Matching is by LANE, not by string: an agent belongs to lane X when it is
// exactly "forge-X" or "standing-X", or "forge-X-<suffix>" where the suffix is
// the repository digest. The longest matching lane wins, so a lane named
// "review" cannot swallow an agent belonging to "review-harvest".
func LaneForAgent(agentName string, laneIDs []string) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		return ""
	}
	best := ""
	for _, raw := range laneIDs {
		lane := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), ForgePrefix))
		if lane == "" {
			continue
		}
		if !laneMatchesAgent(name, lane) {
			continue
		}
		// Longest lane wins: "review-harvest" must beat "review".
		if len(lane) > len(best) {
			best = lane
		}
	}
	return best
}

func laneMatchesAgent(name, lane string) bool {
	for _, prefix := range []string{ForgePrefix, StandingNamePrefix} {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[len(prefix):]
		if rest == lane {
			return true
		}
		// forge-<lane>-<digest>: the remainder after the lane must be a
		// hyphen-separated suffix, never a longer lane name that merely starts
		// with this one.
		if strings.HasPrefix(rest, lane+"-") {
			return true
		}
	}
	return false
}

// LiveLaneIDs returns the subset of laneIDs that have at least one live agent,
// so a caller can report roster coverage instead of a bare count that cannot be
// reconciled against anything.
func LiveLaneIDs(agentNames []string, laneIDs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range agentNames {
		if lane := LaneForAgent(n, laneIDs); lane != "" && !seen[lane] {
			seen[lane] = true
			out = append(out, lane)
		}
	}
	return sortedUnique(out)
}
