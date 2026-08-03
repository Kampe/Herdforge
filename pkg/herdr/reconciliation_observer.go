package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

type ReconciliationResult struct {
	Decisions []TabDecision
}

// ProductionReconciliationObserver performs the complete read-only sweep.
// It is concrete and wired into the daemon, while FAC-180 remains the only
// owner allowed to perform a close mutation.
type ProductionReconciliationObserver struct {
	Workspace string
	Reader    ReconciliationReader
	Last      ReconciliationResult
	Record    func(context.Context, []TabDecision) error
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
	byTab := map[string]AgentEntry{}
	duplicateTabs := map[string]bool{}
	if agents.State == EvidencePresent {
		for _, agent := range agents.Value {
			if agent.TabID != "" {
				if _, exists := byTab[agent.TabID]; exists {
					duplicateTabs[agent.TabID] = true
				}
				byTab[agent.TabID] = agent
			}
		}
	}
	decisions := make([]TabDecision, 0)
	for _, tab := range tabs.Value {
		bindingAuth := o.Reader.Binding(ctx, tab)
		binding := bindingAuth.Value
		if binding.TabID == "" {
			binding.TabID = tab.TabID
		}
		binding.Label = tab.Label
		if duplicateTabs[tab.TabID] {
			bindingAuth = Authority[TabBinding]{State: EvidenceError, Detail: "duplicate agent attachments for tab"}
		}
		agent, found := byTab[tab.TabID]
		ag := Authority[AgentTruth]{State: EvidenceAbsent}
		if found {
			ag = Authority[AgentTruth]{State: EvidencePresent, Value: AgentTruth{Present: true, Status: agent.Status, PaneID: agent.PaneID}}
		}
		if agents.State != EvidencePresent {
			ag = Authority[AgentTruth]{State: agents.State, Detail: agents.Detail}
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
	o.Last = ReconciliationResult{Decisions: decisions}
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
	return nil
}

func (o *ProductionReconciliationObserver) Decisions() []TabDecision {
	if o == nil {
		return nil
	}
	return append([]TabDecision(nil), o.Last.Decisions...)
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
	return nil
}
