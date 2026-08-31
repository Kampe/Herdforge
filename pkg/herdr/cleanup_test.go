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
			o.Authorities.Process = Authority[ProcessTruth]{State: EvidencePresent, Value: ProcessTruth{Alive: true, SessionID: "s1", SessionGeneration: "g1"}}
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
	tests := []struct {
		name           string
		inventory      string
		inventoryErr   error
		wantCandidates int
		wantErrors     int
		wantBlocked    bool
	}{
		{name: "empty", inventory: `{"result":{"agents":[],"type":"agents"}}`},
		{name: "active", inventory: `{"result":{"agents":[{"name":"task-fac-72","agent_status":"working","tab_id":"wF:t1"}],"type":"agents"}}`},
		{name: "done", inventory: `{"result":{"agents":[{"name":"task-fac-72","agent_status":"done","tab_id":"wF:t1"}],"type":"agents"}}`, wantCandidates: 1, wantErrors: 1, wantBlocked: true},
		{name: "malformed", inventory: `{`, wantErrors: 1},
		{name: "unavailable", inventoryErr: errors.New("herdr unavailable"), wantErrors: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			restore := SetRunHerdrForTest(func(args ...string) (string, error) {
				calls++
				if len(args) != 2 || args[0] != "agent" || args[1] != "list" {
					t.Fatalf("unexpected herdr call: %v", args)
				}
				return tc.inventory, tc.inventoryErr
			})
			t.Cleanup(restore)

			got, errs := Cleanup(nil, false)
			if calls != 1 {
				t.Fatalf("agent list calls = %d, want 1", calls)
			}
			if len(got) != tc.wantCandidates {
				t.Fatalf("candidates = %+v, want %d", got, tc.wantCandidates)
			}
			if len(errs) != tc.wantErrors {
				t.Fatalf("errors = %v, want %d", errs, tc.wantErrors)
			}

			if tc.wantBlocked {
				var blocked *CloseUnavailableError
				if !errors.As(errs[0], &blocked) {
					t.Fatalf("mutation Cleanup error = %T, want *CloseUnavailableError", errs[0])
				}
				if blocked.TabID != got[0].TabID {
					t.Fatalf("blocked tab = %q, want %q", blocked.TabID, got[0].TabID)
				}
				if err := TabClose(got[0].TabID); !errors.As(err, &blocked) {
					t.Fatalf("unfenced TabClose error = %T, want *CloseUnavailableError", err)
				}
			}
		})
	}
}

func TestFleetProjectionSeparatesCapacityClasses(t *testing.T) {
	active := boundFixture("to-do")
	active.Authorities.Agent = Authority[AgentTruth]{State: EvidencePresent, Value: AgentTruth{Status: "working", SessionID: "s", SessionGeneration: "g1", PaneID: "wF:p1"}}
	active.Authorities.Process = Authority[ProcessTruth]{State: EvidencePresent, Value: ProcessTruth{Alive: true, SessionID: "s", SessionGeneration: "g1"}}
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

func TestProjectLiveFleetStatusClassifiesLiveAgentsWithoutReconciliationAuthorities(t *testing.T) {
	got := ProjectLiveFleetStatus([]AgentEntry{
		{Name: "forge-reviewer", Status: "idle", Workspace: "wF"},
		{Name: "forge-builder", Status: "working", Workspace: "wF"},
		{Name: "task-fac-1", Status: "working", Workspace: "wF"},
		{Name: "task-fac-2", Status: "idle", Workspace: "wF"},
		{Name: "", Status: "working", Workspace: "wF"},
		{Name: "foreign", Status: "working", Workspace: "wOther"},
	}, map[string]bool{"forge-reviewer": true}, "wF", 4)

	if got.Working != 2 || got.Standing != 1 || got.ControlSeats != 1 || got.Unknown != 1 {
		t.Fatalf("live fleet=%+v, want working=2 standing=1 control=1 unknown=1", got)
	}
	if got.Capacity != 0 {
		t.Fatalf("capacity=%d, want 0 while unknown live state remains", got.Capacity)
	}
}

// seq 3130 / FAC-660 residual: a hashed standing agent that is working must
// count as working capacity. Standing names used to win the switch before live
// status, so herd status reported working=1 standing=13 while herdr showed a
// full busy forge-* fleet.
func TestProjectLiveFleetStatusCountsWorkingStandingAsWorking(t *testing.T) {
	got := ProjectLiveFleetStatus([]AgentEntry{
		{Name: "forge-docs-custodian-2918de97b5", Status: "working", Workspace: "wB"},
		{Name: "forge-platform-ops-2918de97b5", Status: "idle", Workspace: "wB"},
		{Name: "forge-api-crusader-2918de97b5", Status: "working", Workspace: "wB"},
	}, map[string]bool{
		"forge-docs-custodian-2918de97b5": true,
		"forge-platform-ops-2918de97b5":   true,
		"forge-api-crusader-2918de97b5":   true,
	}, "wB", 14)
	if got.Working != 2 {
		t.Fatalf("working=%d, want 2 busy standing-named agents counted as working: %+v", got.Working, got)
	}
	if got.Standing != 1 {
		t.Fatalf("standing=%d, want 1 idle standing agent: %+v", got.Standing, got)
	}
}

func TestProjectLiveFleetStatusSeparatesQueuedAssignmentFromWorkingGoal(t *testing.T) {
	got := ProjectLiveFleetStatus([]AgentEntry{
		{Name: "forge-worker", Status: "working", AssignmentStatus: "queued", Workspace: "wF"},
		{Name: "forge-consumer", Status: "working", AssignmentStatus: "consumed", Workspace: "wF"},
	}, nil, "wF", 2)
	if got.Queued != 1 || got.Working != 1 {
		t.Fatalf("fleet=%+v, want one queued and one consumed/working lane", got)
	}
	if got.Classes[TabQueued] != 1 {
		t.Fatalf("queued class=%d, want 1: %+v", got.Classes[TabQueued], got.Classes)
	}
	if got.Capacity != 0 {
		t.Fatalf("capacity=%d, want 0 while queued work owns a lane", got.Capacity)
	}
}

func TestNormalizeAssignmentStatusIsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		input, want string
	}{
		{"queued", "queued"},
		{"staged", "queued"},
		{"consumed", "consumed"},
		{"mystery", "unknown"},
	} {
		if got := NormalizeAssignmentStatus(tc.input); got != tc.want {
			t.Errorf("NormalizeAssignmentStatus(%q)=%q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestActiveProjectionRequiresExactSessionProcessAndPane(t *testing.T) {
	tests := []struct {
		name  string
		apply func(*TabObservation)
	}{
		{"no session", func(o *TabObservation) {
			o.Authorities.Agent = present(AgentTruth{Status: "working", PaneID: "wF:p1"})
			o.Authorities.Process = present(ProcessTruth{Alive: true})
		}},
		{"no process", func(o *TabObservation) {
			o.Authorities.Agent = present(AgentTruth{Status: "working", SessionID: "s1", SessionGeneration: "g1", PaneID: "wF:p1"})
		}},
		{"missing pane", func(o *TabObservation) {
			o.Authorities.Agent = present(AgentTruth{Status: "working", SessionID: "s1", SessionGeneration: "g1"})
			o.Authorities.Process = present(ProcessTruth{Alive: true, SessionID: "s1", SessionGeneration: "g1"})
		}},
		{"mismatched session process", func(o *TabObservation) {
			o.Authorities.Agent = present(AgentTruth{Status: "working", SessionID: "s1", SessionGeneration: "g1", PaneID: "wF:p1"})
			o.Authorities.Process = present(ProcessTruth{Alive: true, SessionID: "s2", SessionGeneration: "g1"})
		}},
		{"mismatched pane", func(o *TabObservation) {
			o.Authorities.Agent = present(AgentTruth{Status: "working", SessionID: "s1", SessionGeneration: "g1", PaneID: "wF:p2"})
			o.Authorities.Process = present(ProcessTruth{Alive: true, SessionID: "s1", SessionGeneration: "g1"})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := boundFixture("to-do")
			tc.apply(&o)
			if d := ReconcileTabs([]TabObservation{o})[0]; d.Class == TabActive || d.CloseEligible {
				t.Fatalf("unsafe active projection=%+v", d)
			}
		})
	}

	o := boundFixture("to-do")
	o.Authorities.Agent = present(AgentTruth{Status: "in-progress", SessionID: "s1", SessionGeneration: "g1", PaneID: "wF:p1"})
	o.Authorities.Process = present(ProcessTruth{Alive: true, SessionID: "s1", SessionGeneration: "g1"})
	if d := ReconcileTabs([]TabObservation{o})[0]; d.Class != TabActive || d.CloseEligible {
		t.Fatalf("valid active projection=%+v", d)
	}
}

// FAC-660: an exact map lookup could not recognise a standing lane, because the
// roster holds "forge-<lane>" while a live agent is "forge-<lane>-<digest>" or
// "standing-<lane>". In the census that produced contradictory counts. Here it
// is worse: an unrecognised standing agent falls through and becomes eligible to
// be CLOSED. A roster that cannot recognise its own lanes hands them to the
// reaper, and they then look like they stopped on their own.
func TestStandingLanesAreNeverReapedWhateverTheyAreNamed(t *testing.T) {
	standing := map[string]bool{"forge-herd-smith": true, "forge-platform-ops": true}
	for _, name := range []string{
		"forge-herd-smith-2918de97b5", // repository-qualified: the live form
		"standing-herd-smith",         // standing-raiser form
		"forge-platform-ops-abc123",
		"forge-herd-smith", // exact form still works
	} {
		if !isStandingAgent(name, standing) {
			t.Errorf("standing lane %q was not recognised and would be reaped", name)
		}
	}
}

// A non-standing agent must still be reapable, or spent reviewers accumulate and
// hold pool leases forever.
func TestNonStandingAgentsRemainReapable(t *testing.T) {
	standing := map[string]bool{"forge-herd-smith": true}
	for _, name := range []string{"review-cha-2796-abc123", "forge-other-lane", "shot-cha-1"} {
		if isStandingAgent(name, standing) {
			t.Errorf("%q is not a standing lane but was treated as one; spent agents would never be reaped", name)
		}
	}
}

// An empty roster must not make everything look standing, which would disable
// reaping entirely.
func TestEmptyRosterDoesNotProtectEverything(t *testing.T) {
	if isStandingAgent("forge-anything-123", map[string]bool{}) {
		t.Fatal("an empty roster must protect nothing, or reaping silently stops")
	}
}

// FAC-714: after #618 made working standing agents consume capacity, the live
// fleet went to capacity=0 with working=11. That zero is TRUE and it names no
// remedy -- and two of those lanes were `done`, holding slots while producing
// nothing.
//
// A saturated fleet with reclaimable lanes and a saturated fleet with none are
// completely different situations that read identically as a zero.
func TestCountReclaimableFindsSettledLanes(t *testing.T) {
	got := CountReclaimable([]AgentEntry{
		{Name: "forge-orchestrator", Status: "done"},
		{Name: "forge-ux-comber-2918de97b5", Status: "done"},
		{Name: "forge-api-crusader-2918de97b5", Status: "working"},
		{Name: "forge-docs-custodian-2918de97b5", Status: "idle"},
	})
	if got != 2 {
		t.Fatalf("reclaimable=%d, want 2 settled lanes", got)
	}
}

func TestIdleIsNotReclaimable(t *testing.T) {
	// An idle lane is waiting for work, not finished with it. Reaping it would
	// destroy a warm lane that is about to be useful, so only SETTLED counts.
	if got := CountReclaimable([]AgentEntry{{Name: "forge-perf-cost-guard", Status: "idle"}}); got != 0 {
		t.Fatalf("an idle lane was reported reclaimable: %d", got)
	}
}

func TestReclaimableDoesNotInflateCapacity(t *testing.T) {
	// A settled lane genuinely occupies its slot until something reaps it.
	// Counting it as free invites dispatch into a seat that is still taken --
	// trading an honest zero for an optimistic lie.
	got := ProjectLiveFleetStatus([]AgentEntry{
		{Name: "forge-a", Status: "working", Workspace: "wB"},
		{Name: "forge-b", Status: "done", Workspace: "wB"},
	}, map[string]bool{"forge-a": true, "forge-b": true}, "wB", 1)
	if got.Capacity != 0 {
		t.Fatalf("capacity=%d, want 0: a settled lane must not be counted as a free slot", got.Capacity)
	}
}
