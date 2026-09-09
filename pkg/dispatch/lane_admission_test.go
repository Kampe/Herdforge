package dispatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
)

type countingOwner struct {
	claims atomic.Int32
}

func (o *countingOwner) ClaimExclusive(context.Context, deps.TaskID, deps.Ref, string, string, string, string) (*deps.OwnershipToken, error) {
	o.claims.Add(1)
	return &deps.OwnershipToken{OwnerID: "count", Generation: 1}, nil
}
func (o *countingOwner) ClaimExclusiveNamedLane(context.Context, deps.TaskID, deps.Ref, string, string, string, string, string) (*deps.OwnershipToken, error) {
	o.claims.Add(1)
	return &deps.OwnershipToken{OwnerID: "count", Generation: 1}, nil
}
func (o *countingOwner) StillOwns(context.Context, *deps.OwnershipToken) (bool, error) {
	return true, nil
}
func (o *countingOwner) ReleaseIfOwner(context.Context, *deps.OwnershipToken, string) error {
	return nil
}
func (o *countingOwner) Close() error { return nil }

// mismatchedDecision mints a REAL router-issued codex decision. smith-grok is
// configured grok/grok-4.6; a codex decision under that lane is the live
// FAC-608 defect (pane wK:pTM launched codex --model gpt-5.6-luna under
// lane smith-grok). The decision is proof-carrying so the only gate that can
// refuse it is the lane identity/argv fence this test exists for.
func mismatchedDecision(t *testing.T) *router.LaunchDecision {
	t.Helper()
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", t.TempDir())
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{
		CLIPresent: func(cli string) bool { return cli == router.PiHarness || cli == testWorkerProvider },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	d, err := r.Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
	})
	if err != nil {
		t.Fatalf("mint codex launch fixture: %v", err)
	}
	return d
}

// FAC-703 RED: `herd dispatch --lane smith-grok` must never launch smith's
// codex argv. A codex decision under the grok lane must be refused BEFORE the
// ownership claim (and long before any pane is created).
func TestDispatchRefusesLaneArgvMismatchBeforeClaim(t *testing.T) {
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("HERD_MODE", "local")
	cfg := &config.Config{
		Project:      config.ProjectConfig{Name: "Herdforge", DefaultBranch: "main"},
		TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
		Lanes: []config.LaneDef{
			{Name: "smith", Role: "worker", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: launch.Implementation},
			{Name: "smith-grok", Role: "worker", AgentKind: "grok", Harness: "grok", Provider: "grok", Model: "grok-4.6", Effort: "medium", TaskShape: launch.Implementation},
		},
		Verification: config.Verification{TestCommand: "go test ./..."},
	}
	owner := &countingOwner{}
	_, wm := initDispatchRepo(t)
	d := NewDispatcher(cfg, &mockTaskProvider{tasks: []*provider.Task{baseTask("FAC-703")}}, wm)
	d.Ownership = owner
	d.Compensator = &recordingCompensator{}
	// A fake launcher that reports success: on the unfixed base the codex
	// argv then launches to completion under smith-grok, which is exactly the
	// silent wrong-pane defect. Post-fix the dispatch is refused before the
	// claim and this launcher is never reached.
	d.Herdr = &fakeHerdr{available: true, workspace: "wHerd", model: testWorkerModel, tabID: "tab-703"}
	_, err := d.Dispatch(context.Background(), DispatchOptions{
		TicketRef: "FAC-703", LaneName: "smith-grok", Decision: mismatchedDecision(t), LeaseID: "claim:1", LeaseGeneration: 1,
	})
	if err == nil {
		t.Fatal("codex argv under smith-grok must be refused")
	}
	if owner.claims.Load() != 0 {
		t.Fatalf("lane/argv mismatch claimed a lease: claims=%d err=%v", owner.claims.Load(), err)
	}
}

func TestValidateDecisionForLaneRefusesProviderAndArgvDrift(t *testing.T) {
	lane := &config.LaneDef{Name: "smith-claude", Role: "worker", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium"}
	decision := &router.LaunchDecision{LaneName: lane.Name, Provider: "codex", Model: "gpt-5.6-luna", Harness: "codex", Effort: "medium", HarnessArgv: router.ArgvFor("codex", "gpt-5.6-luna", "medium")}
	if err := validateDecisionForLane(decision, lane); err == nil {
		t.Fatal("provider/model/harness drift must be refused before claim or pane creation")
	}
}

func TestValidateDecisionForLaneKeepsStandingReroute(t *testing.T) {
	lane := &config.LaneDef{Name: "standing", Role: "worker", Standing: true, AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium"}
	decision := &router.LaunchDecision{LaneName: lane.Name, Provider: "grok", Model: "grok-4.6", Harness: "grok", Effort: "medium"}
	if err := validateDecisionForLane(decision, lane); err != nil {
		t.Fatalf("standing quota reroute must remain admissible: %v", err)
	}
}

func TestValidateDecisionForLaneRefusesArgvFamilyDrift(t *testing.T) {
	lane := &config.LaneDef{Name: "smith-grok", Role: "worker", AgentKind: "grok", Harness: "grok", Provider: "grok", Model: "grok-4.6", Effort: "medium"}
	decision := &router.LaunchDecision{
		LaneName: lane.Name, Provider: "grok", Model: "grok-4.6", Family: "openai",
		Harness: "grok", Effort: "medium", HarnessArgv: router.ArgvFor("grok", "grok-4.6", "medium"),
	}
	if err := validateDecisionForLane(decision, lane); err == nil {
		t.Fatal("family/argv drift must be refused before claim or pane creation")
	}
}

func TestValidateDecisionForLaneBindsSmithGrokArgv(t *testing.T) {
	lane := &config.LaneDef{Name: "smith-grok", Role: "worker", AgentKind: "grok", Harness: "grok", Provider: "grok", Model: "grok-4.6", Effort: "medium"}
	argv := router.ArgvFor("grok", "grok-4.6", "medium")
	decision := &router.LaunchDecision{
		LaneName: lane.Name, Provider: "grok", Model: "grok-4.6", Family: "xai",
		Harness: "grok", Effort: "medium", HarnessArgv: argv, Argv: argv,
	}
	if err := validateDecisionForLane(decision, lane); err != nil {
		t.Fatalf("exact smith-grok argv must admit: %v", err)
	}
}
