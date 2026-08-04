package lifecycle

import (
	"context"
	"testing"
)

type actHoldReader struct {
	identities []HoldIdentity
	holdLane   bool
}

func (r *actHoldReader) CurrentGeneration(context.Context, HoldIdentity) (int64, error) {
	return 1, nil
}

func (r *actHoldReader) Check(_ context.Context, identity HoldIdentity, generation int64) (HoldDecision, error) {
	if generation != 1 {
		return HoldDecision{}, ErrHoldAuthorityUnavailable
	}
	r.identities = append(r.identities, identity)
	return HoldDecision{Held: identity.Scope == "lane" && r.holdLane || identity.Scope == "task", Reason: "maintenance", Code: "operator_hold"}, nil
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
