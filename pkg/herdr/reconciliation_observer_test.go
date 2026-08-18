package herdr

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type fixtureReader struct {
	tabs    Authority[[]TabRecord]
	agents  Authority[[]AgentEntry]
	board   Authority[BoardTruth]
	binding Authority[TabBinding]
}

type fixtureCompletionProof struct {
	proof Authority[CompletedTaskProof]
}

func (f fixtureCompletionProof) CompletedTaskProof(context.Context, CompletedTaskProofRequest) Authority[CompletedTaskProof] {
	return f.proof
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
	srv := NewFakeCompareCloseServer()
	srv.PutTab(LiveTab{WorkspaceID: "wF", TabID: "wF:t1", Generation: 1})
	restore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		return srv.CompareAndClose(req), nil
	})
	defer restore()

	r := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wF:t1", WorkspaceID: "wF", Label: "misleading label"}}),
		agents:  present([]AgentEntry{}),
		board:   present(BoardTruth{TaskRef: "FAC-72", Status: "to-do"}),
		binding: present(TabBinding{TabID: "wF:t1", Generation: "1", TaskRef: "FAC-72", PaneID: "wF:p1"}),
	}
	o := &ProductionReconciliationObserver{Workspace: "wF", Reader: r}
	if err := o.ObserveReconciliation(context.Background()); err != nil {
		t.Fatal(err)
	}
	decisions := o.Decisions()
	if len(decisions) != 1 || decisions[0].Class != TabSafeOrphan {
		t.Fatalf("decisions=%+v", decisions)
	}
	if !srv.IsClosed("wF:t1") {
		t.Fatal("expected safe orphan tab to be reaped")
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

func TestProductionObserverRecognizesDurableCoordinatorControlBindingWithoutGeneration(t *testing.T) {
	r := fixtureReader{
		tabs:   present([]TabRecord{{TabID: "wK:t2", WorkspaceID: "wK", Label: "1", AgentStatus: "working"}}),
		agents: present([]AgentEntry{{Kind: "codex", TabID: "wK:t2", PaneID: "wK:p2", TerminalID: "term-coordinator", Workspace: "wK", Status: "working"}}),
	}
	o := &ProductionReconciliationObserver{
		Workspace: "wK", Reader: r,
		ControlBinding: TabBinding{TabID: "wK:t2", Workspace: "wK", PaneID: "wK:p2", TerminalID: "term-coordinator", Role: "coordinator", ControlSeat: true},
	}
	if err := o.ObserveReconciliation(context.Background()); err != nil {
		t.Fatalf("coordinator control tab must not require a task generation: %v", err)
	}
	decisions := o.Decisions()
	if len(decisions) != 1 || decisions[0].Class != TabStanding || decisions[0].CloseEligible {
		t.Fatalf("decisions=%+v, want preserved standing control seat", decisions)
	}
}

func TestProductionObserverStillBlocksWorkerWithoutGeneration(t *testing.T) {
	r := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wK:t60", WorkspaceID: "wK", Label: "Herdforge · task-fac-304", AgentStatus: "working"}}),
		agents:  present([]AgentEntry{{Kind: "codex", Name: "task-fac-304", TabID: "wK:t60", PaneID: "wK:p60", TerminalID: "term-worker", Workspace: "wK", Status: "working"}}),
		binding: present(TabBinding{TabID: "wK:t60", Workspace: "wK", PaneID: "wK:p60", TaskRef: "FAC-304"}),
	}
	o := &ProductionReconciliationObserver{
		Workspace: "wK", Reader: r,
		ControlBinding: TabBinding{TabID: "wK:t2", Workspace: "wK", PaneID: "wK:p2", TerminalID: "term-coordinator", Role: "coordinator", ControlSeat: true},
	}
	if err := o.ObserveReconciliation(context.Background()); err == nil {
		t.Fatal("worker tab without durable immutable generation must remain blocked")
	}
	if got := o.Decisions()[0]; got.Class != TabBlocked || got.Evidence[0] != "BLOCKED: missing immutable tab generation" {
		t.Fatalf("decision=%+v, want missing-generation block", got)
	}
}

func TestProductionObserverAllowsIdleGenerationlessTaskWithDurableCompletionProof(t *testing.T) {
	const candidate = "53f868c9"
	r := fixtureReader{
		tabs:   present([]TabRecord{{TabID: "wK:t60", WorkspaceID: "wK", Label: "Herdforge · task-fac-304", AgentStatus: "idle"}}),
		agents: present([]AgentEntry{{Kind: "codex", Name: "task-fac-304", TabID: "wK:t60", PaneID: "wK:p60", Workspace: "wK", Status: "idle"}}),
	}
	o := &ProductionReconciliationObserver{
		Workspace: "wK", Reader: r,
		TaskBinding: func(context.Context, TabRecord, AgentEntry) Authority[TabBinding] {
			return present(TabBinding{TabID: "wK:t60", Workspace: "wK", PaneID: "wK:p60", TaskRef: "FAC-304", CandidateSHA: candidate, LeaseGeneration: 10, Role: "worker"})
		},
		Completion: fixtureCompletionProof{proof: present(CompletedTaskProof{TaskRef: "FAC-304", CandidateSHA: candidate, Complete: true, Authenticated: true})},
	}
	if err := o.ObserveReconciliation(context.Background()); err != nil {
		t.Fatalf("durably completed idle lane must not block reconciliation: %v", err)
	}
	decisions := o.Decisions()
	if len(decisions) != 1 || decisions[0].Class != TabSafeFinished || decisions[0].CloseEligible || decisions[0].Generation != "" {
		t.Fatalf("decisions=%+v, want retained generationless safe-finished lane", decisions)
	}
}

func TestProductionObserverDoesNotBlockTerminalLegacyGenerationlessTab(t *testing.T) {
	r := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wK:t2W2", WorkspaceID: "wK", Label: "legacy-worker", AgentStatus: "idle"}}),
		agents:  present([]AgentEntry{{Kind: "codex", Name: "task-fac-103", TabID: "wK:t2W2", PaneID: "wK:p2W2", Workspace: "wK", Status: "idle"}}),
		binding: present(TabBinding{TabID: "wK:t2W2", Workspace: "wK", PaneID: "wK:p2W2", TaskRef: "FAC-103"}),
	}
	o := &ProductionReconciliationObserver{Workspace: "wK", Reader: r}
	if err := o.ObserveReconciliation(context.Background()); err != nil {
		t.Fatalf("terminal legacy tab must be a cleanup candidate, not a loop blocker: %v", err)
	}
	if got := o.Decisions()[0]; got.Class != TabLegacyCleanup || got.CloseEligible {
		t.Fatalf("decision=%+v, want non-closeable legacy cleanup candidate", got)
	}
}

func TestProductionObserverAllowsDoneGenerationlessTaskWithEmptyContextCandidate(t *testing.T) {
	const candidate = "a55955a2"
	r := fixtureReader{
		tabs:   present([]TabRecord{{TabID: "wK:t60", WorkspaceID: "wK", Label: "Herdforge · task-fac-304", AgentStatus: "done"}}),
		agents: present([]AgentEntry{{Kind: "codex", Name: "task-fac-304", TabID: "wK:t60", PaneID: "wK:p60", Workspace: "wK", Status: "done"}}),
	}
	o := &ProductionReconciliationObserver{
		Workspace: "wK", Reader: r,
		TaskBinding: func(context.Context, TabRecord, AgentEntry) Authority[TabBinding] {
			return present(TabBinding{TabID: "wK:t60", Workspace: "wK", PaneID: "wK:p60", TaskRef: "FAC-304", LeaseGeneration: 10, Role: "worker"})
		},
		Completion: fixtureCompletionProof{proof: present(CompletedTaskProof{TaskRef: "FAC-304", CandidateSHA: candidate, Complete: true, Authenticated: true})},
	}
	if err := o.ObserveReconciliation(context.Background()); err != nil {
		t.Fatalf("done generationless lane with HEAD-derived candidate must not block: %v", err)
	}
	if got := o.Decisions()[0]; got.Class != TabSafeFinished || got.CloseEligible || got.Generation != "" {
		t.Fatalf("decision=%+v, want retained generationless safe-finished lane", got)
	}
}

func TestProductionObserverDoesNotUseCompletionProofForActiveGenerationlessTask(t *testing.T) {
	r := fixtureReader{
		tabs:   present([]TabRecord{{TabID: "wK:t60", WorkspaceID: "wK", AgentStatus: "working"}}),
		agents: present([]AgentEntry{{Kind: "codex", Name: "task-fac-304", TabID: "wK:t60", PaneID: "wK:p60", Workspace: "wK", Status: "working"}}),
	}
	o := &ProductionReconciliationObserver{
		Workspace: "wK", Reader: r,
		TaskBinding: func(context.Context, TabRecord, AgentEntry) Authority[TabBinding] {
			return present(TabBinding{TabID: "wK:t60", Workspace: "wK", PaneID: "wK:p60", TaskRef: "FAC-304", CandidateSHA: "53f868c9", LeaseGeneration: 10, Role: "worker"})
		},
		Completion: fixtureCompletionProof{proof: present(CompletedTaskProof{TaskRef: "FAC-304", CandidateSHA: "53f868c9", Complete: true, Authenticated: true})},
	}
	if err := o.ObserveReconciliation(context.Background()); err == nil {
		t.Fatal("active generationless worker must remain blocked")
	}
	if got := o.Decisions()[0]; got.Class != TabBlocked || got.Evidence[0] != "BLOCKED: missing immutable tab generation" {
		t.Fatalf("decision=%+v, want missing-generation block", got)
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

func TestReapCompletedTaskLanes_ReapsSafeAndPreservesActive(t *testing.T) {
	srv := NewFakeCompareCloseServer()
	srv.PutTab(LiveTab{WorkspaceID: "wF", TabID: "t-done", Generation: 1})
	srv.PutTab(LiveTab{WorkspaceID: "wF", TabID: "t-active", Generation: 2})
	restore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		return srv.CompareAndClose(req), nil
	})
	defer restore()

	o := &ProductionReconciliationObserver{
		Workspace: "wF",
		Last: ReconciliationResult{
			Decisions: []TabDecision{
				{TabID: "t-done", Generation: "1", Class: TabSafeFinished, CloseEligible: true},
				{TabID: "t-active", Generation: "2", Class: TabActive, CloseEligible: false},
				{TabID: "t-blocked", Generation: "3", Class: TabBlocked, CloseEligible: false},
				{TabID: "t-standing", Generation: "4", Class: TabStanding, CloseEligible: false},
			},
		},
	}

	res, err := o.ReapCompletedTaskLanes(context.Background())
	if err != nil {
		t.Fatalf("unexpected reap error: %v (errs: %+v)", err, res.Errs)
	}
	if len(res.Reaped) != 1 || res.Reaped[0] != "t-done" {
		t.Fatalf("reaped=%v, want [t-done]", res.Reaped)
	}
	if len(res.Errs) != 0 {
		t.Fatalf("unexpected reap errors: %v", res.Errs)
	}
}

func TestObserveReconciliation_InvokesAutoReapAndFailsClosed(t *testing.T) {
	srv := NewFakeCompareCloseServer()
	srv.PutTab(LiveTab{WorkspaceID: "wF", TabID: "wF:t1", Generation: 1})
	restore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		return srv.CompareAndClose(req), nil
	})
	defer restore()

	r := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wF:t1", WorkspaceID: "wF", Label: "task tab"}}),
		agents:  present([]AgentEntry{}),
		board:   present(BoardTruth{TaskRef: "FAC-72", Status: "to-do"}),
		binding: present(TabBinding{TabID: "wF:t1", Generation: "1", TaskRef: "FAC-72", PaneID: "wF:p1"}),
	}
	o := &ProductionReconciliationObserver{Workspace: "wF", Reader: r}

	if err := o.ObserveReconciliation(context.Background()); err != nil {
		t.Fatalf("ObserveReconciliation failed to reap: %v", err)
	}

	// Verify tab was reaped from server state
	if !srv.IsClosed("wF:t1") {
		t.Fatal("ObserveReconciliation must reap safe orphan/finished tab")
	}

	// Negative assertion: when reap fails (e.g. transport failure), ObserveReconciliation must fail closed
	failRestore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		return CloseReceipt{}, errors.New("injected transport failure")
	})
	defer failRestore()
	if err := o.ObserveReconciliation(context.Background()); err == nil {
		t.Fatal("ObserveReconciliation must return error and fail closed when CAS reap fails")
	}
}

func TestLegacyTabMigration_BackfillsExactGenerationAndSurvivesRestart(t *testing.T) {
	store := &JSONLLegacyTabStateStore{Path: t.TempDir() + "/legacy.jsonl"}
	srv := NewFakeCompareCloseServer()
	srv.PutTab(LiveTab{WorkspaceID: "wK", TabID: "wK:t2W2", Generation: 42})
	restore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		return srv.CompareAndClose(req), nil
	})
	defer restore()

	reader := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wK:t2W2", WorkspaceID: "wK", Generation: "42", AgentStatus: "idle"}}),
		agents:  present([]AgentEntry{}),
		board:   present(BoardTruth{TaskRef: "FAC-355", Status: "to-do"}),
		binding: present(TabBinding{TabID: "wK:t2W2", Workspace: "wK", PaneID: "wK:p2W2", TaskRef: "FAC-355"}),
	}
	first := &ProductionReconciliationObserver{Workspace: "wK", Reader: reader, LegacyStore: store}
	if err := first.ObserveReconciliation(context.Background()); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	state, ok, err := store.Lookup(context.Background(), "wK", "wK:t2W2")
	if err != nil || !ok || state.Action != legacyActionBackfill || state.Binding.Generation != "42" {
		t.Fatalf("backfill state=%+v found=%t err=%v", state, ok, err)
	}

	// A fresh observer has no process-local binding. It must recover the
	// exact persisted binding after restart, not emit the old missing-generation
	// block again.
	restartedReader := reader
	restartedReader.binding = Authority[TabBinding]{State: EvidenceAbsent}
	restartedReader.tabs = present([]TabRecord{{TabID: "wK:t2W2", WorkspaceID: "wK", AgentStatus: "idle"}})
	second := &ProductionReconciliationObserver{Workspace: "wK", Reader: restartedReader, LegacyStore: store}
	if err := second.ObserveReconciliation(context.Background()); err != nil {
		t.Fatalf("restart migration: %v", err)
	}
	if got := second.Decisions()[0].Class; got != TabSafeOrphan {
		t.Fatalf("restart decision=%s, want safe-orphan from durable backfill", got)
	}
}

func TestLegacyTabMigration_TombstoneIsOneShotAndSuppressesRepeatedBlock(t *testing.T) {
	store := &JSONLLegacyTabStateStore{Path: t.TempDir() + "/legacy.jsonl"}
	reader := fixtureReader{
		tabs:    present([]TabRecord{{TabID: "wK:t2W2", WorkspaceID: "wK", AgentStatus: "idle"}}),
		agents:  present([]AgentEntry{{Name: "task-fac-355", TabID: "wK:t2W2", PaneID: "wK:p2W2", Workspace: "wK", Status: "idle"}}),
		binding: present(TabBinding{TabID: "wK:t2W2", Workspace: "wK", PaneID: "wK:p2W2", TaskRef: "FAC-355"}),
	}
	for cycle := 0; cycle < 2; cycle++ {
		o := &ProductionReconciliationObserver{Workspace: "wK", Reader: reader, LegacyStore: store}
		if err := o.ObserveReconciliation(context.Background()); err != nil {
			t.Fatalf("cycle %d: tombstoned legacy tab must converge: %v", cycle, err)
		}
		if got := o.Decisions()[0]; got.Class != TabLegacyCleanup || strings.Contains(strings.Join(got.Evidence, " "), "BLOCKED:") {
			t.Fatalf("cycle %d decision=%+v, want non-blocking tombstone", cycle, got)
		}
	}
	b, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; lines != 1 {
		t.Fatalf("tombstone records=%d, want one durable record; %s", lines, b)
	}
}
