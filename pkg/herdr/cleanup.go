package herdr

import (
	"fmt"
	"strings"
)

// Port of bin/herd-cleanup: close finished / orphan herdr tabs so the
// workspace does not rot. Binding policy: one agent = one tab — when an agent
// is done, CLOSE THE TAB, not only the pane.
//
// Never closes: standing lanes (re-kicked by design), working agents,
// anything named like an orchestrator, or unnamed panes (the operator's own
// terminals live there).

// CleanupCandidate is one tab the sweep would close, with the reason.
type CleanupCandidate struct {
	Name   string `json:"name"`
	TabID  string `json:"tab_id"`
	PaneID string `json:"pane_id"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

// SelectCleanupCandidates is the pure policy: named, non-standing,
// non-orchestrator agents whose status is done or idle (a finished one-off
// builder that will never be re-kicked). Pure so tests pin the policy.
func SelectCleanupCandidates(agents []AgentEntry, standing map[string]bool) []CleanupCandidate {
	var out []CleanupCandidate
	for _, a := range agents {
		if a.Name == "" {
			continue // unnamed panes are the operator's, never ours to close
		}
		if standing[a.Name] {
			continue // standing fleet is re-kicked by design
		}
		if strings.Contains(strings.ToLower(a.Name), "orchestrator") {
			continue
		}
		if a.Status != "done" && a.Status != "idle" {
			continue // working/starting agents are alive
		}
		out = append(out, CleanupCandidate{
			Name:   a.Name,
			TabID:  a.TabID,
			PaneID: a.PaneID,
			Status: a.Status,
			Reason: fmt.Sprintf("named one-off agent with status %s (one agent = one tab)", a.Status),
		})
	}
	return out
}

// TabClose closes a tab over the herdr socket API.
func TabClose(tabID string) error {
	out, err := runHerdr("tab", "close", tabID)
	if err != nil {
		return fmt.Errorf("herdr tab close %s: %s: %w", tabID, out, err)
	}
	return nil
}

// Cleanup sweeps the workspace: candidates from live agent list, closed
// unless dryRun. Returns candidates and per-tab close errors.
func Cleanup(standing map[string]bool, dryRun bool) ([]CleanupCandidate, []error) {
	agents, err := AgentList()
	if err != nil {
		return nil, []error{err}
	}
	cands := SelectCleanupCandidates(agents, standing)
	if dryRun {
		return cands, nil
	}
	var errs []error
	for _, c := range cands {
		if err := TabClose(c.TabID); err != nil {
			errs = append(errs, err)
		}
	}
	return cands, errs
}
