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
	if err := ReconcileToolChild(tabID, "tab-close"); err != nil {
		return fmt.Errorf("tool-child teardown before tab close %s: %w", tabID, err)
	}
	out, err := runHerdr("tab", "close", tabID)
	if err != nil {
		return fmt.Errorf("herdr tab close %s: %s: %w", tabID, out, err)
	}
	toolChildMu.Lock()
	delete(toolChildByTab, tabID)
	toolChildMu.Unlock()
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

// CloseTabForRef closes the herdr tab of the builder agent working a given
// card ref (FAC-111). The forge calls this once a card reaches done, so the
// workspace does not rot with finished one-off builders — "one agent = one
// tab". The agent is named "task-<ref>" by the dispatcher. Returns nil when
// no such tab exists (already closed / never launched).
func CloseTabForRef(ref string) error {
	agents, err := AgentList()
	if err != nil {
		return err
	}
	want := "task-" + strings.ToLower(ref)
	for _, a := range agents {
		if strings.EqualFold(a.Name, want) && a.TabID != "" {
			return TabClose(a.TabID)
		}
	}
	return nil
}
