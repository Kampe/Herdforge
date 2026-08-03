package herdr

import (
	"errors"
	"testing"
)

func present[T any](v T) Authority[T] { return Authority[T]{State: EvidencePresent, Value: v} }

func authFixture(status string) AuthoritySnapshot {
	return AuthoritySnapshot{
		Board:     present(BoardTruth{TaskRef: "FAC-72", Status: status}),
		Agent:     Authority[AgentTruth]{State: EvidenceAbsent},
		Lifecycle: Authority[LifecycleTruth]{State: EvidenceAbsent},
		Worktree:  present(WorktreeEvidence{Known: true}),
		Review:    present(ReviewTruth{}), Mail: present(MailTruth{}), Process: present(ProcessTruth{}),
		Protection: present(ProtectionTruth{}),
	}
}

func boundFixture(status string) TabObservation {
	b := TabBinding{TabID: "wF:t1", Generation: "g1", TaskRef: "FAC-72", PaneID: "wF:p1"}
	return AssembleBoundObservation(b, authFixture(status))
}

func TestSocketShellOnlyTaskShapeIsSafeOrphan(t *testing.T) {
	d := ReconcileTabs([]TabObservation{boundFixture("to-do")})[0]
	if d.Class != TabSafeOrphan || !d.CloseEligible {
		t.Fatalf("decision=%+v", d)
	}
}

func TestMissingEachAuthorityBlocks(t *testing.T) {
	fields := []struct {
		name  string
		clear func(*AuthoritySnapshot)
	}{
		{"board", func(a *AuthoritySnapshot) { a.Board = Authority[BoardTruth]{} }},
		{"agent", func(a *AuthoritySnapshot) { a.Agent = Authority[AgentTruth]{} }},
		{"lifecycle", func(a *AuthoritySnapshot) { a.Lifecycle = Authority[LifecycleTruth]{} }},
		{"worktree", func(a *AuthoritySnapshot) { a.Worktree = Authority[WorktreeEvidence]{} }},
		{"review", func(a *AuthoritySnapshot) { a.Review = Authority[ReviewTruth]{} }},
		{"mail", func(a *AuthoritySnapshot) { a.Mail = Authority[MailTruth]{} }},
		{"process", func(a *AuthoritySnapshot) { a.Process = Authority[ProcessTruth]{} }},
		{"protection", func(a *AuthoritySnapshot) { a.Protection = Authority[ProtectionTruth]{} }},
	}
	for _, tc := range fields {
		t.Run(tc.name, func(t *testing.T) {
			o := boundFixture("to-do")
			tc.clear(&o.Authorities)
			d := ReconcileTabs([]TabObservation{o})[0]
			if d.Class != TabBlocked || d.CloseEligible {
				t.Fatalf("decision=%+v", d)
			}
		})
	}
}

func TestAuthorityValueAndStateCannotContradict(t *testing.T) {
	o := boundFixture("to-do")
	o.Authorities.Board = Authority[BoardTruth]{State: EvidencePresent, Value: BoardTruth{TaskRef: "FAC-99", Status: "to-do"}}
	if d := ReconcileTabs([]TabObservation{o})[0]; d.Class != TabBlocked {
		t.Fatalf("mismatched value closed: %+v", d)
	}
}

func TestUnknownStatusAndRecoveryPreserve(t *testing.T) {
	unknown := boundFixture("")
	if d := ReconcileTabs([]TabObservation{unknown})[0]; d.Class != TabBlocked {
		t.Fatalf("unknown status=%+v", d)
	}
	recovering := boundFixture("recovering")
	if d := ReconcileTabs([]TabObservation{recovering})[0]; d.Class != TabRecovering || d.CloseEligible {
		t.Fatalf("recovering=%+v", d)
	}
}

func TestPresentIdleOrUnknownAgentCannotBecomeSafeOrphan(t *testing.T) {
	for _, status := range []string{"idle", "unknown", ""} {
		t.Run(status, func(t *testing.T) {
			o := boundFixture("to-do")
			o.Authorities.Agent = present(AgentTruth{Status: status})
			d := ReconcileTabs([]TabObservation{o})[0]
			if d.Class != TabBlocked || d.CloseEligible {
				t.Fatalf("present agent status %q was closable: %+v", status, d)
			}
		})
	}
}

func TestWorktreeAndForegroundProcessUnknownsPreserve(t *testing.T) {
	o := boundFixture("to-do")
	o.Authorities.Worktree.Value.Known = false
	if d := ReconcileTabs([]TabObservation{o})[0]; d.Class != TabBlocked {
		t.Fatalf("unknown worktree=%+v", d)
	}
	o = boundFixture("to-do")
	o.Authorities.Process = present(ProcessTruth{Alive: true})
	if d := ReconcileTabs([]TabObservation{o})[0]; d.Class != TabBlocked {
		t.Fatalf("unowned process=%+v", d)
	}
	o = boundFixture("to-do")
	o.Authorities.Agent = present(AgentTruth{Status: "working"})
	o.Authorities.Process = present(ProcessTruth{Alive: true})
	if d := ReconcileTabs([]TabObservation{o})[0]; d.Class != TabBlocked {
		t.Fatalf("sessionless process=%+v", d)
	}
}

func TestDoneBoardNeedsTerminalIntegration(t *testing.T) {
	o := boundFixture("done")
	if d := ReconcileTabs([]TabObservation{o})[0]; d.Class != TabBlocked {
		t.Fatalf("done without integration=%+v", d)
	}
	o.Authorities.Lifecycle = present(LifecycleTruth{State: "reconciled"})
	if d := ReconcileTabs([]TabObservation{o})[0]; !d.CloseEligible || d.Class != TabSafeFinished {
		t.Fatalf("terminal integration=%+v", d)
	}
}

func TestDirtyUniqueReviewAndActiveSessionBlock(t *testing.T) {
	mutations := []struct {
		name  string
		apply func(*TabObservation)
	}{
		{"dirty", func(o *TabObservation) { o.Authorities.Worktree.Value.Dirty = true }},
		{"unique", func(o *TabObservation) { o.Authorities.Worktree.Value.UniqueCommits = true }},
		{"review", func(o *TabObservation) { o.Authorities.Review.Value.Pending = true }},
		{"active", func(o *TabObservation) {
			o.Authorities.Agent = Authority[AgentTruth]{State: EvidencePresent, Value: AgentTruth{Status: "working", SessionID: "s1", SessionGeneration: "g1", PaneID: "wF:p1"}}
			o.Authorities.Process = Authority[ProcessTruth]{State: EvidencePresent, Value: ProcessTruth{Alive: true}}
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			o := boundFixture("to-do")
			tc.apply(&o)
			if d := ReconcileTabs([]TabObservation{o})[0]; d.CloseEligible {
				t.Fatalf("unsafe mutation closed: %+v", d)
			}
		})
	}
}

func TestExactBindingAndGenerationRequired(t *testing.T) {
	o := boundFixture("to-do")
	o.Binding.Generation = ""
	o.Generation = ""
	if d := ReconcileTabs([]TabObservation{o})[0]; d.Class != TabBlocked {
		t.Fatalf("empty generation=%+v", d)
	}
	o = boundFixture("to-do")
	o.Binding.TaskRef = "FAC-99"
	if d := ReconcileTabs([]TabObservation{o})[0]; d.Class != TabBlocked {
		t.Fatalf("inferred binding=%+v", d)
	}
}

func TestAuthorizeAndLiveCloseRemainSeparate(t *testing.T) {
	o := boundFixture("to-do")
	d := ReconcileTabs([]TabObservation{o})[0]
	if _, err := AuthorizeClose(d, o); err != nil {
		t.Fatal(err)
	}
	var blocked *CloseUnavailableError
	if err := TabCloseCAS(CloseRequest{TabID: "wF:t1", Generation: "g1"}); !errors.As(err, &blocked) {
		t.Fatalf("want typed BLOCKED, got %T", err)
	}
}

func TestLegacyAgentListCannotAuthorizeCandidate(t *testing.T) {
	// Status-based SelectCleanupCandidates is observe-only (main behavior).
	// It must never become a close authorization: mutation Cleanup and
	// unfenced TabClose remain BLOCKED without FAC-180 compare-and-close.
	got := SelectCleanupCandidates([]AgentEntry{{Name: "task-fac-72", Status: "done", TabID: "wF:t1"}}, nil)
	if len(got) != 1 {
		t.Fatalf("observe candidates = %+v, want 1 status-based candidate", got)
	}
	_, errs := Cleanup(nil, false)
	if len(errs) == 0 {
		t.Fatal("mutation Cleanup must fail closed without FAC-180 fence")
	}
	if err := TabClose(got[0].TabID); err == nil {
		t.Fatal("unfenced TabClose must fail closed")
	}
}

func TestFleetProjectionSeparatesCapacityClasses(t *testing.T) {
	active := boundFixture("to-do")
	active.Authorities.Agent = Authority[AgentTruth]{State: EvidencePresent, Value: AgentTruth{Status: "working", SessionID: "s", SessionGeneration: "g1", PaneID: "wF:p1"}}
	active.Authorities.Process = Authority[ProcessTruth]{State: EvidencePresent, Value: ProcessTruth{Alive: true}}
	standing := boundFixture("to-do")
	standing.Binding.ControlSeat = true
	standing.Authorities.Protection = Authority[ProtectionTruth]{State: EvidencePresent, Value: ProtectionTruth{Standing: true}}
	decisions := ReconcileTabs([]TabObservation{active, standing})
	p := ProjectFleetStatus(decisions, 2)
	if p.Working != 1 || p.Capacity != 1 || p.Standing != 1 {
		t.Fatalf("projection=%+v", p)
	}
}

func TestCloseTabForRef_FailsClosedWithoutFencedBinding(t *testing.T) {
	// FAC-180: legacy ref lookup cannot establish generation/session fence.
	if err := CloseTabForRef("FAC-999"); err == nil {
		t.Fatal("CloseTabForRef must fail closed without FAC-180 compare-and-close")
	}
}

func TestTabCloseFailsClosedWithoutCompareAndClose(t *testing.T) {
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	closeCalled := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			closeCalled = true
		}
		return `{}`, nil
	}
	if err := TabClose("tab-no-pane"); err == nil {
		t.Fatal("unfenced TabClose must fail closed")
	}
	if closeCalled {
		t.Fatal("unfenced TabClose reached plain herdr tab close")
	}
}

func TestLegacyTabCloseWithLifecycleStillRequiresPaneAndFence(t *testing.T) {
	events := []string{}
	lc := &rollbackLifecycle{bound: true, events: &events}
	toolChildMu.Lock()
	toolChildByTab["tab-no-pane"] = lc
	toolChildMu.Unlock()
	defer dropToolChild("tab-no-pane", "")
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	closeCalled := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
			closeCalled = true
		}
		return `{}`, nil
	}
	if err := LegacyTabCloseWithLifecycle("tab-no-pane"); err == nil {
		t.Fatal("empty pane authority must fail closed")
	}
	if closeCalled {
		t.Fatal("empty pane authority reached tab close")
	}
	if len(events) != 1 || events[0] != "reconcile" {
		t.Fatalf("cleanup skipped or performed terminal lifecycle steps: %v", events)
	}
}

func TestTabCloseCASUsesMainFAC180Adapter(t *testing.T) {
	err := TabCloseCAS(CloseRequest{TabID: "wF:t1", Generation: "g1"})
	var blocked *CloseUnavailableError
	if !errors.As(err, &blocked) {
		t.Fatalf("want typed unavailable error, got %T %v", err, err)
	}
}

func TestFleetProjectionFailsClosedOnUnknown(t *testing.T) {
	p := ProjectFleetStatus([]TabDecision{{Class: TabBlocked}}, 4)
	if p.Unknown != 1 || p.Capacity != 0 {
		t.Fatalf("unknown fleet was treated as available: %+v", p)
	}
}
