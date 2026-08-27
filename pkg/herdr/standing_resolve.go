package herdr

import (
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/standing"
)

// ResolveStandingLaneMatches maps a bare standing lane role to live Herdr
// agents that hold that lane (FAC-617).
//
// The naive "forge-"+label concatenation is NOT used: live names come from
// standing.AgentName / AgentNameForRepository (truncation + digest). Callers
// of Send must go through this (or LiveAgentName) so a second path cannot
// drift back to the naive rule.
//
// Returns:
//   - one match: that agent
//   - zero matches: empty slice (caller emits noAgentFoundError)
//   - multiple matches: empty slice + AmbiguousStandingTargetError
func ResolveStandingLaneMatches(target string, agents []AgentEntry, repository string) ([]AgentEntry, error) {
	target = strings.TrimSpace(target)
	if target == "" || strings.Contains(target, ":") || strings.HasPrefix(target, standing.ForgePrefix) {
		return nil, nil
	}
	wanted := map[string]struct{}{}
	sa := make([]standing.Agent, 0, len(agents))
	for _, a := range agents {
		sa = append(sa, standing.Agent{
			Name: a.Name, Status: a.Status, PaneID: a.PaneID,
			Workspace: a.Workspace, Cwd: a.Cwd,
		})
	}
	if name, live := standing.LiveAgentName(sa, target, repository); live {
		wanted[name] = struct{}{}
	}
	// Legacy forge-<lane> without digest (AgentName) when that form is live.
	wanted[standing.AgentName(target)] = struct{}{}
	// Truncated digest form: readable prefix shared across repo digests.
	sample := standing.AgentNameForRepository(target, "fac-617-truncation-probe")
	samplePrefix := standingNamePrefix(sample)

	var matches []AgentEntry
	seen := map[string]bool{}
	for _, a := range agents {
		name := strings.TrimSpace(a.Name)
		if name == "" || seen[name] {
			continue
		}
		_, wantExact := wanted[name]
		prefixHit := samplePrefix != "" && standingNamePrefix(name) == samplePrefix &&
			strings.HasPrefix(name, standing.ForgePrefix)
		if wantExact || prefixHit {
			seen[name] = true
			matches = append(matches, a)
		}
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, m := range matches {
			names = append(names, m.Name)
		}
		return nil, &AmbiguousStandingTargetError{Target: target, Candidates: names}
	}
	return matches, nil
}

// AmbiguousStandingTargetError names every live candidate so the operator can
// pick deliberately instead of getting a silent guess.
type AmbiguousStandingTargetError struct {
	Target     string
	Candidates []string
}

func (e *AmbiguousStandingTargetError) Error() string {
	return fmt.Sprintf("agent '%s' is ambiguous: standing-lane resolution matches %d live agents (%s); specify the registered name explicitly",
		e.Target, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

func standingNamePrefix(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndex(name, "-"); i > 0 {
		suf := name[i+1:]
		if len(suf) >= 8 && len(suf) <= 10 && isHexDigits(suf) {
			return name[:i]
		}
	}
	return name
}

func isHexDigits(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return len(s) > 0
}
