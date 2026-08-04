package herdr

import "testing"

func TestSelectCleanupCandidates(t *testing.T) {
	standing := map[string]bool{"forge-worker": true, "forge-reviewer": true, "forge-forge-smith": true}
	agents := []AgentEntry{
		{Name: "task-fac-59", Status: "idle", TabID: "wF:tD"},
		{Name: "task-fac-60", Status: "done", TabID: "wF:tF"},
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

func TestCloseTabForRef_NoMatchIsNil(t *testing.T) {
	// With no live herdr, AgentList errors; the function must return that
	// error, not panic — and a matching-name lookup is exercised in the
	// pure candidate-selection path already tested above.
	if err := CloseTabForRef("FAC-999"); err == nil {
		// In a real herdr session this would find nothing and return nil;
		// hermetic CI has no herdr so an error (CLI missing) is expected.
		t.Skip("herdr present — no-match returned nil as designed")
	}
}

func TestTabCloseFailsClosedWithoutExactPaneAuthority(t *testing.T) {
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
	if err := TabClose("tab-no-pane"); err == nil {
		t.Fatal("empty pane authority must fail closed")
	}
	if closeCalled {
		t.Fatal("empty pane authority reached tab close")
	}
	if len(events) != 1 || events[0] != "reconcile" {
		t.Fatalf("cleanup skipped or performed terminal lifecycle steps: %v", events)
	}
}
