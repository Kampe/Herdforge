package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ReconciliationReader is the authority boundary. Each method returns a
// value-bearing typed read; adapters must return EvidenceError on transport
// or parse failure, never a fabricated zero-value Present result.
type ReconciliationReader interface {
	ListTabs(context.Context, string) (Authority[[]TabRecord], error)
	ListAgents(context.Context) (Authority[[]AgentEntry], error)
	Binding(context.Context, TabRecord) Authority[TabBinding]
	Board(context.Context, string) Authority[BoardTruth]
	Lifecycle(context.Context, string) Authority[LifecycleTruth]
	Worktree(context.Context, TabBinding) Authority[WorktreeEvidence]
	Review(context.Context, string) Authority[ReviewTruth]
	Mail(context.Context, string) Authority[MailTruth]
	Process(context.Context, TabBinding, AgentTruth) Authority[ProcessTruth]
	Protection(context.Context, TabBinding) Authority[ProtectionTruth]
}

// CompletedTaskProof is the durable completion evidence for a managed lane.
// It is intentionally separate from Herdr tab generations: a completion
// proof may clear an idle lane from reconciliation, but it never authorizes
// closing a generationless tab.
type CompletedTaskProof struct {
	TaskRef       string
	CandidateSHA  string
	Complete      bool
	Authenticated bool
}

type CompletedTaskProofRequest struct {
	TaskRef         string
	CandidateSHA    string
	LeaseGeneration int64
}

// CompletedTaskProofReader is the narrow control/mail evidence seam. The
// production adapter is responsible for authenticating the task context and
// checking the durable completion callback; fixtures can provide deterministic
// in-memory evidence.
type CompletedTaskProofReader interface {
	CompletedTaskProof(context.Context, CompletedTaskProofRequest) Authority[CompletedTaskProof]
}

type ReconciliationResult struct {
	Decisions  []TabDecision
	LiveAgents []AgentEntry
}

// ProductionReconciliationObserver performs the complete read-only sweep.
// It is concrete and wired into the daemon, while FAC-180 remains the only
// owner allowed to perform a close mutation.
type ProductionReconciliationObserver struct {
	Workspace      string
	Reader         ReconciliationReader
	ControlBinding TabBinding
	Last           ReconciliationResult
	Record         func(context.Context, []TabDecision) error
	TaskBinding    func(context.Context, TabRecord, AgentEntry) Authority[TabBinding]
	Completion     CompletedTaskProofReader
	LegacyStore    LegacyTabStateStore
}

// SocketAuthorityReader is the real Herdr transport adapter. It only claims
// authority for tab/agent inventory; board, git, lifecycle, review, mail, and
// process truth remain explicitly unavailable until their adapters are wired.
type SocketAuthorityReader struct{}

func (SocketAuthorityReader) ListTabs(_ context.Context, workspace string) (Authority[[]TabRecord], error) {
	v, err := TabList(workspace)
	if err != nil {
		return Authority[[]TabRecord]{State: EvidenceError, Detail: err.Error()}, err
	}
	return Authority[[]TabRecord]{State: EvidencePresent, Value: v}, nil
}
func (SocketAuthorityReader) ListAgents(_ context.Context) (Authority[[]AgentEntry], error) {
	v, err := AgentList()
	if err != nil {
		return Authority[[]AgentEntry]{State: EvidenceError, Detail: err.Error()}, err
	}
	return Authority[[]AgentEntry]{State: EvidencePresent, Value: v}, nil
}
func (SocketAuthorityReader) Binding(context.Context, TabRecord) Authority[TabBinding] {
	return unavailable[TabBinding]("durable launch-receipt binding")
}
func unavailable[T any](name string) Authority[T] {
	return Authority[T]{State: EvidenceError, Detail: name + " authority adapter unavailable"}
}
func (SocketAuthorityReader) Board(context.Context, string) Authority[BoardTruth] {
	return unavailable[BoardTruth]("board")
}
func (SocketAuthorityReader) Lifecycle(context.Context, string) Authority[LifecycleTruth] {
	return unavailable[LifecycleTruth]("lifecycle")
}
func (SocketAuthorityReader) Worktree(context.Context, TabBinding) Authority[WorktreeEvidence] {
	return unavailable[WorktreeEvidence]("worktree")
}
func (SocketAuthorityReader) Review(context.Context, string) Authority[ReviewTruth] {
	return unavailable[ReviewTruth]("review")
}
func (SocketAuthorityReader) Mail(context.Context, string) Authority[MailTruth] {
	return unavailable[MailTruth]("mail")
}
func (SocketAuthorityReader) Process(context.Context, TabBinding, AgentTruth) Authority[ProcessTruth] {
	return unavailable[ProcessTruth]("process")
}
func (SocketAuthorityReader) Protection(context.Context, TabBinding) Authority[ProtectionTruth] {
	return unavailable[ProtectionTruth]("protection")
}

func (o *ProductionReconciliationObserver) ObserveReconciliation(ctx context.Context) error {
	recordBlocked := func(reason string) error {
		d := TabDecision{TabID: "__reconciliation__", Class: TabBlocked, Evidence: []string{"BLOCKED: " + reason}}
		if o != nil {
			o.Last = ReconciliationResult{Decisions: []TabDecision{d}, LiveAgents: o.Last.LiveAgents}
		}
		if o != nil && o.Record != nil {
			if err := o.Record(ctx, []TabDecision{d}); err != nil {
				return fmt.Errorf("reconciliation record: %w", err)
			}
		}
		return fmt.Errorf("reconciliation BLOCKED: %s", reason)
	}
	if o == nil || o.Reader == nil || o.Workspace == "" {
		return recordBlocked("authority reader and workspace are required")
	}
	tabs, err := o.Reader.ListTabs(ctx, o.Workspace)
	if err != nil {
		return recordBlocked("tabs: " + err.Error())
	}
	if tabs.State != EvidencePresent {
		return recordBlocked("tabs: " + tabs.Detail)
	}
	agents, err := o.Reader.ListAgents(ctx)
	if err != nil {
		return recordBlocked("agents: " + err.Error())
	}
	if agents.State != EvidencePresent {
		return recordBlocked("agents: " + agents.Detail)
	}
	o.Last = ReconciliationResult{LiveAgents: agents.Value}
	byTab := map[string]AgentEntry{}
	duplicateTabs := map[string]bool{}
	for _, agent := range agents.Value {
		if agent.TabID != "" {
			if _, exists := byTab[agent.TabID]; exists {
				duplicateTabs[agent.TabID] = true
			}
			byTab[agent.TabID] = agent
		}
	}
	decisions := make([]TabDecision, 0)
	for _, tab := range tabs.Value {
		bindingAuth := o.Reader.Binding(ctx, tab)
		agent, found := byTab[tab.TabID]
		if tab.Generation == "" && found && agent.TabGeneration > 0 {
			tab.Generation = strconv.FormatUint(agent.TabGeneration, 10)
		}
		if decision, handled, err := o.reconcileLegacyTab(ctx, tab, agent, found, bindingAuth); err != nil {
			return recordBlocked("legacy tab state: " + err.Error())
		} else if handled {
			decisions = append(decisions, decision)
			continue
		}
		// A tab without a live agent and without an authoritative binding is
		// stale registry state (or an operator-owned tab). It cannot identify a
		// managed lane and must not keep reconciliation blocked forever. Managed
		// no-agent tabs still proceed when their durable binding is present.
		if !found && bindingAuth.State != EvidencePresent {
			continue
		}
		// Herdr 0.8 can report a terminal tab created before immutable tab
		// generations existed. It is not safe for CAS close, but it is also not
		// a live reconciliation blocker: retain one durable cleanup candidate and
		// let the explicit cleanup path delegate the exact tab id to Herdr.
		// Active generationless workers remain fail-closed below.
		if tab.Generation == "" && found && isTerminalAgent(agent.Status) && bindingAuth.State == EvidencePresent && bindingAuth.Value.TaskRef != "" {
			decisions = append(decisions, TabDecision{TabID: tab.TabID, Class: TabLegacyCleanup,
				Evidence: []string{"legacy terminal tab has no immutable generation; exact Herdr tab close is a cleanup candidate"}})
			continue
		}
		if bindingAuth.State != EvidencePresent && found && isTerminalAgent(agent.Status) && o.TaskBinding != nil {
			fallback := o.TaskBinding(ctx, tab, agent)
			if fallback.State == EvidencePresent && fallback.Value.TabID == tab.TabID && fallback.Value.TaskRef != "" && fallback.Value.LeaseGeneration > 0 && o.Completion != nil {
				proof := o.Completion.CompletedTaskProof(ctx, CompletedTaskProofRequest{TaskRef: fallback.Value.TaskRef, CandidateSHA: fallback.Value.CandidateSHA, LeaseGeneration: fallback.Value.LeaseGeneration})
				candidateMatches := fallback.Value.CandidateSHA == "" || proof.Value.CandidateSHA == fallback.Value.CandidateSHA
				if proof.State == EvidencePresent && proof.Value.Complete && proof.Value.Authenticated && proof.Value.TaskRef == fallback.Value.TaskRef && proof.Value.CandidateSHA != "" && candidateMatches {
					decisions = append(decisions, TabDecision{TabID: tab.TabID, Class: TabSafeFinished,
						Evidence: []string{"durable task context and authenticated completion proof; idle generationless tab retained"}})
					continue
				}
			}
		}
		binding := bindingAuth.Value
		if binding.TabID == "" {
			binding.TabID = tab.TabID
		}
		if binding.Generation == "" {
			binding.Generation = tab.Generation
		}
		binding.Label = tab.Label
		if duplicateTabs[tab.TabID] {
			bindingAuth = Authority[TabBinding]{State: EvidenceError, Detail: "duplicate agent attachments for tab"}
		}
		ag := Authority[AgentTruth]{State: EvidenceAbsent}
		if found {
			ag = Authority[AgentTruth]{State: EvidencePresent, Value: AgentTruth{Status: agent.Status, PaneID: agent.PaneID}}
		}
		if o.isExactControlTab(tab, agent, found) {
			decisions = append(decisions, TabDecision{
				TabID:    tab.TabID,
				Class:    TabStanding,
				Evidence: []string{"durable coordinator control binding; no task lease generation required"},
			})
			continue
		}
		taskRef := binding.TaskRef
		a := AuthoritySnapshot{Agent: ag, Board: o.Reader.Board(ctx, taskRef), Lifecycle: o.Reader.Lifecycle(ctx, taskRef),
			Worktree: o.Reader.Worktree(ctx, binding), Review: o.Reader.Review(ctx, taskRef), Mail: o.Reader.Mail(ctx, taskRef),
			Process: o.Reader.Process(ctx, binding, ag.Value), Protection: o.Reader.Protection(ctx, binding)}
		if bindingAuth.State != EvidencePresent {
			a.Board = Authority[BoardTruth]{State: EvidenceError, Detail: bindingAuth.Detail}
		}
		decisions = append(decisions, ReconcileTabs([]TabObservation{AssembleBoundObservation(binding, a)})[0])
	}
	o.Last = ReconciliationResult{Decisions: decisions, LiveAgents: agents.Value}
	if o.Record != nil {
		if err := o.Record(ctx, decisions); err != nil {
			return fmt.Errorf("reconciliation record: %w", err)
		}
	}
	for _, d := range decisions {
		if d.Class == TabBlocked {
			return fmt.Errorf("reconciliation BLOCKED: tab %s: %s", d.TabID, d.Evidence)
		}
	}
	if _, err := o.ReapCompletedTaskLanes(ctx); err != nil {
		return fmt.Errorf("auto-reap completed task lanes: %w", err)
	}
	return nil
}

func (o *ProductionReconciliationObserver) reconcileLegacyTab(ctx context.Context, tab TabRecord, agent AgentEntry, found bool, bindingAuth Authority[TabBinding]) (TabDecision, bool, error) {
	if o.LegacyStore == nil || tab.TabID == "" {
		return TabDecision{}, false, nil
	}
	state, exists, err := o.LegacyStore.Lookup(ctx, o.Workspace, tab.TabID)
	if err != nil {
		return TabDecision{}, false, err
	}
	if !legacyCandidate(tab, agent, found, bindingAuth) && !exists {
		return TabDecision{}, false, nil
	}
	if exists {
		if state.Action == legacyActionBackfill && exactLegacyIdentity(tab, agent, found, state.Binding) {
			return TabDecision{TabID: tab.TabID, Generation: state.Binding.Generation, Class: TabSafeOrphan,
				Evidence: []string{"authenticated legacy tab generation recovered from durable backfill"}}, true, nil
		}
		return legacyDecision(tab, state), true, nil
	}
	binding := bindingAuth.Value
	if binding.TabID == "" {
		binding.TabID = tab.TabID
	}
	binding.Workspace = legacyFirstNonEmpty(binding.Workspace, tab.WorkspaceID, o.Workspace)
	binding.Label = tab.Label
	if binding.Generation == "" {
		binding.Generation = tab.Generation
	}
	if bindingAuth.State == EvidencePresent && exactLegacyIdentity(tab, agent, found, binding) && binding.Generation != "" {
		if err := o.LegacyStore.Record(ctx, LegacyTabState{Workspace: o.Workspace, TabID: tab.TabID, PaneID: binding.PaneID, Action: legacyActionBackfill, Binding: binding, Reason: "authenticated exact Herdr identity backfilled"}); err != nil {
			return TabDecision{}, false, err
		}
		return TabDecision{TabID: tab.TabID, Generation: binding.Generation, Class: TabSafeOrphan,
			Evidence: []string{"authenticated exact Herdr identity backfilled"}}, true, nil
	}
	state = LegacyTabState{Workspace: o.Workspace, TabID: tab.TabID, PaneID: binding.PaneID, Action: legacyActionTombstone, Binding: binding, Reason: "immutable tab generation unavailable from authenticated exact Herdr identity"}
	if err := o.LegacyStore.Record(ctx, state); err != nil {
		return TabDecision{}, false, err
	}
	return legacyDecision(tab, state), true, nil
}

func legacyCandidate(tab TabRecord, agent AgentEntry, found bool, binding Authority[TabBinding]) bool {
	if tab.Generation != "" && binding.State == EvidencePresent && binding.Value.Generation != "" {
		return false
	}
	// Labels are mutable and coordinator labels share the Herdforge prefix;
	// never treat a label alone as a legacy worker candidate. Require either a
	// bound task or an attached named agent.
	return (binding.State == EvidencePresent && binding.Value.TaskRef != "") || (found && agent.Name != "" && !isCoordinatorAgent(agent.Name))
}

func isCoordinatorAgent(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "coordinator" || strings.HasPrefix(name, "coordinator-") || strings.HasSuffix(name, "-coordinator")
}

func exactLegacyIdentity(tab TabRecord, agent AgentEntry, found bool, binding TabBinding) bool {
	if binding.TabID != tab.TabID || (binding.Workspace != "" && binding.Workspace != tab.WorkspaceID) {
		return false
	}
	return !found || binding.PaneID == "" || agent.PaneID == "" || binding.PaneID == agent.PaneID
}

func legacyDecision(tab TabRecord, state LegacyTabState) TabDecision {
	reason := state.Reason
	if reason == "" {
		reason = "legacy tab migration state recorded"
	}
	return TabDecision{TabID: tab.TabID, Generation: state.Binding.Generation, Class: TabLegacyCleanup, Evidence: []string{"legacy tab state: " + state.Action + ": " + reason}}
}

func legacyFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isTerminalAgent(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "idle", "done", "complete", "completed":
		return true
	default:
		return false
	}
}

// isExactControlTab recognizes only the durable coordinator registration's
// complete Herdr incarnation. It deliberately does not infer control status
// from labels, cwd, agent kind, or a missing task binding: those are mutable
// or ambiguous and worker fencing must remain fail-closed.
func (o *ProductionReconciliationObserver) isExactControlTab(tab TabRecord, agent AgentEntry, found bool) bool {
	b := o.ControlBinding
	return found && b.ControlSeat && b.Workspace == o.Workspace && b.TabID == tab.TabID &&
		b.TabID == agent.TabID && b.PaneID != "" && b.PaneID == agent.PaneID &&
		b.Role == "coordinator" && b.Generation == "" && b.TerminalID != "" &&
		b.TerminalID == agent.TerminalID
}

// ReapResult records the outcome of an automated task lane reap attempt.
type ReapResult struct {
	Reaped []string `json:"reaped"`
	Errs   []error  `json:"errors,omitempty"`
}

// ReapCompletedTaskLanes iterates over eligible reconciliation decisions and executes
// an atomic FAC-180 compare-and-close (TabCloseCAS) for safe finished/orphan task lanes.
// Live, active, user shell, standing, blocked, or preserved tabs are strictly retained.
func (o *ProductionReconciliationObserver) ReapCompletedTaskLanes(ctx context.Context) (ReapResult, error) {
	if o == nil {
		return ReapResult{}, fmt.Errorf("reconciliation observer is nil")
	}
	res := ReapResult{}
	for _, d := range o.Last.Decisions {
		if !d.CloseEligible || (d.Class != TabSafeFinished && d.Class != TabSafeOrphan) {
			continue
		}
		req := CloseRequest{
			WorkspaceID: o.Workspace,
			TabID:       d.TabID,
			Generation:  d.Generation,
			Nonce:       fmt.Sprintf("reap-%s-%s", d.TabID, d.Generation),
		}
		if err := TabCloseCAS(req); err != nil {
			res.Errs = append(res.Errs, fmt.Errorf("tab %s (gen %s): %w", d.TabID, d.Generation, err))
		} else {
			res.Reaped = append(res.Reaped, d.TabID)
		}
	}
	if len(res.Errs) > 0 {
		return res, fmt.Errorf("reap completed task lanes: %d failed", len(res.Errs))
	}
	return res, nil
}

func (o *ProductionReconciliationObserver) Decisions() []TabDecision {
	if o == nil {
		return nil
	}
	return append([]TabDecision(nil), o.Last.Decisions...)
}

// LiveAgents returns the inventory captured during the latest observation.
// It lets status render the same live census used by reconciliation without a
// second fleet query.
func (o *ProductionReconciliationObserver) LiveAgents() []AgentEntry {
	if o == nil {
		return nil
	}
	return append([]AgentEntry(nil), o.Last.LiveAgents...)
}

// JSONLRecorder provides durable observe-mode evidence without any close
// mutation. The path is caller-supplied and should be repository-relative.
type JSONLRecorder struct {
	Path string
	mu   sync.Mutex
}

func (r *JSONLRecorder) Record(_ context.Context, decisions []TabDecision) error {
	if r == nil || r.Path == "" {
		return fmt.Errorf("reconciliation recorder path is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, d := range decisions {
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return f.Sync()
}
