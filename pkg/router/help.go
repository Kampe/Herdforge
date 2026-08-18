package router

import (
	"fmt"
	"sort"
	"strings"
)

// HelpKind identifies the capability needed to make progress on a block.
type HelpKind string

const (
	HelpKindReview         HelpKind = "review"
	HelpKindHarvest        HelpKind = "harvest"
	HelpKindMerge          HelpKind = "merge"
	HelpKindBoard          HelpKind = "board"
	HelpKindImplementation HelpKind = "implementation"
)

// HelpCandidate is a currently available helper advertised by the fleet.
// Capabilities are explicit so routing never guesses from a pane name.
type HelpCandidate struct {
	Identity     string
	Family       string
	Capabilities []string
	Available    bool
}

type HelpRoute struct {
	Target     string `json:"target"`
	Family     string `json:"family,omitempty"`
	Capability string `json:"capability"`
	Escalated  bool   `json:"escalated"`
}

// HelpKindForReason maps operator-facing blocked details to the narrowest
// control-plane capability. Unknown details are implementation blockers.
func HelpKindForReason(reason string) HelpKind {
	reason = strings.ToLower(reason)
	switch {
	case strings.Contains(reason, "review"), strings.Contains(reason, "harvest"):
		return HelpKindReview
	case strings.Contains(reason, "merge"), strings.Contains(reason, "board"), strings.Contains(reason, "approve"):
		return HelpKindMerge
	default:
		return HelpKindImplementation
	}
}

// DefaultHelpRoute supplies the standing authority identities used when a
// blocked lane has not supplied a live helper roster.
func DefaultHelpRoute(kind HelpKind) (HelpRoute, error) {
	candidates := []HelpCandidate{
		{Identity: "review-supervisor", Family: "supervisor", Capabilities: []string{"review"}, Available: true},
		{Identity: "coordinator", Family: "coordinator", Capabilities: []string{"merge"}, Available: true},
		{Identity: "domain-lane", Family: "implementation", Capabilities: []string{"implementation"}, Available: true},
	}
	return SelectHelpRoute(kind, candidates)
}

// SelectHelpRoute chooses the narrowest available capable helper. If none is
// capable, it returns one bounded fleet escalation rather than broadcasting
// to every lane. Ties are lexical for deterministic replay and tests.
func SelectHelpRoute(kind HelpKind, candidates []HelpCandidate) (HelpRoute, error) {
	capability := strings.ToLower(strings.TrimSpace(string(kind)))
	switch capability {
	case string(HelpKindReview), string(HelpKindHarvest):
		capability = "review"
	case string(HelpKindMerge), string(HelpKindBoard):
		capability = "merge"
	case string(HelpKindImplementation):
	default:
		return HelpRoute{}, fmt.Errorf("router: unknown help capability %q", kind)
	}
	eligible := make([]HelpCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if !candidate.Available || strings.TrimSpace(candidate.Identity) == "" || !hasHelpCapability(candidate.Capabilities, capability) {
			continue
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 {
		return HelpRoute{Target: "fleet", Capability: capability, Escalated: true}, nil
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if len(eligible[i].Capabilities) != len(eligible[j].Capabilities) {
			return len(eligible[i].Capabilities) < len(eligible[j].Capabilities)
		}
		return strings.ToLower(eligible[i].Identity) < strings.ToLower(eligible[j].Identity)
	})
	best := eligible[0]
	return HelpRoute{Target: best.Identity, Family: best.Family, Capability: capability}, nil
}

func hasHelpCapability(capabilities []string, wanted string) bool {
	for _, capability := range capabilities {
		if strings.EqualFold(strings.TrimSpace(capability), wanted) {
			return true
		}
	}
	return false
}
