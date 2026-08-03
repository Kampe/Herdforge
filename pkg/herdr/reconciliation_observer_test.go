package herdr

import (
	"context"
	"os"
	"testing"
)

type fixtureReader struct {
	tabs    Authority[[]TabRecord]
	agents  Authority[[]AgentEntry]
	board   Authority[BoardTruth]
	binding Authority[TabBinding]
}

func (f fixtureReader) ListTabs(context.Context, string) (Authority[[]TabRecord], error) {
	return f.tabs, nil
}
func (f fixtureReader) ListAgents(context.Context) (Authority[[]AgentEntry], error) {
	return f.agents, nil
}
func (f fixtureReader) Binding(context.Context, TabRecord) Authority[TabBinding] { return f.binding }
func (f fixtureReader) Board(context.Context, string) Authority[BoardTruth]      { return f.board }
func (f fixtureReader) Lifecycle(context.Context, string) Authority[LifecycleTruth] {
	return Authority[LifecycleTruth]{State: EvidenceAbsent}
}
func (f fixtureReader) Worktree(context.Context, TabBinding) Authority[WorktreeEvidence] {
	return present(WorktreeEvidence{Known: true})
}
func (f fixtureReader) Review(context.Context, string) Authority[ReviewTruth] {
	return present(ReviewTruth{})
}
func (f fixtureReader) Mail(context.Context, string) Authority[MailTruth] {
	return present(MailTruth{})
}
func (f fixtureReader) Process(context.Context, TabBinding, AgentTruth) Authority[ProcessTruth] {
	return present(ProcessTruth{})
}
func (f fixtureReader) Protection(context.Context, TabBinding) Authority[ProtectionTruth] {
	return present(ProtectionTruth{})
}

func TestProductionObserverUsesExactTabRegistryBinding(t *testing.T) {
	r := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wF:t1", WorkspaceID: "wF", Label: "misleading label"}}),
		agents:  present([]AgentEntry{}),
		board:   present(BoardTruth{TaskRef: "FAC-72", Status: "to-do"}),
		binding: present(TabBinding{TabID: "wF:t1", Generation: "g1", TaskRef: "FAC-72", PaneID: "wF:p1"}),
	}
	o := &ProductionReconciliationObserver{Workspace: "wF", Reader: r}
	if err := o.ObserveReconciliation(context.Background()); err != nil {
		t.Fatal(err)
	}
	decisions := o.Decisions()
	if len(decisions) != 1 || decisions[0].Class != TabSafeOrphan {
		t.Fatalf("decisions=%+v", decisions)
	}
}

func TestProductionObserverUnavailableAuthorityBlocks(t *testing.T) {
	r := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wF:t1", WorkspaceID: "wF"}}),
		agents:  present([]AgentEntry{}),
		board:   Authority[BoardTruth]{State: EvidenceError, Detail: "fixture transport failure"},
		binding: present(TabBinding{TabID: "wF:t1", Generation: "g1", TaskRef: "FAC-72"}),
	}
	o := &ProductionReconciliationObserver{Workspace: "wF", Reader: r}
	path := t.TempDir() + "/reconciliation.jsonl"
	o.Record = (&JSONLRecorder{Path: path}).Record
	if err := o.ObserveReconciliation(context.Background()); err == nil {
		t.Fatal("blocked authority must return non-nil")
	}
	if got := o.Decisions()[0].Class; got != TabBlocked {
		t.Fatalf("unavailable authority=%s", got)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Fatalf("missing durable blocked evidence: err=%v bytes=%d", err, len(data))
	}
}

func TestProductionObserverUnavailableAgentInventoryBlocksGlobally(t *testing.T) {
	r := fixtureReader{
		tabs:   present([]TabRecord{}),
		agents: Authority[[]AgentEntry]{State: EvidenceUnknown, Detail: "agent inventory was not read"},
	}
	o := &ProductionReconciliationObserver{Workspace: "wF", Reader: r}
	path := t.TempDir() + "/reconciliation.jsonl"
	o.Record = (&JSONLRecorder{Path: path}).Record
	if err := o.ObserveReconciliation(context.Background()); err == nil {
		t.Fatal("unavailable agent inventory must block globally")
	}
	decisions := o.Decisions()
	if len(decisions) != 1 || decisions[0].Class != TabBlocked || decisions[0].Evidence[0] != "BLOCKED: agents: agent inventory was not read" {
		t.Fatalf("global blocked decision=%+v", decisions)
	}
	if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
		t.Fatalf("missing durable global blocked evidence: err=%v bytes=%d", err, len(data))
	}
}

func TestProductionObserverDuplicateAgentAttachmentBlocks(t *testing.T) {
	r := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wF:t1", WorkspaceID: "wF"}}),
		agents:  present([]AgentEntry{{TabID: "wF:t1", PaneID: "p1"}, {TabID: "wF:t1", PaneID: "p2"}}),
		board:   present(BoardTruth{TaskRef: "FAC-72", Status: "to-do"}),
		binding: present(TabBinding{TabID: "wF:t1", Generation: "g1", TaskRef: "FAC-72"}),
	}
	o := &ProductionReconciliationObserver{Workspace: "wF", Reader: r}
	if err := o.ObserveReconciliation(context.Background()); err == nil {
		t.Fatal("duplicate attachments must block")
	}
	if got := o.Decisions()[0].Class; got != TabBlocked {
		t.Fatalf("duplicate decision=%s", got)
	}
}

func TestTabListSocketFixtureDoesNotInventAgentFields(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[{"tab_id":"t1","workspace_id":"wF","label":"Herdforge · FAC-72","number":1,"pane_count":1,"focused":false,"agent_status":"unknown"}]}}`, nil
		}
		return `{"result":{"agents":[{"name":"shell","agent_status":"unknown","tab_id":"t1"}]}}`, nil
	}
	tabs, err := TabList("wF")
	if err != nil || len(tabs) != 1 || tabs[0].Label != "Herdforge · FAC-72" || tabs[0].PaneCount != 1 {
		t.Fatalf("tabs=%+v err=%v", tabs, err)
	}
	agents, err := AgentList()
	if err != nil || len(agents) != 1 || agents[0].Name != "shell" {
		t.Fatalf("agents=%+v err=%v", agents, err)
	}
}
