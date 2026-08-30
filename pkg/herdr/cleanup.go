package herdr

import (
	"errors"
	"fmt"
	"github.com/Kampe/Herdforge/pkg/kick"
	"sort"
	"strings"
	"sync"

	"github.com/Kampe/Herdforge/pkg/toolchild"
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
	TabPaused          TabClass = "paused"
	TabStanding        TabClass = "standing"
	TabPreservedReview TabClass = "preserved-review"
	TabUserShell       TabClass = "user-shell"
	TabSafeFinished    TabClass = "safe-finished"
	TabSafeOrphan      TabClass = "safe-orphan"
	TabLegacyCleanup   TabClass = "legacy-cleanup"
	TabRecovering      TabClass = "recovering"
	TabQueued          TabClass = "queued-but-not-consumed"
	TabBlocked         TabClass = "unknown/BLOCKED"
	// TabUnfenceable: this herdr build supplies no immutable tab generation, so
	// the tab CANNOT be fenced — as opposed to a tab whose state we failed to
	// read, or whose generation is stale.
	//
	// FAC-571: a missing generation used to be TabBlocked, which made
	// reconciliation report a blanket BLOCKED for every tab on such a build and
	// jammed the subsystem. A per-item capability gap must not halt the whole
	// projection.
	//
	// THE RECORDED DECISION: unfenceable is a KNOWN state, not an unknown one.
	// We know exactly what it is and why, so it does not consume capacity the
	// way genuine uncertainty does. It never authorizes a mutation:
	// CloseEligible stays false, because nothing may be closed without fencing
	// evidence. Read-only classification and reporting are permitted; mutation
	// is not. A STALE generation remains TabBlocked, because that is a real
	// conflict rather than an absent capability.
	TabUnfenceable TabClass = "unfenceable"
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

// Authority is a value-bearing read result. The state and value are carried
// together so callers cannot claim Present while supplying an unrelated
// default value. Unknown and Error are never eligible for cleanup.
type Authority[T any] struct {
	State  EvidenceState
	Value  T
	Detail string
}

type TabBinding struct {
	TabID           string
	Workspace       string
	Generation      string
	TaskRef         string
	CandidateSHA    string
	LeaseGeneration int64
	PaneID          string
	TerminalID      string
	Label           string
	Role            string
	ControlSeat     bool
}

type BoardTruth struct{ TaskRef, Status string }
type AgentTruth struct {
	Status, SessionID, SessionGeneration, PaneID string
}
type LifecycleTruth struct{ State string }
type ReviewTruth struct{ Pending, Active, UnsupersededVerdict bool }
type MailTruth struct{ PendingOutbox, PendingCallback bool }
type ProcessTruth struct {
	Alive                        bool
	PID                          int
	SessionID, SessionGeneration string
}
type ProtectionTruth struct {
	Standing, UserShell, Protected bool
	Role                           string
}

type AuthoritySnapshot struct {
	Board      Authority[BoardTruth]
	Agent      Authority[AgentTruth]
	Lifecycle  Authority[LifecycleTruth]
	Worktree   Authority[WorktreeEvidence]
	Review     Authority[ReviewTruth]
	Mail       Authority[MailTruth]
	Process    Authority[ProcessTruth]
	Protection Authority[ProtectionTruth]
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
	TabID       string
	Generation  string
	Label       string
	Binding     TabBinding
	Authorities AuthoritySnapshot
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
	case "in-progress", "in progress", "working", "starting":
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

// NormalizeAssignmentStatus keeps prompt delivery state separate from the
// agent's own work state. A queued assignment is not evidence that a lane is
// consuming it, even when agent_status is already working on a standing goal.
func NormalizeAssignmentStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "queued", "pending", "staged", "delivered":
		return "queued"
	case "consumed", "working", "done":
		return "consumed"
	default:
		return "unknown"
	}
}

type FleetStatus struct {
	Working      int
	Paused       int
	Queued       int
	Capacity     int
	Unknown      int
	Standing     int
	Preserved    int
	Recovering   int
	ControlSeats int
	// Unfenceable counts tabs this herdr build cannot fence. Reported
	// separately from Unknown so an operator can see the difference between
	// "we could not read this" and "this build has no compare-and-close"
	// (FAC-571).
	Unfenceable int
	// Reclaimable counts SETTLED lanes -- live agents reporting done/complete
	// -- that are holding a slot while producing nothing.
	//
	// FAC-714: after FAC-660's residual fix (#618) working standing agents
	// correctly consume capacity, and the fleet went to capacity=0 with
	// working=11. That number is now TRUE and it names no remedy, which is the
	// same defect this repository keeps finding: a count is reportable, only an
	// identity or an action is usable. Two of those lanes were `done` and
	// reclaimable at the moment capacity read zero.
	//
	// Reported rather than subtracted from Capacity. A settled lane genuinely
	// occupies its slot until something reaps it, so counting it as free would
	// invite dispatch into a seat that is still taken -- trading an honest zero
	// for an optimistic lie.
	Reclaimable int
	Classes     map[TabClass]int
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
		// Unfenceable is deliberately NOT counted as unknown: it is a known,
		// explained state, and counting it as uncertainty zeroed capacity for
		// every lane on a build that never supplies generations.
		if d.Class == TabUnfenceable {
			p.Unfenceable++
		}
		if d.Class == TabLegacyCleanup {
			// A tombstone suppresses repeated noise, but it does not prove
			// capacity: the legacy tab remains unverifiable and uncloseable.
			p.Unknown++
		}
		if d.Class == TabStanding {
			p.Standing++
		}
		if d.Class == TabPreservedReview {
			p.Preserved++
		}
		if d.Class == TabRecovering {
			p.Recovering++
		}
		if d.Class == TabUserShell {
			p.ControlSeats++
		}
	}
	p.Capacity = maxLanes - p.Working - p.Recovering
	if p.Unknown > 0 {
		p.Capacity = 0
	}
	if p.Capacity < 0 {
		p.Capacity = 0
	}
	return p
}

// ProjectLiveFleetStatus is the read-only census used by operator status.
// Live Herdr inventory is sufficient to classify a lane for display; requiring
// the cleanup reconciler's durable task, process, and worktree authorities here
// turns healthy live lanes into unknown merely because those adapters are not
// available in the socket reader.
func ProjectLiveFleetStatus(agents []AgentEntry, standing map[string]bool, workspace string, maxLanes int) FleetStatus {
	p := FleetStatus{Classes: map[TabClass]int{}}
	for _, agent := range agents {
		if workspace != "" && agent.Workspace != workspace {
			continue
		}
		var class TabClass
		switch {
		case agent.Name == "":
			class = TabUserShell
		case NormalizeAssignmentStatus(agent.AssignmentStatus) == "queued":
			class = TabQueued
		case strings.EqualFold(strings.TrimSpace(agent.Status), "paused"):
			class = TabPaused
		// FAC-660 residual / seq 3130: a standing-named agent that is WORKING
		// is productive capacity, not idle standing inventory. Classifying
		// standing names before live status made herd status report
		// working=1 standing=13 while herdr/pulse showed ~14 busy forge-*
		// agents. Match on activity first; standing is the idle remainder.
		case NormalizeTaskStatus(agent.Status) == "in-progress":
			class = TabActive
		case NormalizeTaskStatus(agent.Status) == "recovering":
			class = TabRecovering
		case isStandingAgent(agent.Name, standing):
			class = TabStanding
		default:
			class = TabBlocked
		}
		p.Classes[class]++
		switch class {
		case TabActive:
			p.Working++
		case TabPaused:
			p.Paused++
		case TabQueued:
			p.Queued++
		case TabStanding:
			p.Standing++
		case TabRecovering:
			p.Recovering++
		case TabUserShell:
			p.ControlSeats++
		case TabBlocked:
			p.Unknown++
		}
	}
	p.Capacity = maxLanes - p.Working - p.Paused - p.Queued - p.Recovering
	if p.Unknown > 0 || p.Capacity < 0 {
		p.Capacity = 0
	}
	return p
}

// CountReclaimable reports how many live agents are SETTLED -- done or complete
// -- and therefore holding a slot while producing nothing.
//
// FAC-714: this exists so `capacity=0` can carry its own remedy. A saturated
// fleet with reclaimable lanes and a saturated fleet with none are completely
// different situations that read identically as a zero.
func CountReclaimable(agents []AgentEntry) int {
	n := 0
	for _, a := range agents {
		switch NormalizeTaskStatus(a.Status) {
		case "done", "complete", "completed":
			n++
		}
	}
	return n
}

func authorityReady[T any](a Authority[T]) bool {
	return a.State == EvidenceAbsent || a.State == EvidencePresent
}

func reconcileBoundTab(tab TabObservation) TabDecision {
	d := TabDecision{TabID: tab.Binding.TabID, Generation: tab.Binding.Generation}
	blocked := func(reason string) TabDecision {
		d.Class = TabBlocked
		d.Evidence = append(d.Evidence, "BLOCKED: "+reason)
		return d
	}
	if tab.Binding.TabID == "" || tab.Binding.TabID != tab.TabID {
		return blocked("exact tab binding missing")
	}
	if tab.Binding.Generation == "" {
		// FAC-571: a capability gap, not a failure to read state.
		//
		// But NARROWLY. A tab bound to a TASK must still be BLOCKED: an active
		// worker whose identity cannot be fenced is genuinely unsafe to reason
		// about, and that refusal is a deliberate pre-existing invariant with
		// tests defending it. Downgrading it would trade a jammed subsystem for
		// an unsound one.
		//
		// The case worth reclassifying is the one where blocking is pure noise:
		// no task binding, so there is no in-flight work whose safety depends on
		// fencing. Reconciliation then keeps reporting actionable state instead
		// of halting on every tab, and still authorizes nothing —
		// CloseEligible stays false.
		// Reclassify ONLY the unambiguously inert case: no task binding and no
		// live agent attached. Anything with work or a process behind it stays
		// blocked, because that is the case where being unable to fence the
		// identity actually matters.
		inert := strings.TrimSpace(tab.Binding.TaskRef) == "" &&
			tab.Authorities.Agent.State == EvidenceAbsent &&
			tab.Authorities.Process.State == EvidenceAbsent
		if !inert {
			return blocked("missing immutable tab generation")
		}
		d.Class = TabUnfenceable
		d.CloseEligible = false
		d.Evidence = append(d.Evidence,
			"UNFENCEABLE: this herdr build supplies no immutable tab generation and the tab is not bound to a task; it cannot be compare-and-closed, so it is classified read-only")
		return d
	}
	a := tab.Authorities
	checks := []struct {
		name  string
		state EvidenceState
	}{
		{"board", a.Board.State}, {"agent", a.Agent.State}, {"lifecycle", a.Lifecycle.State},
		{"worktree", a.Worktree.State}, {"review", a.Review.State}, {"mail", a.Mail.State},
		{"process", a.Process.State}, {"protection", a.Protection.State},
	}
	for _, c := range checks {
		if c.state == EvidenceError {
			return blocked(c.name + " source error")
		}
		if c.state != EvidenceAbsent && c.state != EvidencePresent {
			return blocked(c.name + " source was not authoritatively read")
		}
	}
	if a.Board.State == EvidencePresent {
		if a.Board.Value.TaskRef == "" || a.Board.Value.TaskRef != tab.Binding.TaskRef {
			return blocked("board/task binding mismatch")
		}
	} else if tab.Binding.TaskRef != "" {
		return blocked("task binding has no authoritative board record")
	}
	if a.Lifecycle.State == EvidencePresent && a.Board.State == EvidencePresent {
		boardStatus := NormalizeTaskStatus(a.Board.Value.Status)
		lifecycleStatus := NormalizeTaskStatus(a.Lifecycle.Value.State)
		if !(boardStatus == "done" && isTerminalIntegrationState(a.Lifecycle.Value.State)) && lifecycleStatus != boardStatus {
			return blocked("board/lifecycle status mismatch")
		}
	}
	if a.Agent.State == EvidencePresent {
		if a.Agent.Value.PaneID != "" && tab.Binding.PaneID != "" && a.Agent.Value.PaneID != tab.Binding.PaneID {
			return blocked("agent pane binding mismatch")
		}
		if a.Agent.Value.SessionID != "" && (a.Agent.Value.SessionGeneration == "" || a.Agent.Value.SessionGeneration != tab.Binding.Generation) {
			return blocked("session generation mismatch")
		}
	}
	if a.Process.State == EvidencePresent && a.Agent.State == EvidencePresent && a.Agent.Value.SessionID != "" && !a.Process.Value.Alive {
		return blocked("agent session process is not alive")
	}
	if a.Process.State == EvidencePresent && a.Process.Value.Alive && (a.Agent.State != EvidencePresent || a.Agent.Value.SessionID == "" || a.Process.Value.SessionID != a.Agent.Value.SessionID || a.Process.Value.SessionGeneration != a.Agent.Value.SessionGeneration) {
		return blocked("foreground process has no matching agent session")
	}
	if a.Worktree.State == EvidencePresent && !a.Worktree.Value.Known {
		return blocked("worktree authority did not establish known state")
	}
	if a.Worktree.Value.Dirty || a.Worktree.Value.UniqueCommits || a.Worktree.Value.UniqueRefs {
		return blocked("dirty or unique work exists")
	}
	if a.Review.State == EvidencePresent && (a.Review.Value.Pending || a.Review.Value.Active || a.Review.Value.UnsupersededVerdict) {
		d.Class, d.Evidence = TabPreservedReview, []string{"review evidence is still live"}
		return d
	}
	if a.Mail.State == EvidencePresent && (a.Mail.Value.PendingOutbox || a.Mail.Value.PendingCallback) {
		return blocked("pending mailbox/outbox callback")
	}
	if a.Board.State == EvidencePresent && NormalizeTaskStatus(a.Board.Value.Status) == "recovering" {
		d.Class, d.Evidence = TabRecovering, []string{"authoritative recovering task; preserved"}
		return d
	}
	if a.Protection.State == EvidencePresent && (a.Protection.Value.Protected || a.Protection.Value.Standing || a.Protection.Value.UserShell || tab.Binding.ControlSeat) {
		if a.Protection.Value.UserShell {
			d.Class = TabUserShell
		} else if a.Protection.Value.Standing || tab.Binding.ControlSeat {
			d.Class = TabStanding
		} else {
			return blocked("protected incident or audit marker")
		}
		d.Evidence = []string{"protected control or user seat"}
		return d
	}
	if a.Agent.State == EvidencePresent {
		switch NormalizeTaskStatus(a.Agent.Value.Status) {
		case "done":
			if a.Board.State != EvidencePresent || NormalizeTaskStatus(a.Board.Value.Status) != "done" || a.Lifecycle.State != EvidencePresent || !isTerminalIntegrationState(a.Lifecycle.Value.State) {
				return blocked("terminal agent lacks terminal board/lifecycle proof")
			}
		case "in-progress":
			if a.Agent.Value.SessionID == "" || a.Agent.Value.SessionGeneration == "" || a.Agent.Value.SessionGeneration != tab.Binding.Generation || a.Agent.Value.PaneID == "" || tab.Binding.PaneID == "" || a.Agent.Value.PaneID != tab.Binding.PaneID {
				return blocked("active agent lacks exact session and pane identity")
			}
			if a.Process.State != EvidencePresent || !a.Process.Value.Alive || a.Process.Value.SessionID != a.Agent.Value.SessionID || a.Process.Value.SessionGeneration != tab.Binding.Generation {
				return blocked("active agent lacks authoritative live process bound to session")
			}
			d.Class = TabActive
			d.Evidence = []string{"matching active session"}
			return d
		default:
			return blocked("present agent has nonterminal or unknown status")
		}
	}
	if a.Board.State == EvidencePresent && NormalizeTaskStatus(a.Board.Value.Status) == "done" {
		if a.Lifecycle.State != EvidencePresent || !isTerminalIntegrationState(a.Lifecycle.Value.State) {
			return blocked("done board lacks terminal integration truth")
		}
		d.Class = TabSafeFinished
		d.Evidence = []string{"authoritative done board/lifecycle, no active session, clean worktree"}
		d.CloseEligible = true
		return d
	}
	if a.Board.State == EvidencePresent && NormalizeTaskStatus(a.Board.Value.Status) == "to-do" && tab.Binding.TaskRef != "" {
		d.Class = TabSafeOrphan
		d.Evidence = []string{"exact task binding, authoritative to-do board, no active session, clean worktree"}
		d.CloseEligible = true
		return d
	}
	if tab.Binding.TaskRef == "" {
		d.Class, d.Evidence = TabUserShell, []string{"unowned tab registry record"}
		return d
	}
	return blocked("task status is unknown or recoverable")
}

func isTerminalIntegrationState(state string) bool {
	switch NormalizeTaskStatus(state) {
	case "done":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "integrated", "reconciled", "cleaned":
		return true
	default:
		return false
	}
}

// AssembleBoundObservation is the only production assembly path. TaskRef is
// supplied by the tab registry binding, never inferred from labels or names.
func AssembleBoundObservation(tab TabBinding, authorities AuthoritySnapshot) TabObservation {
	return TabObservation{TabID: tab.TabID, Generation: tab.Generation, Label: tab.Label, Binding: tab, Authorities: authorities}
}

// ReconcileTabs classifies every supplied tab. It never uses label text or a
// board status as a substitute for session/worktree/review evidence.
func ReconcileTabs(tabs []TabObservation) []TabDecision {
	out := make([]TabDecision, 0, len(tabs))
	for _, tab := range tabs {
		out = append(out, reconcileBoundTab(tab))
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
	request := CloseRequest{TabID: current.TabID, Generation: current.Generation}
	if current.Authorities.Agent.State == EvidencePresent {
		request.SessionID = current.Authorities.Agent.Value.SessionID
		request.SessionGeneration = current.Authorities.Agent.Value.SessionGeneration
	}
	return request, nil
}

// SelectCleanupCandidates is the pure policy: named, non-standing,
// non-orchestrator agents whose status is done or idle (a finished one-off
// builder that will never be re-kicked). Pure so tests pin the policy.
// Observe-only: candidates are not a close authorization. Actual close still
// requires FAC-180 TabCloseCAS with a durable generation fence (Cleanup and
// TabClose fail closed without it).

// isStandingAgent reports whether a live agent belongs to the standing roster.
//
// FAC-660: this was an exact map lookup on the agent's name, and the roster and
// the fleet never spell a lane the same way. The roster holds "forge-<lane>";
// a live agent is "forge-<lane>-<repository digest>" or "standing-<lane>". So a
// running standing lane did not match its own roster entry.
//
// In the census that produced contradictory counts. HERE it is worse: an
// unrecognised standing agent falls through to SelectCleanupCandidates and
// becomes eligible to be CLOSED. A roster that cannot recognise its own lanes
// hands them to the reaper, which is how standing lanes get killed and then look
// like they "stopped on their own".
//
// Matching is by lane, with a hyphen boundary and longest-lane-wins, so a lane
// named "review" cannot claim an agent belonging to "review-harvest".
func isStandingAgent(name string, standing map[string]bool) bool {
	if standing[name] {
		return true // exact match still counts, and is the cheapest case
	}
	if len(standing) == 0 || strings.TrimSpace(name) == "" {
		return false
	}
	lanes := make([]string, 0, len(standing))
	for k := range standing {
		lanes = append(lanes, k)
	}
	return kick.LaneForAgent(name, lanes) != ""
}

func SelectCleanupCandidates(agents []AgentEntry, standing map[string]bool) []CleanupCandidate {
	var out []CleanupCandidate
	for _, a := range agents {
		if a.Name == "" {
			continue // unnamed panes are the operator's, never ours to close
		}
		if isStandingAgent(a.Name, standing) {
			continue // standing fleet is re-kicked by design, never reaped
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

// ---------------------------------------------------------------------------
// FAC-302: Fenced cleanup — generation/session/revision/pane-fenced
// compare-and-close via TabCloseCAS, with absence readback, deterministic
// receipts/counts, dry-run report-only, and fail-closed protection.
// ---------------------------------------------------------------------------

// CleanupOutcome is the typed result of one fenced cleanup close attempt.
type CleanupOutcome string

const (
	CleanupClosed  CleanupOutcome = "closed"
	CleanupBlocked CleanupOutcome = "blocked"
	CleanupError   CleanupOutcome = "error"
)

// CleanupAttempt is the deterministic receipt for one candidate's fenced
// close attempt. Every mutation-mode candidate produces exactly one attempt.
type CleanupAttempt struct {
	Name    string         `json:"name"`
	TabID   string         `json:"tab_id"`
	Outcome CleanupOutcome `json:"outcome"`
	Reason  string         `json:"reason"`
}

// CleanupResult is the deterministic fenced sweep result with explicit counts
// that callers can audit without re-deriving from the attempt list.
type CleanupResult struct {
	DryRun     bool               `json:"dry_run"`
	Workspace  string             `json:"workspace"`
	Repository string             `json:"repository,omitempty"`
	Candidates []CleanupCandidate `json:"candidates"`
	Attempts   []CleanupAttempt   `json:"attempts"`
	Closed     int                `json:"closed"`
	Blocked    int                `json:"blocked"`
	Errored    int                `json:"errored"`
}

// cleanupCloseFunc is the injectable execution seam for one fenced close
// attempt. The default builds a CloseRequest from the agent entry and calls
// TabCloseCAS, then does absence readback. Tests inject a fake.
type cleanupCloseFunc func(agent AgentEntry) CleanupAttempt

var (
	cleanupCloseMu   sync.Mutex
	cleanupCloseImpl cleanupCloseFunc = defaultCleanupClose
)

// SetCleanupCloseForTest installs a hermetic cleanup close seam. Restore with
// the returned func.
func SetCleanupCloseForTest(fn cleanupCloseFunc) func() {
	cleanupCloseMu.Lock()
	old := cleanupCloseImpl
	if fn != nil {
		cleanupCloseImpl = fn
	} else {
		cleanupCloseImpl = defaultCleanupClose
	}
	cleanupCloseMu.Unlock()
	return func() {
		cleanupCloseMu.Lock()
		cleanupCloseImpl = old
		cleanupCloseMu.Unlock()
	}
}

func currentCleanupClose() cleanupCloseFunc {
	cleanupCloseMu.Lock()
	defer cleanupCloseMu.Unlock()
	return cleanupCloseImpl
}

// defaultCleanupClose builds a CloseRequest from the agent entry and calls
// TabCloseCAS. Herdr 0.8 agent-list responses have no immutable generation;
// when that capability is unavailable, delegate the exact observed tab id to
// Herdr's own tab-close operation and verify the exact name/pane/tab identity
// disappeared. This keeps cleanup scoped to the finished one-off tab while
// allowing the Herdr server to enforce any generation it owns.
func defaultCleanupClose(agent AgentEntry) CleanupAttempt {
	// FAC-569: this used to delegate to a plain close on ANY blocked outcome,
	// including a stale generation. A stale generation is a real conflict and
	// must keep refusing, or a close race can recycle-kill a tab that gained a
	// new agent between readback and mutation. Only a capability gap may
	// degrade, and that decision now lives in exactly one place.
	outcome, err := CloseExactTab(exactIdentityFor(agent))
	if err != nil {
		var blocked *CloseUnavailableError
		if errors.As(err, &blocked) {
			return CleanupAttempt{Name: agent.Name, TabID: agent.TabID, Outcome: CleanupBlocked, Reason: blocked.Reason}
		}
		return CleanupAttempt{Name: agent.Name, TabID: agent.TabID, Outcome: CleanupError, Reason: err.Error()}
	}
	return CleanupAttempt{Name: agent.Name, TabID: agent.TabID, Outcome: CleanupClosed, Reason: outcome.Reason}
}

// CleanupFenced is the FAC-302 fenced cleanup sweep. Dry-run returns
// observe-only candidates with zero attempts. Mutation mode attempts fenced
// compare-and-close for each candidate via the execution seam, with absence
// readback and deterministic receipts/counts. Fail-closed: incomplete
// evidence (no generation) yields BLOCKED, never a silent close.
func CleanupFenced(standing map[string]bool, dryRun bool) (CleanupResult, error) {
	agents, err := AgentList()
	if err != nil {
		return CleanupResult{}, err
	}
	cands := SelectCleanupCandidates(agents, standing)
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].TabID != cands[j].TabID {
			return cands[i].TabID < cands[j].TabID
		}
		return cands[i].Name < cands[j].Name
	})
	res := CleanupResult{DryRun: dryRun, Candidates: cands}
	if dryRun || len(cands) == 0 {
		return res, nil
	}
	closeFn := currentCleanupClose()
	agentByTab := map[string]AgentEntry{}
	for _, a := range agents {
		if a.TabID != "" {
			agentByTab[a.TabID] = a
		}
	}
	for _, c := range cands {
		agent := agentByTab[c.TabID]
		agent.Name = c.Name
		att := closeFn(agent)
		res.Attempts = append(res.Attempts, att)
		switch att.Outcome {
		case CleanupClosed:
			res.Closed++
		case CleanupBlocked:
			res.Blocked++
		default:
			res.Errored++
		}
	}
	if res.Errored > 0 {
		return res, fmt.Errorf("cleanup: %d errored", res.Errored)
	}
	return res, nil
}

// CleanupFencedInWorkspace applies the fenced cleanup policy only to the
// repository's already-validated Herdr workspace. The workspace filter is
// performed before candidate selection and before any close callback, so a
// cross-repository agent can never become a mutation candidate.
func CleanupFencedInWorkspace(workspace string, standing map[string]bool, dryRun bool) (CleanupResult, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return CleanupResult{}, fmt.Errorf("cleanup: workspace is required")
	}
	agents, err := AgentList()
	if err != nil {
		return CleanupResult{}, err
	}
	filtered := make([]AgentEntry, 0, len(agents))
	for _, agent := range agents {
		if agent.Workspace == workspace {
			filtered = append(filtered, agent)
		}
	}
	res, err := cleanupFencedAgents(filtered, standing, dryRun)
	res.Workspace = workspace
	return res, err
}

func cleanupFencedAgents(agents []AgentEntry, standing map[string]bool, dryRun bool) (CleanupResult, error) {
	cands := SelectCleanupCandidates(agents, standing)
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].TabID != cands[j].TabID {
			return cands[i].TabID < cands[j].TabID
		}
		return cands[i].Name < cands[j].Name
	})
	res := CleanupResult{DryRun: dryRun, Candidates: cands}
	if dryRun || len(cands) == 0 {
		return res, nil
	}
	closeFn := currentCleanupClose()
	agentByTab := map[string]AgentEntry{}
	for _, a := range agents {
		if a.TabID != "" {
			agentByTab[a.TabID] = a
		}
	}
	for _, c := range cands {
		agent := agentByTab[c.TabID]
		agent.Name = c.Name
		att := closeFn(agent)
		res.Attempts = append(res.Attempts, att)
		switch att.Outcome {
		case CleanupClosed:
			res.Closed++
		case CleanupBlocked:
			res.Blocked++
		default:
			res.Errored++
		}
	}
	if res.Errored > 0 {
		return res, fmt.Errorf("cleanup: %d errored", res.Errored)
	}
	return res, nil
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
