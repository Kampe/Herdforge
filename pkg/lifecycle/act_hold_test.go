package lifecycle

import (
	"context"
	"encoding/json"
	"testing"
)

type actHoldReader struct {
	identities []HoldIdentity
	holdLane   bool
	taskHeld   *bool
}

func (r *actHoldReader) CurrentGeneration(context.Context, HoldIdentity) (int64, error) {
	return 1, nil
}

func (r *actHoldReader) Check(_ context.Context, identity HoldIdentity, generation int64) (HoldDecision, error) {
	if generation != 1 {
		return HoldDecision{}, ErrHoldAuthorityUnavailable
	}
	r.identities = append(r.identities, identity)
	taskHeld := true
	if r.taskHeld != nil {
		taskHeld = *r.taskHeld
	}
	return HoldDecision{Held: identity.Scope == "lane" && r.holdLane || identity.Scope == "task" && taskHeld, Reason: "maintenance", Code: "operator_hold"}, nil
}

func actHoldEngine(reader *actHoldReader) *Engine {
	return &Engine{
		HoldReader: reader,
		HoldRoles:  []string{"worker", "forge-smith"},
		HoldLaneResolver: func(role string) (string, error) {
			if role == "worker" {
				return "smith", nil
			}
			if role == "forge-smith" {
				return "scout", nil
			}
			return "", ErrActiveTaskUnknown
		},
		HoldLiveAgentResolver: func(agent string) (string, string, error) {
			if agent == "forge-smith" {
				return "worker", "smith", nil
			}
			return "", "", ErrActiveTaskUnknown
		},
		HoldIdentity: func(task, lane, owner string) HoldIdentity {
			scope := "lane"
			if task != "" {
				scope = "task"
			}
			return HoldIdentity{Repository: "repo", Owner: owner, Lane: lane, Task: task, Scope: scope}
		},
	}
}

func TestActModeStaleReclaimUsesConfiguredRoleLaneBeforeCommand(t *testing.T) {
	reader := &actHoldReader{holdLane: false}
	e := actHoldEngine(reader)
	s := &Summary{StaleInProgress: 1, StaleCards: []StaleCard{{Ref: "FAC-1", Role: "worker", Owner: "worker", Lane: "wrong"}}}
	if err := e.executeActMode(t.TempDir(), t.TempDir(), s, nil, nil, nil, nil); err != nil {
		t.Fatalf("held stale reclaim should skip independently: %v", err)
	}
	if len(reader.identities) != 2 || reader.identities[0].Lane != "smith" || reader.identities[0].Owner != "worker" || reader.identities[0].Task != "" || reader.identities[1].Task != "FAC-1" {
		t.Fatalf("stale reclaim used noncanonical identities: %+v", reader.identities)
	}
}

func TestActModeSettledKickUsesTypedLiveAgentRoleLaneBeforeCommand(t *testing.T) {
	reader := &actHoldReader{holdLane: true}
	e := actHoldEngine(reader)
	s := &Summary{Dispatchable: 1, Settled: []AgentSnapshot{{Name: "forge-smith"}}}
	if err := e.executeActMode(t.TempDir(), t.TempDir(), s, nil, nil, nil, nil); err == nil {
		t.Fatal("held settled lane unexpectedly reached kick command")
	}
	if len(reader.identities) != 1 || reader.identities[0].Lane != "smith" || reader.identities[0].Owner != "worker" || reader.identities[0].Scope != "lane" {
		t.Fatalf("settled kick used noncanonical identity: %+v", reader.identities)
	}
}

func TestConfiguredLiveForgeSmithSettledKickUsesSmithWorkerHold(t *testing.T) {
	taskHeld := false
	reader := &actHoldReader{holdLane: false, taskHeld: &taskHeld}
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{
		{Name: "smith", Role: "worker"},
		{Name: "scout", Role: "forge-smith"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := actHoldEngine(reader)
	e.StandingRoster = &registry
	e.HoldLiveAgentResolver = func(id string) (string, string, error) {
		lane, err := registry.ResolveLiveAgentID(id)
		if err != nil {
			return "", "", err
		}
		return lane.Role, lane.Name, nil
	}
	agents := struct {
		Result struct {
			Agents []json.RawMessage `json:"agents"`
		} `json:"result"`
	}{}
	agents.Result.Agents = []json.RawMessage{json.RawMessage(`{"name":"forge-smith","status":"idle","interactive":true}`)}
	board := json.RawMessage(`{"tasks":[{"ref":"FAC-202","status":"to-do","labels":["worker"]}]}`)
	s := e.computeSummary(agents, board, nil, nil)
	if len(s.Settled) != 1 || s.Settled[0].Lane != "smith" || s.Settled[0].Role != "worker" {
		t.Fatalf("live forge-smith was not resolved as smith/worker: %+v", s.Settled)
	}
	if s.Dispatchable != 1 {
		t.Fatalf("expected dispatchable task before hold, summary=%+v", s)
	}
	reader.holdLane = true
	taskHeld = true
	if err := e.executeActMode(t.TempDir(), t.TempDir(), s, nil, nil, nil, nil); err == nil {
		t.Fatal("held smith/worker live agent unexpectedly reached kick")
	}
	if len(reader.identities) == 0 || reader.identities[0].Owner != "worker" || reader.identities[0].Lane != "smith" {
		t.Fatalf("settled kick did not check canonical smith/worker identity: %+v", reader.identities)
	}
}

func TestUnknownLiveIdentityIsCriticalAndNotSettled(t *testing.T) {
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{{Name: "smith", Role: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{StandingRoster: &registry}
	agents := struct {
		Result struct {
			Agents []json.RawMessage `json:"agents"`
		} `json:"result"`
	}{}
	agents.Result.Agents = []json.RawMessage{json.RawMessage(`{"name":"legacy-worker","status":"idle"}`)}
	s := e.computeSummary(agents, json.RawMessage(`{"tasks":[]}`), nil, nil)
	e.computeRedCodes(s)
	if len(s.Settled) != 0 || len(s.Critical) != 1 || s.Healthy {
		t.Fatalf("unknown live identity was not fail-closed: %+v", s)
	}
}

func TestActModeBlockedRoutingUsesCanonicalRoleLaneBeforeCommand(t *testing.T) {
	reader := &actHoldReader{holdLane: false}
	e := actHoldEngine(reader)
	s := &Summary{Todo: 1, Blocked: 1, BlockedRefs: []string{"FAC-1"}, BlockedTargets: []HoldTarget{{Repository: "repo", Owner: "worker", Lane: "smith", Task: "FAC-1", Scope: "task"}}}
	if err := e.executeActMode(t.TempDir(), t.TempDir(), s, nil, nil, nil, nil); err != nil {
		t.Fatalf("held blocked target should skip routing: %v", err)
	}
	if len(reader.identities) != 2 || reader.identities[0].Lane != "smith" || reader.identities[1].Task != "FAC-1" {
		t.Fatalf("blocked routing used noncanonical identities: %+v", reader.identities)
	}
}

func TestActModeBlockedRoutingPreservesForgeSmithRoleScoutLane(t *testing.T) {
	reader := &actHoldReader{holdLane: false}
	e := actHoldEngine(reader)
	s := &Summary{Todo: 1, Blocked: 1, BlockedRefs: []string{"FAC-2"}, BlockedTargets: []HoldTarget{{Repository: "repo", Owner: "forge-smith", Lane: "scout", Task: "FAC-2", Scope: "task"}}}
	if err := e.executeActMode(t.TempDir(), t.TempDir(), s, nil, nil, nil, nil); err != nil {
		t.Fatalf("held forge-smith/scout target should skip routing: %v", err)
	}
	if len(reader.identities) != 2 || reader.identities[0].Owner != "forge-smith" || reader.identities[0].Lane != "scout" || reader.identities[1].Task != "FAC-2" {
		t.Fatalf("forge-smith/scout identity was altered: %+v", reader.identities)
	}
}
