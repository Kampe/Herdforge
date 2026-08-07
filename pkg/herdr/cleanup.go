package herdr

import (
	"fmt"
	"github.com/Kampe/Herdforge/pkg/toolchild"
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

// TabClose is the legacy unfenced close entrypoint. Autonomous Herdforge
// callers (cleanup, forge auto-close, FAC-158 reconciliation) MUST NOT use
// it: FAC-180 requires generation/session compare-and-close via TabCloseCAS
// / CompareAndCloseTab. This function fails closed so a plain tab-id close
// can never recycle-kill a tab that gained a new agent between readback and
// mutation.
//
// Internal launch compensation still uses unexported tabCloseRaw after the
// tool-child lifecycle has already proven the exact pane identity it owns.
func TabClose(tabID string) error {
	return &CloseUnavailableError{
		TabID:  tabID,
		Reason: "FAC-180 atomic generation/session compare-and-close is required; use TabCloseCAS",
	}
}

// Cleanup sweeps the workspace: candidates from live agent list. Dry-run
// returns observe-only candidates; mutation mode is BLOCKED without a
// FAC-180 fenced decision (TabCloseCAS). Never falls back to plain tab close.
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
		errs = append(errs, &CloseUnavailableError{
			TabID:  c.TabID,
			Reason: "automatic close requires FAC-180 compare-and-close evidence via TabCloseCAS",
		})
	}
	return cands, errs
}

// CloseTabForRef closes the herdr tab of the builder agent working a given
// card ref (FAC-111). Legacy name/ref lookup cannot establish the exact
// durable generation/session binding FAC-180 requires, so this path fails
// closed. Callers with a durable reconciliation decision must use TabCloseCAS.
func CloseTabForRef(ref string) error {
	return &CloseUnavailableError{
		TabID:  ref,
		Reason: "legacy ref/name lookup cannot establish exact durable binding; FAC-180 compare-and-close required",
	}
}

// LegacyTabCloseWithLifecycle is retained only for unit tests that pin the
// pre-FAC-180 pane-authority gate. Production autonomous code must not call
// it; it still refuses empty pane authority and never becomes the cleanup path.
func LegacyTabCloseWithLifecycle(tabID string) error {
	if err := ReconcileToolChild(tabID, "tab-close"); err != nil {
		return fmt.Errorf("tool-child teardown before tab close %s: %w", tabID, err)
	}
	lc := lifecycleForTab(tabID)
	paneID := ""
	if concrete, ok := lc.(*toolchild.Lifecycle); ok {
		paneID = concrete.Inventory.Owner.PaneID
	}
	if paneID == "" {
		return fmt.Errorf("tab close %s requires exact lifecycle pane authority", tabID)
	}
	// Even the legacy lifecycle helper refuses plain close after FAC-180:
	// pane authority alone is not a generation fence.
	return &CloseUnavailableError{
		TabID:  tabID,
		Reason: "lifecycle pane authority is not a generation fence; FAC-180 compare-and-close required",
	}
}
