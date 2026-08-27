package main

import (
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// resolveSendTarget maps a standing lane role (e.g. review-harvest-supervisor)
// to the live Herdr agent name (forge-review-harvest-su-<digest>) when one is
// present. FAC-617: herd send previously only tried exact name or naive
// "forge-"+label, which is not AgentNameForRepository and misses truncated
// digest names. Bare roles that match no live agent are returned unchanged so
// herdr.Send still fails closed with the FAC-597 census message.
func resolveSendTarget(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || strings.Contains(target, ":") || strings.HasPrefix(target, "forge-") {
		return target
	}
	agents, err := herdr.AgentList()
	if err != nil || len(agents) == 0 {
		return target
	}
	for _, a := range agents {
		if a.Name == target || a.PaneID == target {
			return target
		}
	}
	sa := make([]standing.Agent, 0, len(agents))
	for _, a := range agents {
		sa = append(sa, standing.Agent{Name: a.Name, Status: a.Status, PaneID: a.PaneID, Workspace: a.Workspace, Cwd: a.Cwd})
	}
	repo := ""
	if cfg, cfgErr := config.LoadConfig(".herd/herd.yaml"); cfgErr == nil {
		repo = repositoryIdentityForLaunch(cfg)
	}
	if name, live := standing.LiveAgentName(sa, target, repo); live {
		return name
	}
	// Repo identity may be unavailable in tests or bare checkouts; still try
	// the repository-qualified form against each live forge-* agent by probing
	// whether any agent's name equals AgentNameForRepository for a synthetic
	// identity that reproduces truncation (long lane names).
	if name := matchTruncatedStandingName(target, agents); name != "" {
		return name
	}
	return target
}

// matchTruncatedStandingName finds a unique live forge-* agent whose name is
// the AgentNameForRepository form of lane for some repository identity. Used
// when AuthenticatedRepositoryIdentity is unavailable but a truncated standing
// name is live (the perf-cost-guard failure mode).
func matchTruncatedStandingName(lane string, agents []herdr.AgentEntry) string {
	var hit string
	for _, a := range agents {
		name := strings.TrimSpace(a.Name)
		if !strings.HasPrefix(name, standing.ForgePrefix) {
			continue
		}
		// Reconstruct: AgentNameForRepository(lane, repo) must equal name for
		// the repo digest embedded in the live name. Brute-force is unnecessary:
		// LiveAgentName already covered known repo. Here accept a unique agent
		// whose readable prefix matches the truncated standing form.
		sample := standing.AgentNameForRepository(lane, "fac-617-truncation-probe")
		if name == sample {
			if hit != "" && hit != name {
				return ""
			}
			hit = name
			continue
		}
		// Different repos change only the digest suffix; compare without suffix.
		if standingNamePrefix(sample) != "" && standingNamePrefix(sample) == standingNamePrefix(name) {
			if hit != "" && hit != name {
				return ""
			}
			hit = name
		}
	}
	return hit
}

func standingNamePrefix(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndex(name, "-"); i > 0 {
		suf := name[i+1:]
		if len(suf) >= 8 && len(suf) <= 10 && isHex(suf) {
			return name[:i]
		}
	}
	return name
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return len(s) > 0
}
