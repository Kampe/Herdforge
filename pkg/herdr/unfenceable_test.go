package herdr

import (
	"context"
	"strings"
	"testing"
)

func boundTabWith(generation string) TabObservation {
	ok := AuthoritySnapshot{}
	// Every authority must read as authoritatively absent, or the decision
	// blocks for a different reason and this test proves nothing.
	ok.Board.State = EvidenceAbsent
	ok.Agent.State = EvidenceAbsent
	ok.Lifecycle.State = EvidenceAbsent
	ok.Worktree.State = EvidenceAbsent
	ok.Review.State = EvidenceAbsent
	ok.Mail.State = EvidenceAbsent
	ok.Process.State = EvidenceAbsent
	ok.Protection.State = EvidenceAbsent
	return TabObservation{
		TabID:       "wK:t2",
		Binding:     TabBinding{TabID: "wK:t2", Generation: generation},
		Authorities: ok,
	}
}

// TestTaskBoundGenerationlessTabStaysBlocked preserves the deliberate invariant
// this change must not trade away.
//
// A generationless tab bound to a TASK is genuinely unsafe to reason about: an
// active worker whose identity cannot be fenced must halt reconciliation. Three
// pre-existing tests defend that, and they are right. FAC-571 reclassifies only
// the case where blocking is pure noise.
func TestTaskBoundGenerationlessTabStaysBlocked(t *testing.T) {
	obs := boundTabWith("")
	obs.Binding.TaskRef = "FAC-304"
	d := reconcileBoundTab(obs)
	if d.Class != TabBlocked {
		t.Fatalf("a task-bound generationless tab must stay blocked, got %s", d.Class)
	}
	if d.CloseEligible {
		t.Error("and must never be close-eligible")
	}
}

// TestMissingGenerationIsUnfenceableNotBlocked is the FAC-571 gate.
//
// A missing generation was TabBlocked, so on a herdr build that never supplies
// one, reconciliation reported a blanket BLOCKED for every tab and the whole
// subsystem was jammed by a per-item capability gap.
func TestMissingGenerationIsUnfenceableNotBlocked(t *testing.T) {
	d := reconcileBoundTab(boundTabWith(""))
	if d.Class == TabBlocked {
		t.Fatal("a build that cannot supply a generation is a capability gap, not an unreadable tab")
	}
	if d.Class != TabUnfenceable {
		t.Fatalf("class = %s, want %s", d.Class, TabUnfenceable)
	}
	// THE decision this card records: no mutation without fencing evidence.
	if d.CloseEligible {
		t.Error("an unfenceable tab must never be close-eligible")
	}
	if len(d.Evidence) == 0 {
		t.Error("the classification must carry its reason")
	}
}

// A known-and-explained state must not consume capacity the way genuine
// uncertainty does; counting it as unknown zeroed capacity for every lane.
func TestUnfenceableDoesNotZeroCapacity(t *testing.T) {
	decisions := []TabDecision{
		{TabID: "a", Class: TabUnfenceable},
		{TabID: "b", Class: TabUnfenceable},
	}
	p := ProjectFleetStatus(decisions, 10)
	if p.Unknown != 0 {
		t.Errorf("unfenceable must not be counted as unknown, got unknown=%d", p.Unknown)
	}
	if p.Unfenceable != 2 {
		t.Errorf("unfenceable count = %d, want 2", p.Unfenceable)
	}
	if p.Capacity == 0 {
		t.Error("a known capability gap must not zero fleet capacity")
	}

	// Genuine uncertainty still does zero it: that behaviour is intentional and
	// must not be relaxed by this change.
	unknown := ProjectFleetStatus([]TabDecision{{TabID: "c", Class: TabBlocked}}, 10)
	if unknown.Unknown != 1 || unknown.Capacity != 0 {
		t.Errorf("an unknown tab must still zero capacity, got %+v", unknown)
	}
}

// A tab that CAN be fenced still goes through every other check, so this change
// did not turn the generation test into a bypass.
func TestPresentGenerationStillReconciles(t *testing.T) {
	d := reconcileBoundTab(boundTabWith("7"))
	if d.Class == TabUnfenceable {
		t.Fatal("a tab with a real generation is not unfenceable")
	}
	if d.Generation != "7" {
		t.Errorf("generation must be carried through, got %q", d.Generation)
	}
}

// An unbound tab is still BLOCKED: a missing binding is not a capability gap,
// it is a tab we cannot identify.
func TestMissingBindingStaysBlocked(t *testing.T) {
	obs := boundTabWith("7")
	obs.Binding.TabID = ""
	if d := reconcileBoundTab(obs); d.Class != TabBlocked {
		t.Errorf("an unidentifiable tab must stay blocked, got %s", d.Class)
	}
}

// A generationless tab with a LIVE agent stays blocked even without a task
// ref: a process behind the tab is exactly when an unfenceable identity matters.
func TestLiveAgentGenerationlessTabStaysBlocked(t *testing.T) {
	obs := boundTabWith("")
	obs.Authorities.Agent = Authority[AgentTruth]{State: EvidencePresent, Value: AgentTruth{Status: "working"}}
	if d := reconcileBoundTab(obs); d.Class != TabBlocked {
		t.Errorf("a live generationless tab must stay blocked, got %s", d.Class)
	}
}

// TestAllBlockedTabsAreReported is the other half of FAC-571.
//
// Returning on the FIRST blocked tab made a multi-tab problem look like a
// single-tab one: an operator could not tell whether fixing that tab would
// unblock reconciliation or reveal four more behind it. The decision set was
// always complete; only the message was lossy.
func TestAllBlockedTabsAreReported(t *testing.T) {
	r := fixtureReader{
		tabs: present([]TabRecord{
			{TabID: "wK:t1", WorkspaceID: "wK", AgentStatus: "working"},
			{TabID: "wK:t2", WorkspaceID: "wK", AgentStatus: "working"},
		}),
		agents: present([]AgentEntry{
			{Kind: "codex", Name: "task-a", TabID: "wK:t1", PaneID: "wK:p1", Workspace: "wK", Status: "working"},
			{Kind: "codex", Name: "task-b", TabID: "wK:t2", PaneID: "wK:p2", Workspace: "wK", Status: "working"},
		}),
	}
	o := &ProductionReconciliationObserver{
		Workspace: "wK", Reader: r,
		TaskBinding: func(_ context.Context, tab TabRecord, _ AgentEntry) Authority[TabBinding] {
			return present(TabBinding{TabID: tab.TabID, Workspace: "wK", TaskRef: "FAC-1", Role: "worker"})
		},
	}
	err := o.ObserveReconciliation(context.Background())
	if err == nil {
		t.Fatal("generationless worker tabs must still block")
	}
	msg := err.Error()
	for _, want := range []string{"wK:t1", "wK:t2", "2 of 2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error must account for every blocked tab; missing %q in: %s", want, msg)
		}
	}
}
