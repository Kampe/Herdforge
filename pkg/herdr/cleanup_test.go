package herdr

import (
	"errors"
	"testing"
)

func cleanOrphan(ref string) TabObservation {
	return TabObservation{
		TabID: "wF:t1", Generation: "g1", Label: "Herdforge · " + ref,
		TaskRef: ref, TaskStatus: "to-do", AgentStatus: "unknown",
		Worktree: WorktreeEvidence{Known: true}, Evidence: completeFixtureEvidence(),
	}
}

func completeFixtureEvidence() TabEvidence {
	p := SourceEvidence{State: EvidencePresent}
	return TabEvidence{Board: p, Agent: p, Lifecycle: p, Worktree: p, Review: p, Mail: p, Process: p, Protection: p}
}

func TestReconcileTabsShellOnlyTaskLabelIsSafeOrphan(t *testing.T) {
	d := ReconcileTabs([]TabObservation{cleanOrphan("FAC-72")})[0]
	if d.Class != TabSafeOrphan || !d.CloseEligible {
		t.Fatalf("shell-only task label = %+v, want safe-orphan and close eligible", d)
	}
}

func TestReconcileTabsMissingEachEvidenceSourceBlocks(t *testing.T) {
	base := cleanOrphan("FAC-72")
	checks := []struct {
		name  string
		clear func(*TabEvidence)
	}{
		{"board", func(e *TabEvidence) { e.Board = SourceEvidence{} }},
		{"agent", func(e *TabEvidence) { e.Agent = SourceEvidence{} }},
		{"lifecycle", func(e *TabEvidence) { e.Lifecycle = SourceEvidence{} }},
		{"worktree", func(e *TabEvidence) { e.Worktree = SourceEvidence{} }},
		{"review", func(e *TabEvidence) { e.Review = SourceEvidence{} }},
		{"mail", func(e *TabEvidence) { e.Mail = SourceEvidence{} }},
		{"process", func(e *TabEvidence) { e.Process = SourceEvidence{} }},
		{"protection", func(e *TabEvidence) { e.Protection = SourceEvidence{} }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			tc.clear(&o.Evidence)
			d := ReconcileTabs([]TabObservation{o})[0]
			if d.Class != TabBlocked || d.CloseEligible {
				t.Fatalf("missing %s evidence was not fail-closed: %+v", tc.name, d)
			}
		})
	}
}

func TestReconcileTabsEmptyGenerationNeverCloses(t *testing.T) {
	o := cleanOrphan("FAC-72")
	o.Generation = ""
	d := ReconcileTabs([]TabObservation{o})[0]
	if d.Class != TabBlocked || d.CloseEligible {
		t.Fatalf("empty generation authorized cleanup: %+v", d)
	}
}

func TestReconcileTabsPreservesEveryUnsafeGate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TabObservation)
		want   TabClass
	}{
		{"dirty", func(o *TabObservation) { o.Worktree.Dirty = true }, TabBlocked},
		{"unique commit", func(o *TabObservation) { o.Worktree.UniqueCommits = true }, TabBlocked},
		{"unique ref", func(o *TabObservation) { o.Worktree.UniqueRefs = true }, TabBlocked},
		{"pending review", func(o *TabObservation) { o.PendingReview = true }, TabPreservedReview},
		{"pending callback", func(o *TabObservation) { o.PendingCallback = true }, TabBlocked},
		{"active session", func(o *TabObservation) { o.SessionID = "s1"; o.SessionGeneration = "g1" }, TabActive},
		{"recycled session", func(o *TabObservation) { o.SessionID = "s2"; o.SessionGeneration = "g2" }, TabBlocked},
		{"unknown worktree", func(o *TabObservation) { o.Worktree.Known = false }, TabBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := cleanOrphan("FAC-72")
			tc.mutate(&o)
			d := ReconcileTabs([]TabObservation{o})[0]
			if d.Class != tc.want || d.CloseEligible {
				t.Fatalf("decision = %+v, want class %s and no close", d, tc.want)
			}
		})
	}
}

func TestReconcileTabsStandingAndExplicitShellAreVisibleNotWorking(t *testing.T) {
	standing := cleanOrphan("FAC-72")
	standing.Standing = true
	user := cleanOrphan("FAC-72")
	user.ExplicitUserShell = true
	got := ReconcileTabs([]TabObservation{standing, user})
	if got[0].Class != TabStanding || got[1].Class != TabUserShell {
		t.Fatalf("classes = %s, %s", got[0].Class, got[1].Class)
	}
	for _, d := range got {
		if d.CloseEligible {
			t.Fatalf("non-owned tab became close eligible: %+v", d)
		}
	}
}

func TestAuthorizeCloseGenerationFence(t *testing.T) {
	initial := cleanOrphan("FAC-72")
	d := ReconcileTabs([]TabObservation{initial})[0]
	current := initial
	current.Generation = "g2"
	if _, err := AuthorizeClose(d, current); err == nil {
		t.Fatal("recycled tab must not be closed")
	}
	if req, err := AuthorizeClose(d, initial); err != nil || req.Generation != "g1" {
		t.Fatalf("matching exact-id readback should produce a fenced request: req=%+v err=%v", req, err)
	}
}
func TestTabCloseCASIsTypedBlockedUntilFAC180(t *testing.T) {
	err := TabCloseCAS(CloseRequest{TabID: "wF:t1", Generation: "g1"})
	var blocked *CloseUnavailableError
	if !errors.As(err, &blocked) {
		t.Fatalf("want typed unavailable error, got %T %v", err, err)
	}
}

func TestUnknownTaskStatusBlocks(t *testing.T) {
	o := cleanOrphan("FAC-72")
	o.TaskStatus = ""
	d := ReconcileTabs([]TabObservation{o})[0]
	if d.Class != TabBlocked || d.CloseEligible {
		t.Fatalf("unknown task status closed: %+v", d)
	}
}

func TestProjectFleetStatusExcludesShellsAndUnknowns(t *testing.T) {
	decisions := ReconcileTabs([]TabObservation{
		cleanOrphan("FAC-72"),
		func() TabObservation {
			o := cleanOrphan("FAC-73")
			o.SessionID = "s1"
			o.SessionGeneration = "g1"
			o.AgentStatus = "working"
			return o
		}(),
		func() TabObservation { o := cleanOrphan("FAC-74"); o.Evidence.Board = SourceEvidence{}; return o }(),
	})
	p := ProjectFleetStatus(decisions, 2)
	if p.Working != 1 || p.Capacity != 1 || p.Unknown != 1 {
		t.Fatalf("projection = %+v, want working=1 capacity=1 unknown=1", p)
	}
}

func TestReconcileTabsTerminalSessionRequiresExactFence(t *testing.T) {
	o := cleanOrphan("FAC-72")
	o.SessionID, o.AgentStatus = "session-1", "done"
	d := ReconcileTabs([]TabObservation{o})[0]
	if d.Class != TabBlocked || d.CloseEligible {
		t.Fatalf("terminal session without generation was closable: %+v", d)
	}
	o.SessionGeneration = o.Generation
	d = ReconcileTabs([]TabObservation{o})[0]
	if !d.CloseEligible {
		t.Fatalf("terminal session with exact fence was not closable: %+v", d)
	}
}

func TestSelectCleanupCandidates(t *testing.T) {
	standing := map[string]bool{"forge-worker": true, "forge-reviewer": true, "forge-forge-smith": true}
	agents := []AgentEntry{
		{Name: "task-fac-59", Status: "idle", TaskRef: "FAC-59", TaskStatus: "done", WorktreeKnown: true, Generation: "g1", Evidence: completeFixtureEvidence(), TabID: "wF:tD"},
		{Name: "task-fac-60", Status: "done", TaskRef: "FAC-60", TaskStatus: "done", WorktreeKnown: true, Generation: "g1", Evidence: completeFixtureEvidence(), TabID: "wF:tF"},
		{Name: "forge-worker", Status: "idle", TabID: "wF:t7"},        // standing: kept
		{Name: "forge-reviewer", Status: "done", TabID: "wF:t8"},      // standing: kept
		{Name: "cs-orchestrator", Status: "idle", TabID: "wF:t9"},     // orchestrator: kept
		{Name: "task-fac-99", Status: "working", TabID: "wF:tA"},      // alive: kept
		{Name: "", Kind: "claude", Status: "working", TabID: "wF:tC"}, // unnamed: kept
		{Name: "", Kind: "opencode", Status: "idle", TabID: "wF:t1"},  // unnamed: kept
	}
	got := SelectCleanupCandidates(agents, standing)
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(got), got)
	}
	want := map[string]bool{"task-fac-59": true, "task-fac-60": true}
	for _, c := range got {
		if !want[c.Name] {
			t.Errorf("unexpected candidate %s", c.Name)
		}
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
