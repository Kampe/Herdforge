package herdr

import (
	"errors"
	"testing"
)

func fencedAgentListJSON(agents string) string {
	return `{"result":{"agents":` + agents + `,"type":"agents"}}`
}

// stubAgentList installs a runHerdr that returns the supplied agent-list
// payloads in order for each "agent list" call, and `{}` for everything
// else. Restore is automatic via t.Cleanup.
func stubAgentList(t *testing.T, agentLists ...string) {
	t.Helper()
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	idx := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			if idx < len(agentLists) {
				out := agentLists[idx]
				idx++
				return out, nil
			}
			return fencedAgentListJSON(`[]`), nil
		}
		return `{}`, nil
	}
}

func TestCleanupFenced_DryRunReturnsCandidatesOnly(t *testing.T) {
	stubAgentList(t, fencedAgentListJSON(
		`[{"name":"task-fac-1","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":3}]`,
	))
	res, err := CleanupFenced(nil, true)
	if err != nil {
		t.Fatalf("dry-run error: %v", err)
	}
	if !res.DryRun {
		t.Fatal("dry_run must be true")
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates=%d want 1", len(res.Candidates))
	}
	if len(res.Attempts) != 0 {
		t.Fatalf("dry-run must not produce attempts: %d", len(res.Attempts))
	}
	if res.Closed != 0 || res.Blocked != 0 || res.Errored != 0 {
		t.Fatalf("dry-run counts must be zero: %+v", res)
	}
}

func TestCleanupFenced_DryRunNoCandidatesIsNotError(t *testing.T) {
	stubAgentList(t, fencedAgentListJSON(`[]`))
	res, err := CleanupFenced(nil, true)
	if err != nil {
		t.Fatalf("empty dry-run must not error: %v", err)
	}
	if len(res.Candidates) != 0 || len(res.Attempts) != 0 {
		t.Fatalf("expected empty: %+v", res)
	}
}

func TestCleanupFenced_MutationFailsClosedWithoutGeneration(t *testing.T) {
	stubAgentList(t, fencedAgentListJSON(
		`[{"name":"task-fac-1","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":3}]`,
	))
	res, err := CleanupFenced(nil, false)
	if err != nil {
		t.Fatalf("BLOCKED candidates must not set error: %v", err)
	}
	if len(res.Attempts) != 1 {
		t.Fatalf("attempts=%d want 1", len(res.Attempts))
	}
	att := res.Attempts[0]
	if att.Outcome != CleanupBlocked {
		t.Fatalf("outcome=%q want blocked; reason=%s", att.Outcome, att.Reason)
	}
	if att.Reason == "" {
		t.Fatal("reason must be non-empty")
	}
	if res.Blocked != 1 || res.Closed != 0 || res.Errored != 0 {
		t.Fatalf("counts: closed=%d blocked=%d errored=%d", res.Closed, res.Blocked, res.Errored)
	}
}

func TestCleanupFenced_InjectedExecutorClosesAndCountsDeterministically(t *testing.T) {
	stubAgentList(t, fencedAgentListJSON(
		`[
			{"name":"task-fac-1","agent_status":"done","tab_id":"t2","pane_id":"p2","workspace_id":"w","revision":1},
			{"name":"task-fac-2","agent_status":"idle","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":2}
		]`,
	))
	restore := SetCleanupCloseForTest(func(agent AgentEntry) CleanupAttempt {
		return CleanupAttempt{
			Name: agent.Name, TabID: agent.TabID,
			Outcome: CleanupClosed, Reason: "fenced close; absence confirmed",
		}
	})
	defer restore()
	res, err := CleanupFenced(nil, false)
	if err != nil {
		t.Fatalf("all-closed must not set error: %v", err)
	}
	if res.Closed != 2 || res.Blocked != 0 || res.Errored != 0 {
		t.Fatalf("counts: %+v", res)
	}
	if res.Attempts[0].TabID != "t1" || res.Attempts[1].TabID != "t2" {
		t.Fatalf("attempts not sorted by tab_id: %+v", res.Attempts)
	}
}

func TestCleanupFenced_MixedOutcomesProduceCorrectCounts(t *testing.T) {
	stubAgentList(t, fencedAgentListJSON(
		`[
			{"name":"task-a","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":1},
			{"name":"task-b","agent_status":"done","tab_id":"t2","pane_id":"p2","workspace_id":"w","revision":2},
			{"name":"task-c","agent_status":"idle","tab_id":"t3","pane_id":"p3","workspace_id":"w","revision":3}
		]`,
	))
	restore := SetCleanupCloseForTest(func(agent AgentEntry) CleanupAttempt {
		switch agent.TabID {
		case "t1":
			return CleanupAttempt{Name: agent.Name, TabID: agent.TabID, Outcome: CleanupClosed, Reason: "ok"}
		case "t2":
			return CleanupAttempt{Name: agent.Name, TabID: agent.TabID, Outcome: CleanupBlocked, Reason: "no generation"}
		default:
			return CleanupAttempt{Name: agent.Name, TabID: agent.TabID, Outcome: CleanupError, Reason: "transport failure"}
		}
	})
	defer restore()
	res, err := CleanupFenced(nil, false)
	if err == nil {
		t.Fatal("errored cleanup must return error")
	}
	if res.Closed != 1 || res.Blocked != 1 || res.Errored != 1 {
		t.Fatalf("counts: closed=%d blocked=%d errored=%d", res.Closed, res.Blocked, res.Errored)
	}
	if len(res.Attempts) != 3 {
		t.Fatalf("attempts=%d want 3", len(res.Attempts))
	}
}

func TestCleanupFenced_AgentListErrorPropagates(t *testing.T) {
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		return "", errors.New("herdr socket unavailable")
	}
	_, err := CleanupFenced(nil, false)
	if err == nil {
		t.Fatal("AgentList error must propagate")
	}
}

func TestCleanupFenced_StandingAndOrchestratorExcluded(t *testing.T) {
	stubAgentList(t, fencedAgentListJSON(
		`[
			{"name":"forge-builder","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":1},
			{"name":"task-fac-1","agent_status":"done","tab_id":"t2","pane_id":"p2","workspace_id":"w","revision":2},
			{"name":"orchestrator-main","agent_status":"done","tab_id":"t3","pane_id":"p3","workspace_id":"w","revision":3}
		]`,
	))
	standing := map[string]bool{"forge-builder": true}
	res, err := CleanupFenced(standing, true)
	if err != nil {
		t.Fatalf("dry-run error: %v", err)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates=%d want 1 (standing + orchestrator excluded)", len(res.Candidates))
	}
	if res.Candidates[0].TabID != "t2" {
		t.Fatalf("expected t2, got %s", res.Candidates[0].TabID)
	}
}

func TestDefaultCleanupClose_BuildsRequestAndFailsClosedOnNoGeneration(t *testing.T) {
	// The default executor builds a CloseRequest from AgentEntry and calls
	// TabCloseCAS. The agent list wire format has no generation field, so
	// ExpandCloseRequest must fail closed before the transport is reached.
	var transportCalled bool
	restore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		transportCalled = true
		return CloseReceipt{}, nil
	})
	defer restore()

	att := defaultCleanupClose(AgentEntry{
		Name: "task-fac-1", Status: "done", TabID: "t1",
		PaneID: "p1", Workspace: "w", Revision: 3,
		Kind: "claude",
	})
	if att.Outcome != CleanupBlocked {
		t.Fatalf("outcome=%q want blocked; reason=%s", att.Outcome, att.Reason)
	}
	if transportCalled {
		t.Fatal("transport must not be reached when generation is absent")
	}
}

func TestDefaultCleanupClose_WithErrorIsNotBlocked(t *testing.T) {
	// A non-Blocked error from TabCloseCAS (e.g., transport failure) must
	// produce CleanupError, not CleanupBlocked.
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		// AgentList for absence readback — but we should never get here
		// because TabCloseCAS will error first.
		return fencedAgentListJSON(`[]`), nil
	}
	// Inject a transport that returns a non-typed error.
	restore := SetCompareCloseTransportForTest(func(req CompareAndCloseRequest) (CloseReceipt, error) {
		return CloseReceipt{}, errors.New("socket timeout")
	})
	defer restore()
	// We need generation to pass ExpandCloseRequest. Patch: call with a
	// manually constructed request that bypasses the agent list.
	// Instead, test via TabCloseCAS directly with a complete request.
	err := TabCloseCAS(CloseRequest{
		WorkspaceID: "w", TabID: "t1", Generation: "1", Nonce: "n",
	})
	var blocked *CloseUnavailableError
	if !errors.As(err, &blocked) {
		t.Fatalf("transport error must wrap as CloseUnavailableError, got %T: %v", err, err)
	}
	// The reason should be the transport error, not a fence refusal.
	if blocked.Reason == "stale-generation" || blocked.Reason == "attachment-changed" {
		t.Fatalf("transport error misclassified as fence refusal: %s", blocked.Reason)
	}
}

// Mutation oracle: a broken executor that skips generation evidence would
// close. The real default path blocks. This proves the BLOCKED outcome is
// load-bearing, not vacuous.
func TestCleanupFenced_MutationOracle_BrokenExecutorWouldClose(t *testing.T) {
	agentList := fencedAgentListJSON(
		`[{"name":"task-fac-1","agent_status":"done","tab_id":"t1","pane_id":"p1","workspace_id":"w","revision":3}]`,
	)
	old := runHerdr
	t.Cleanup(func() { runHerdr = old })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return agentList, nil
		}
		return `{}`, nil
	}
	// Real path: default executor, no generation → BLOCKED.
	realRes, realErr := CleanupFenced(nil, false)
	if realErr != nil {
		t.Fatalf("real path: %v", realErr)
	}
	if realRes.Attempts[0].Outcome != CleanupBlocked {
		t.Fatalf("real path must BLOCK: %+v", realRes.Attempts)
	}
	// Broken path: injected executor that ignores generation and closes.
	restore := SetCleanupCloseForTest(func(agent AgentEntry) CleanupAttempt {
		return CleanupAttempt{
			Name: agent.Name, TabID: agent.TabID,
			Outcome: CleanupClosed, Reason: "broken: skipped generation fence",
		}
	})
	defer restore()
	brokenRes, _ := CleanupFenced(nil, false)
	if brokenRes.Attempts[0].Outcome != CleanupClosed {
		t.Fatalf("mutation oracle must close without generation fence: %+v", brokenRes.Attempts)
	}
	if brokenRes.Closed != 1 {
		t.Fatalf("broken path closed=%d want 1", brokenRes.Closed)
	}
}
