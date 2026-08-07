package herdr

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestDiagnoseRescue_HealthySingleAgentNeverChanged(t *testing.T) {
	panes := []PaneEntry{
		{PaneID: "wK:p1", TabID: "wK:t1", AgentStatus: "working", Name: "task-a", Cwd: "/wt/a", Focused: false},
	}
	rep := DiagnoseRescue(panes, nil, nil)
	if len(rep.Findings) != 0 {
		t.Fatalf("healthy single-agent tab must produce no findings, got %+v", rep.Findings)
	}
	if rep.Next != nil {
		t.Fatalf("Next must be nil for healthy inventory, got %+v", rep.Next)
	}
}

func TestDiagnoseRescue_FocusedPaneNeverChanged(t *testing.T) {
	panes := []PaneEntry{
		{PaneID: "wK:p1", TabID: "wK:t1", AgentStatus: "working", Name: "task-a", Cwd: "/wt/a", Focused: true},
		{PaneID: "wK:p2", TabID: "wK:t1", AgentStatus: "idle", Name: "task-b", Cwd: "/wt/b", Focused: false},
	}
	rep := DiagnoseRescue(panes, nil, nil)
	if len(rep.Findings) == 0 {
		t.Fatal("multi-agent split must emit a finding")
	}
	for _, f := range rep.Findings {
		if f.PaneID == "wK:p1" && f.Safe {
			t.Fatalf("focused pane must never be Safe to mutate: %+v", f)
		}
		if f.PaneID == "wK:p1" && f.Action == RescueActionMove {
			t.Fatalf("focused pane must not be scheduled for move: %+v", f)
		}
	}
	// The non-focused agent should be the move target (keep is focused).
	next, ok := SelectNextRescue(rep.Findings)
	if !ok {
		t.Fatal("expected a safe move of the non-focused agent")
	}
	if next.PaneID != "wK:p2" || next.Action != RescueActionMove {
		t.Fatalf("next=%+v, want move of wK:p2", next)
	}
}

func TestDiagnoseRescue_UnknownPaneNeverMoved(t *testing.T) {
	panes := []PaneEntry{
		{PaneID: "wK:p1", TabID: "wK:t1", AgentStatus: "working", Name: "task-a", Cwd: "/wt/a"},
		{PaneID: "wK:p2", TabID: "wK:t1", AgentStatus: "unknown"},
	}
	rep := DiagnoseRescue(panes, nil, nil)
	for _, f := range rep.Findings {
		if f.PaneID == "wK:p2" && f.Action == RescueActionMove {
			t.Fatalf("unknown pane must never be a move target: %+v", f)
		}
		if f.PaneID == "wK:p1" && f.Action == RescueActionClose {
			t.Fatalf("identifiable agent must never be closed as empty sibling: %+v", f)
		}
	}
	next, ok := SelectNextRescue(rep.Findings)
	if !ok || next.PaneID != "wK:p2" || next.Action != RescueActionClose {
		t.Fatalf("expected close of unknown sibling, got ok=%v next=%+v", ok, next)
	}
}

func TestDiagnoseRescue_EmptySiblingFixture(t *testing.T) {
	panes := []PaneEntry{
		{PaneID: "wF:pA", TabID: "wF:t9", AgentStatus: "idle", Name: "forge-worker", Cwd: "/repo/.herd/worktrees/fac-1", ForegroundCwd: "/repo/.herd/worktrees/fac-1"},
		{PaneID: "wF:pB", TabID: "wF:t9", AgentStatus: "unknown"},
	}
	rep := DiagnoseRescue(panes, nil, nil)
	if len(rep.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d: %+v", len(rep.Findings), rep.Findings)
	}
	f := rep.Findings[0]
	if f.Kind != RescueEmptySibling || f.Action != RescueActionClose || !f.Safe {
		t.Fatalf("finding=%+v", f)
	}
	if f.KeepPaneID != "wF:pA" || f.PaneID != "wF:pB" {
		t.Fatalf("keep/target wrong: %+v", f)
	}
	if f.TabID != "wF:t9" || !strings.Contains(f.Reason, "wF:pB") {
		t.Fatalf("must report exact tab/pane ids and reason: %+v", f)
	}
}

func TestDiagnoseRescue_MultiAgentSplitFixture(t *testing.T) {
	panes := []PaneEntry{
		{PaneID: "wK:p1", TabID: "wK:t1", AgentStatus: "working", Name: "task-a", Cwd: "/wt/a"},
		{PaneID: "wK:p2", TabID: "wK:t1", AgentStatus: "done", Name: "task-b", Cwd: "/wt/b"},
	}
	rep := DiagnoseRescue(panes, nil, nil)
	next, ok := SelectNextRescue(rep.Findings)
	if !ok {
		t.Fatalf("expected multi-agent move, findings=%+v", rep.Findings)
	}
	// Keep working agent; move done agent.
	if next.PaneID != "wK:p2" || next.Kind != RescueMultiAgent || next.Action != RescueActionMove {
		t.Fatalf("next=%+v, want move of done agent wK:p2", next)
	}
	if next.KeepPaneID != "wK:p1" {
		t.Fatalf("keep should be working agent, got %q", next.KeepPaneID)
	}
	if next.BeforeCwd != "/wt/b" {
		t.Fatalf("before cwd=%q", next.BeforeCwd)
	}
}

func TestDiagnoseRescue_MissingCwdRefusesMove(t *testing.T) {
	panes := []PaneEntry{
		{PaneID: "wK:p1", TabID: "wK:t1", AgentStatus: "working", Name: "task-a", Cwd: "/wt/a"},
		{PaneID: "wK:p2", TabID: "wK:t1", AgentStatus: "idle", Name: "task-b"}, // no cwd
	}
	rep := DiagnoseRescue(panes, nil, nil)
	var found bool
	for _, f := range rep.Findings {
		if f.PaneID == "wK:p2" {
			found = true
			if f.Safe || f.Action != RescueActionNone {
				t.Fatalf("missing cwd must refuse move: %+v", f)
			}
			if !strings.Contains(f.RefuseReason, "work preservation unproven") {
				t.Fatalf("refuse reason=%q", f.RefuseReason)
			}
		}
	}
	if !found {
		t.Fatal("expected refuse finding for cwd-less agent")
	}
	// No safe next that targets p2.
	if next, ok := SelectNextRescue(rep.Findings); ok && next.PaneID == "wK:p2" {
		t.Fatalf("unsafe finding must not be Next: %+v", next)
	}
}

func TestDiagnoseRescue_CrampedGeometry(t *testing.T) {
	panes := []PaneEntry{
		{PaneID: "wK:p1", TabID: "wK:t1", AgentStatus: "working", Name: "task-a", Cwd: "/wt/a"},
		{PaneID: "wK:p2", TabID: "wK:t1", AgentStatus: "unknown"},
	}
	layouts := map[string]PaneLayout{
		"wK:p1": {
			TabID: "wK:t1",
			Panes: []LayoutPane{
				{PaneID: "wK:p1", Rect: PaneRect{Width: 20, Height: 40}},
				{PaneID: "wK:p2", Rect: PaneRect{Width: 180, Height: 40}},
			},
			SplitCount: 1,
		},
	}
	rep := DiagnoseRescue(panes, nil, layouts)
	// Empty sibling close is preferred (kind rank 0) over cramped move.
	next, ok := SelectNextRescue(rep.Findings)
	if !ok || next.Kind != RescueEmptySibling {
		t.Fatalf("empty sibling should rank first, got ok=%v next=%+v findings=%+v", ok, next, rep.Findings)
	}
	var cramped bool
	for _, f := range rep.Findings {
		if f.Kind == RescueCramped && f.PaneID == "wK:p1" && f.Safe && f.Action == RescueActionMove {
			cramped = true
		}
	}
	if !cramped {
		t.Fatalf("expected safe cramped move finding, got %+v", rep.Findings)
	}
}

func TestDiagnoseRescue_IdempotentWhenAlreadyOneAgentPerTab(t *testing.T) {
	// After rescue: two tabs, one agent each + no unknowns.
	panes := []PaneEntry{
		{PaneID: "wK:p1", TabID: "wK:t1", AgentStatus: "working", Name: "task-a", Cwd: "/wt/a"},
		{PaneID: "wK:p2", TabID: "wK:t2", AgentStatus: "idle", Name: "task-b", Cwd: "/wt/b"},
	}
	rep := DiagnoseRescue(panes, nil, nil)
	if len(rep.Findings) != 0 || rep.Next != nil {
		t.Fatalf("already healthy inventory must be idempotent empty, got %+v", rep)
	}
}

func TestSelectNextRescue_OneAtATime(t *testing.T) {
	findings := []RescueFinding{
		{Kind: RescueEmptySibling, Action: RescueActionClose, PaneID: "pB", TabID: "t1", Safe: true},
		{Kind: RescueEmptySibling, Action: RescueActionClose, PaneID: "pC", TabID: "t1", Safe: true},
		{Kind: RescueMultiAgent, Action: RescueActionMove, PaneID: "pD", TabID: "t2", Safe: true, BeforeCwd: "/x"},
	}
	sortRescueFindings(findings)
	first, ok := SelectNextRescue(findings)
	if !ok {
		t.Fatal("expected first safe finding")
	}
	// Only one is returned; apply loop must re-diagnose for the next.
	second, ok2 := SelectNextRescue(findings[1:])
	if !ok2 {
		t.Fatal("expected second")
	}
	if first.PaneID == second.PaneID {
		t.Fatalf("SelectNext must advance: %q %q", first.PaneID, second.PaneID)
	}
}

func TestApplyRescue_RefusesUnsafe(t *testing.T) {
	f := RescueFinding{
		Kind:         RescueMultiAgent,
		Action:       RescueActionNone,
		PaneID:       "wK:p1",
		Safe:         false,
		RefuseReason: "focused pane is never changed",
	}
	_, err := ApplyRescue(f, RescueOptions{})
	if !errors.Is(err, ErrRescueUnsafe) {
		t.Fatalf("err=%v, want ErrRescueUnsafe", err)
	}
}

func TestApplyRescue_CloseEmptySibling_RecordsKeepAndIdempotent(t *testing.T) {
	// Live inventory: agent + blank sibling. First apply closes sibling;
	// second diagnose is empty.
	closed := map[string]bool{}
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "list" {
			var panes []string
			if !closed["wF:pB"] {
				panes = append(panes,
					`{"pane_id":"wF:pA","tab_id":"wF:t9","agent_status":"idle","name":"forge-worker","cwd":"/wt","foreground_cwd":"/wt"}`,
					`{"pane_id":"wF:pB","tab_id":"wF:t9","agent_status":"unknown"}`,
				)
			} else {
				panes = append(panes,
					`{"pane_id":"wF:pA","tab_id":"wF:t9","agent_status":"idle","name":"forge-worker","cwd":"/wt","foreground_cwd":"/wt"}`,
				)
			}
			return fmt.Sprintf(`{"result":{"panes":[%s]}}`, strings.Join(panes, ",")), nil
		}
		if len(args) >= 3 && args[0] == "pane" && args[1] == "close" {
			closed[args[2]] = true
			return `{"result":{}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "layout" {
			return `{"result":{"layout":{"tab_id":"wF:t9","panes":[{"pane_id":"wF:pA","rect":{"width":100,"height":40}},{"pane_id":"wF:pB","rect":{"width":100,"height":40}}],"splits":[{}]}}}`, nil
		}
		t.Fatalf("unexpected herdr args: %v", args)
		return "", fmt.Errorf("unexpected")
	}

	rep, err := SnapshotRescue()
	if err != nil {
		t.Fatal(err)
	}
	next, ok := SelectNextRescue(rep.Findings)
	if !ok {
		t.Fatalf("expected finding, got %+v", rep.Findings)
	}
	got, err := ApplyRescue(next, RescueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !closed["wF:pB"] {
		t.Fatal("pane close was not issued")
	}
	if got.AfterCwd != "/wt" {
		t.Fatalf("after cwd for keep agent=%q", got.AfterCwd)
	}

	// Idempotent: re-snapshot has no safe target.
	rep2, err := SnapshotRescue()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := SelectNextRescue(rep2.Findings); ok {
		t.Fatalf("second diagnose must be empty, got %+v", rep2.Findings)
	}
	// Applying the stale finding must fail closed (pane gone).
	_, err = ApplyRescue(next, RescueOptions{})
	if !errors.Is(err, ErrRescueNoTarget) {
		t.Fatalf("stale apply err=%v, want ErrRescueNoTarget", err)
	}
}

func TestApplyRescue_MoveRecordsBeforeAfterCwd(t *testing.T) {
	moved := false
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "list" {
			if !moved {
				return `{"result":{"panes":[
					{"pane_id":"wK:p1","tab_id":"wK:t1","agent_status":"working","name":"task-a","cwd":"/wt/a","foreground_cwd":"/wt/a"},
					{"pane_id":"wK:p2","tab_id":"wK:t1","agent_status":"done","name":"task-b","cwd":"/wt/b","foreground_cwd":"/wt/b"}
				]}}`, nil
			}
			// After move: p2 on its own tab.
			return `{"result":{"panes":[
				{"pane_id":"wK:p1","tab_id":"wK:t1","agent_status":"working","name":"task-a","cwd":"/wt/a","foreground_cwd":"/wt/a"},
				{"pane_id":"wK:p2","tab_id":"wK:t9","agent_status":"done","name":"task-b","cwd":"/wt/b","foreground_cwd":"/wt/b"}
			]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			// paneProcesses parses process_info envelope.
			return `{"result":{"process_info":{"foreground_processes":[{"pid":1,"name":"grok","cwd":"/wt/b","argv":["grok"]}]}}}`, nil
		}
		if len(args) >= 3 && args[0] == "pane" && args[1] == "move" {
			moved = true
			return `{"result":{}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		// layout probes for multi-pane tabs during SnapshotRescue
		if len(args) >= 2 && args[0] == "pane" && args[1] == "layout" {
			return `{"result":{"layout":{"tab_id":"wK:t1","panes":[{"pane_id":"wK:p1","rect":{"width":100,"height":40}},{"pane_id":"wK:p2","rect":{"width":100,"height":40}}],"splits":[{}]}}}`, nil
		}
		t.Fatalf("unexpected herdr args: %v", args)
		return "", fmt.Errorf("unexpected")
	}

	rep, err := SnapshotRescue()
	if err != nil {
		t.Fatal(err)
	}
	next, ok := SelectNextRescue(rep.Findings)
	if !ok || next.Action != RescueActionMove {
		t.Fatalf("expected move, got ok=%v next=%+v findings=%+v", ok, next, rep.Findings)
	}
	got, err := ApplyRescue(next, RescueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !moved {
		t.Fatal("pane move not issued")
	}
	if got.BeforeCwd != "/wt/b" {
		t.Fatalf("before cwd=%q", got.BeforeCwd)
	}
	if got.AfterCwd != "/wt/b" {
		t.Fatalf("after cwd=%q (task ownership/cwd must be preserved)", got.AfterCwd)
	}
	if got.TabID != "wK:t9" {
		t.Fatalf("after tab=%q, want new tab wK:t9", got.TabID)
	}
}

func TestApplyRescue_MoveFailureLeavesOriginalRecoverable(t *testing.T) {
	moveCalled := false
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "pane" && args[1] == "list" {
			return `{"result":{"panes":[
				{"pane_id":"wK:p1","tab_id":"wK:t1","agent_status":"working","name":"task-a","cwd":"/wt/a","foreground_cwd":"/wt/a"},
				{"pane_id":"wK:p2","tab_id":"wK:t1","agent_status":"done","name":"task-b","cwd":"/wt/b","foreground_cwd":"/wt/b"}
			]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"foreground_processes":[{"pid":1,"name":"grok","cwd":"/wt/b"}]}}}`, nil
		}
		if len(args) >= 3 && args[0] == "pane" && args[1] == "move" {
			moveCalled = true
			return "", fmt.Errorf("simulated move failure")
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "layout" {
			return `{"result":{"layout":{"tab_id":"wK:t1","panes":[],"splits":[]}}}`, nil
		}
		return `{}`, nil
	}

	rep, err := SnapshotRescue()
	if err != nil {
		t.Fatal(err)
	}
	next, ok := SelectNextRescue(rep.Findings)
	if !ok {
		t.Fatal("expected finding")
	}
	_, err = ApplyRescue(next, RescueOptions{})
	if err == nil {
		t.Fatal("expected move failure")
	}
	if !moveCalled {
		t.Fatal("move should have been attempted")
	}
	// Original inventory still intact (list still returns both panes on same tab).
	panes, err := PaneList()
	if err != nil || len(panes) != 2 {
		t.Fatalf("original session must remain listable: panes=%v err=%v", panes, err)
	}
	for _, p := range panes {
		if p.TabID != "wK:t1" {
			t.Fatalf("failure must not re-home panes, got %+v", p)
		}
	}
}

func TestFilterEmptySiblingFindings(t *testing.T) {
	in := []RescueFinding{
		{Kind: RescueEmptySibling, PaneID: "a"},
		{Kind: RescueMultiAgent, PaneID: "b"},
		{Kind: RescueEmptySibling, PaneID: "c"},
	}
	got := FilterEmptySiblingFindings(in)
	if len(got) != 2 || got[0].PaneID != "a" || got[1].PaneID != "c" {
		t.Fatalf("got=%+v", got)
	}
}

func TestDiagnoseRescue_WorkingAgentNotClosed(t *testing.T) {
	// Two working agents: move is ok, close is not.
	panes := []PaneEntry{
		{PaneID: "wK:p1", TabID: "wK:t1", AgentStatus: "working", Name: "a", Cwd: "/a"},
		{PaneID: "wK:p2", TabID: "wK:t1", AgentStatus: "working", Name: "b", Cwd: "/b"},
	}
	rep := DiagnoseRescue(panes, nil, nil)
	for _, f := range rep.Findings {
		if f.Action == RescueActionClose {
			t.Fatalf("must never close a working agent: %+v", f)
		}
	}
	next, ok := SelectNextRescue(rep.Findings)
	if !ok || next.Action != RescueActionMove {
		t.Fatalf("expected move, got ok=%v next=%+v", ok, next)
	}
}

// TestDiagnoseRescue_RegressionGuard: if someone "simplifies" policy to always
// emit a move for every pane, healthy and focused cases must still fail this
// test. Deliberately non-vacuous: empty findings are the pass condition.
func TestDiagnoseRescue_RegressionGuardHealthyAndFocused(t *testing.T) {
	healthy := DiagnoseRescue([]PaneEntry{
		{PaneID: "p1", TabID: "t1", AgentStatus: "working", Cwd: "/x"},
	}, nil, nil)
	if len(healthy.Findings) != 0 {
		t.Fatalf("REGRESSION: healthy pane mutated: %+v", healthy.Findings)
	}

	// Focused unknown sibling must not be closed.
	focusedUnknown := DiagnoseRescue([]PaneEntry{
		{PaneID: "p1", TabID: "t1", AgentStatus: "idle", Name: "a", Cwd: "/a"},
		{PaneID: "p2", TabID: "t1", AgentStatus: "unknown", Focused: true},
	}, nil, nil)
	for _, f := range focusedUnknown.Findings {
		if f.PaneID == "p2" && f.Safe {
			t.Fatalf("REGRESSION: focused unknown marked safe: %+v", f)
		}
	}
}
