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

// TabClass is the reconciler's explicit projection of a tab. Unknown facts
// are intentionally represented as BLOCKED rather than being treated as idle.
type TabClass string

const (
	TabActive          TabClass = "active"
	TabStanding        TabClass = "standing"
	TabPreservedReview TabClass = "preserved-review"
	TabUserShell       TabClass = "user-shell"
	TabSafeFinished    TabClass = "safe-finished"
	TabSafeOrphan      TabClass = "safe-orphan"
	TabBlocked         TabClass = "unknown/BLOCKED"
)

// EvidenceState distinguishes an authoritative absence from an unread or
// unqueried source. Unknown is the zero-value and never permits cleanup.
type EvidenceState string

const (
	EvidenceUnknown EvidenceState = "unknown"
	EvidenceAbsent  EvidenceState = "absent"
	EvidencePresent EvidenceState = "present"
	EvidenceError   EvidenceState = "error"
)

type SourceEvidence struct {
	State  EvidenceState `json:"state"`
	Detail string        `json:"detail,omitempty"`
}

type TabEvidence struct {
	Board, Agent, Lifecycle, Worktree, Review, Mail, Process, Protection SourceEvidence
}

// WorktreeEvidence is the minimum local evidence needed before a tab may be
// closed. Known must be true: an absent or unreadable worktree is not proof
// that it is safe to reap.
type WorktreeEvidence struct {
	Known         bool `json:"known"`
	Dirty         bool `json:"dirty"`
	UniqueCommits bool `json:"unique_commits"`
	UniqueRefs    bool `json:"unique_refs"`
}

// TabObservation is a read-only snapshot assembled from Herdr, board,
// lifecycle, review/mail, and git evidence. It is deliberately independent
// of the live socket so fixtures can exercise the policy hermetically.
type TabObservation struct {
	TabID               string
	Generation          string
	Label               string
	AgentName           string
	AgentStatus         string
	SessionID           string
	SessionGeneration   string
	TaskRef             string
	TaskStatus          string
	Standing            bool
	ExplicitUserShell   bool
	PendingReview       bool
	PendingOutbox       bool
	PendingCallback     bool
	ActiveReview        bool
	UnsupersededVerdict bool
	Protected           bool
	ProviderError       string
	Worktree            WorktreeEvidence
	Evidence            TabEvidence
}

// TabDecision is durable, structured reconciliation evidence. CloseEligible
// is true only for the two safe classes and a complete preservation proof.
type TabDecision struct {
	TabID         string   `json:"tab_id"`
	Generation    string   `json:"generation"`
	Class         TabClass `json:"class"`
	CloseEligible bool     `json:"close_eligible"`
	Evidence      []string `json:"evidence"`
}

func NormalizeTaskStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "to-do", "todo", "to do":
		return "to-do"
	case "in-progress", "in progress", "working":
		return "in-progress"
	case "in-review", "in review", "reviewing":
		return "in-review"
	case "done", "complete", "completed":
		return "done"
	case "recovering":
		return "recovering"
	case "blocked":
		return "blocked"
	default:
		return "unknown"
	}
}

// AssembleTabObservation binds the independent source readbacks into the
// policy input. Callers must supply explicit Evidence states; omitted source
// results remain Unknown and therefore BLOCKED.
func AssembleTabObservation(tab TabInfo, agent AgentEntry, evidence TabEvidence) TabObservation {
	ref := agent.TaskRef
	if ref == "" && strings.HasPrefix(strings.ToLower(agent.Name), "task-") {
		ref = strings.ToUpper(strings.TrimPrefix(strings.ToLower(agent.Name), "task-"))
	}
	return TabObservation{
		TabID: tab.ID, Generation: tab.Generation, Label: tab.Label,
		AgentName: agent.Name, AgentStatus: agent.Status, SessionID: agent.SessionID,
		SessionGeneration: agent.SessionGeneration, TaskRef: ref, TaskStatus: NormalizeTaskStatus(agent.TaskStatus),
		Standing: false, ExplicitUserShell: agent.ExplicitUserShell,
		PendingReview: agent.PendingReview, PendingOutbox: agent.PendingOutbox,
		PendingCallback: agent.PendingCallback, ActiveReview: agent.ActiveReview,
		UnsupersededVerdict: agent.UnsupersededVerdict, Protected: agent.Protected,
		Evidence: evidence,
		Worktree: WorktreeEvidence{Known: agent.WorktreeKnown, Dirty: agent.Dirty,
			UniqueCommits: agent.UniqueCommits, UniqueRefs: agent.UniqueRefs},
	}
}

type FleetStatus struct {
	Working  int
	Capacity int
	Unknown  int
	Classes  map[TabClass]int
}

// ProjectFleetStatus excludes user shells, standing tabs, and BLOCKED tabs
// from working capacity. Unknown is surfaced explicitly for operator audits.
func ProjectFleetStatus(decisions []TabDecision, maxLanes int) FleetStatus {
	p := FleetStatus{Classes: map[TabClass]int{}}
	for _, d := range decisions {
		p.Classes[d.Class]++
		if d.Class == TabActive {
			p.Working++
		}
		if d.Class == TabBlocked {
			p.Unknown++
		}
	}
	p.Capacity = maxLanes - p.Working
	if p.Capacity < 0 {
		p.Capacity = 0
	}
	return p
}

func completeEvidence(e TabEvidence) (bool, string) {
	checks := []struct {
		name   string
		source SourceEvidence
	}{
		{"board", e.Board}, {"agent", e.Agent}, {"lifecycle", e.Lifecycle},
		{"worktree", e.Worktree}, {"review", e.Review}, {"mail", e.Mail},
		{"process", e.Process}, {"protection", e.Protection},
	}
	for _, c := range checks {
		if c.source.State == EvidenceError {
			return false, c.name + " source error"
		}
		if c.source.State != EvidenceAbsent && c.source.State != EvidencePresent {
			return false, c.name + " source was not authoritatively read"
		}
	}
	return true, ""
}

// ReconcileTabs classifies every supplied tab. It never uses label text or a
// board status as a substitute for session/worktree/review evidence.
func ReconcileTabs(tabs []TabObservation) []TabDecision {
	out := make([]TabDecision, 0, len(tabs))
	for _, tab := range tabs {
		d := TabDecision{TabID: tab.TabID, Generation: tab.Generation}
		blocked := func(reason string) {
			d.Class = TabBlocked
			d.Evidence = append(d.Evidence, "BLOCKED: "+reason)
		}
		if strings.TrimSpace(tab.TabID) == "" {
			blocked("missing exact tab id")
			out = append(out, d)
			continue
		}
		if ok, reason := completeEvidence(tab.Evidence); !ok {
			blocked(reason)
			out = append(out, d)
			continue
		}
		if tab.ProviderError != "" {
			blocked("provider error: " + tab.ProviderError)
			out = append(out, d)
			continue
		}
		if tab.Protected {
			blocked("protected incident or audit marker")
			out = append(out, d)
			continue
		}
		if tab.ExplicitUserShell {
			d.Class = TabUserShell
			d.Evidence = append(d.Evidence, "explicit user-shell marker")
			out = append(out, d)
			continue
		}
		if tab.Standing {
			d.Class = TabStanding
			d.Evidence = append(d.Evidence, "standing role")
			out = append(out, d)
			continue
		}
		if tab.PendingReview || tab.ActiveReview || tab.UnsupersededVerdict {
			d.Class = TabPreservedReview
			d.Evidence = append(d.Evidence, "review evidence is still live")
			out = append(out, d)
			continue
		}
		if tab.PendingOutbox || tab.PendingCallback {
			blocked("pending mailbox/outbox callback")
			out = append(out, d)
			continue
		}
		if tab.AgentStatus == "working" || tab.AgentStatus == "starting" || (tab.SessionID != "" && tab.AgentStatus != "done") {
			if tab.SessionID == "" || tab.SessionGeneration == "" || tab.Generation == "" || tab.SessionGeneration != tab.Generation {
				blocked("active or recycled session lacks matching generation")
			} else {
				d.Class = TabActive
				d.Evidence = append(d.Evidence, "matching active session")
			}
			out = append(out, d)
			continue
		}
		if !tab.Worktree.Known {
			blocked("worktree evidence is unknown")
			out = append(out, d)
			continue
		}
		if tab.SessionID != "" && (tab.SessionGeneration == "" || tab.Generation == "" || tab.SessionGeneration != tab.Generation) {
			blocked("terminal session lacks exact session-generation fence")
			out = append(out, d)
			continue
		}
		if tab.Worktree.Dirty || tab.Worktree.UniqueCommits || tab.Worktree.UniqueRefs {
			blocked("dirty or unique work exists")
			out = append(out, d)
			continue
		}
		if tab.Generation == "" {
			blocked("missing immutable tab generation")
			out = append(out, d)
			continue
		}
		status := NormalizeTaskStatus(tab.TaskStatus)
		if tab.TaskRef != "" && status == "done" {
			d.Class = TabSafeFinished
			d.Evidence = append(d.Evidence, "done task, no session, clean worktree, no pending evidence")
		} else if tab.TaskRef != "" && status == "to-do" {
			d.Class = TabSafeOrphan
			d.Evidence = append(d.Evidence, "task-shaped orphan with no matching session and no preserved work")
		} else if tab.TaskRef == "" {
			d.Class = TabUserShell
			d.Evidence = append(d.Evidence, "unowned shell")
		} else {
			blocked("task lifecycle/board status is not terminal or orphanable")
		}
		d.CloseEligible = d.Class == TabSafeFinished || d.Class == TabSafeOrphan
		out = append(out, d)
	}
	return out
}

// AuthorizeClose performs the final pure readback check and returns the
// request FAC-180 must enforce atomically. It does not close anything.
func AuthorizeClose(decision TabDecision, current TabObservation) (CloseRequest, error) {
	if !decision.CloseEligible || (decision.Class != TabSafeFinished && decision.Class != TabSafeOrphan) {
		return CloseRequest{}, fmt.Errorf("tab %s: close not authorized (%s)", decision.TabID, decision.Class)
	}
	if decision.TabID == "" || decision.Generation == "" {
		return CloseRequest{}, fmt.Errorf("tab close: immutable tab generation is required")
	}
	if current.TabID != decision.TabID || current.Generation != decision.Generation {
		return CloseRequest{}, fmt.Errorf("tab %s: generation changed; refusing close", decision.TabID)
	}
	currentDecision := ReconcileTabs([]TabObservation{current})[0]
	if !currentDecision.CloseEligible || currentDecision.Class != decision.Class {
		return CloseRequest{}, fmt.Errorf("tab %s: readback no longer proves safe close", decision.TabID)
	}
	return CloseRequest{TabID: current.TabID, Generation: current.Generation,
		SessionID: current.SessionID, SessionGeneration: current.SessionGeneration}, nil
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
		ref := a.TaskRef
		if ref == "" && strings.HasPrefix(strings.ToLower(a.Name), "task-") {
			ref = strings.ToUpper(strings.TrimPrefix(strings.ToLower(a.Name), "task-"))
		}
		status := a.TaskStatus
		if status == "" && a.Status == "done" {
			status = "done"
		}
		decision := ReconcileTabs([]TabObservation{{
			TabID: a.TabID, Generation: a.Generation, Label: a.Name,
			AgentName: a.Name, AgentStatus: a.Status, SessionID: a.SessionID,
			SessionGeneration: a.SessionGeneration, TaskRef: ref, TaskStatus: status,
			Standing: standing[a.Name], ExplicitUserShell: a.ExplicitUserShell,
			PendingReview: a.PendingReview, PendingOutbox: a.PendingOutbox,
			PendingCallback: a.PendingCallback, ActiveReview: a.ActiveReview,
			UnsupersededVerdict: a.UnsupersededVerdict, Protected: a.Protected,
			Evidence: a.Evidence,
			Worktree: WorktreeEvidence{Known: a.WorktreeKnown, Dirty: a.Dirty,
				UniqueCommits: a.UniqueCommits, UniqueRefs: a.UniqueRefs},
		}})[0]
		if !decision.CloseEligible {
			continue
		}
		out = append(out, CleanupCandidate{Name: a.Name, TabID: a.TabID, PaneID: a.PaneID,
			Status: a.Status, Reason: strings.Join(decision.Evidence, "; ")})
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
