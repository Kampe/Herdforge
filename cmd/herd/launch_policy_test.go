package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

func testLaunchRouter(t *testing.T) *router.SurfaceRouter {
	t.Helper()
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{
		CLIPresent: func(cli string) bool { return cli == launch.WorkerProvider },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	return r
}

func TestWorkerConfigDriftRejectsBeforeLaunch(t *testing.T) {
	lane := &config.LaneDef{Name: "mutant", Role: "worker", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation"}
	err := validateLaneLaunchConfig(lane)
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("drift must fail at worker policy boundary, got %v", err)
	}
}

type fakeLaunchLifecycle struct {
	providerList, claim, status, comment, worktree, tab, process, prompt int
	decision                                                             *router.LaunchDecision
}

func (r *fakeLaunchLifecycle) Run(decision *router.LaunchDecision, effect func(*router.LaunchDecision) error) error {
	r.decision = decision
	r.providerList++
	r.claim++
	r.status++
	r.comment++
	r.worktree++
	r.tab++
	r.process++
	r.prompt++
	return effect(decision)
}

func TestLaunchAdmissionRejectsBeforeCompiledLifecycleSeams(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "mutant", Role: "worker", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation"}}}
	rec := &fakeLaunchLifecycle{}
	valid, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "worker", Scope: router.ScopeLane, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = launchAdmissionWithLifecycle(rec, cfg, "worker", true, func(*config.LaneDef) (*router.LaunchDecision, error) { return valid, nil }, func(admitted *router.LaunchDecision) error {
		if admitted != valid {
			t.Fatalf("lifecycle received a different decision: got %p want %p", admitted, valid)
		}
		return nil
	})
	if *rec != (fakeLaunchLifecycle{}) {
		t.Fatalf("rejected launch invoked lifecycle seams: counters/decision=%+v", rec)
	}
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("config must reject before lifecycle seams: %v", err)
	}
}

func TestLaunchAdmissionPassesExactDecisionToLifecycle(t *testing.T) {
	lane := config.LaneDef{Name: "worker", Role: "worker", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"}
	cfg := &config.Config{Lanes: []config.LaneDef{lane}}
	valid, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "worker", Scope: router.ScopeLane, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeLaunchLifecycle{}
	got, err := launchAdmissionWithLifecycle(rec, cfg, lane.Role, true, func(*config.LaneDef) (*router.LaunchDecision, error) { return valid, nil }, func(admitted *router.LaunchDecision) error {
		if admitted != valid || admitted.Proof != valid.Proof {
			t.Fatalf("lifecycle did not receive exact admitted decision")
		}
		return nil
	})
	if err != nil || got != valid || rec.decision != valid {
		t.Fatalf("decision identity was not preserved: got=%p err=%v recorded=%p", got, err, rec.decision)
	}
}

func TestWorkerFinalTupleRejectsSparkFallbackBeforeAnySideEffect(t *testing.T) {
	roles := []string{launch.WorkerRole, launch.ForgeSmithRole, launch.RecoveryRole}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
			lane := config.LaneDef{Name: role, Role: role, AgentKind: launch.WorkerProvider, Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort, TaskShape: launch.Implementation}
			cfg := &config.Config{Lanes: []config.LaneDef{lane}}
			r := router.NewRouter(usage.NewQuotaEngine(), map[string]usage.BurnState{
				"codex": {
					Available: false,
					Reason:    "exhausted",
					Pools: map[string]usage.BurnState{
						"spark": {Available: true},
					},
				},
			})
			r.Probes = &router.Probes{CLIPresent: func(cli string) bool { return cli == launch.WorkerProvider }, Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
			rec := &fakeLaunchLifecycle{}
			decision, err := launchAdmissionWithLifecycle(rec, cfg, role, true, func(lane *config.LaneDef) (*router.LaunchDecision, error) {
				return r.Decide(router.LaunchRequest{
					Role:              router.Role(role),
					Shape:             launch.Implementation,
					RequestedProvider: launch.WorkerProvider,
					RequestedModel:    launch.WorkerModel,
					RequestedEffort:   launch.WorkerEffort,
					TaskRef:           lane.Name,
					Scope:             router.ScopeLane,
					ProbeResults:      map[string]bool{router.ProbeKey(launch.WorkerProvider, "gpt-5.3-codex-spark"): true},
				})
			}, func(*router.LaunchDecision) error {
				t.Fatal("forbidden Spark fallback reached lifecycle side effects")
				return nil
			})
			if decision != nil {
				t.Fatalf("rejected final tuple must not produce a LaunchDecision: %+v", decision)
			}
			if !errors.Is(err, router.ErrWorkerPolicy) {
				t.Fatalf("Spark fallback must fail with ErrWorkerPolicy: %v", err)
			}
			if *rec != (fakeLaunchLifecycle{}) {
				t.Fatalf("rejected final tuple caused side effects: %+v", rec)
			}
		})
	}
}

func TestTaskLaunchRequestCarriesExactReboundGeneration(t *testing.T) {
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel,
		RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-B", LeaseGeneration: 7,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := taskLaunchRequest(d, "FAC-B", "test-repository", "worker")
	if req.LeaseGeneration != 7 {
		t.Fatalf("task request generation = %d, want 7", req.LeaseGeneration)
	}
	if req.SessionGeneration != 0 {
		t.Fatalf("task request must defer standing session recovery, got generation %d", req.SessionGeneration)
	}
	if err := launch.Validate(req, nil); err != nil {
		t.Fatalf("exact task request must validate: %v", err)
	}
	for name, mutate := range map[string]func(*launch.Request){
		"zero":     func(r *launch.Request) { r.LeaseGeneration = 0 },
		"mismatch": func(r *launch.Request) { r.LeaseGeneration = 6 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := req
			mutate(&bad)
			if err := launch.Validate(bad, nil); err == nil {
				t.Fatal("zero or mismatched generation must fail before lifecycle seams")
			}
		})
	}
}

func TestStandingIdentityMismatchCreatesEphemeralTaskAgent(t *testing.T) {
	if !shouldCreateEphemeralTaskAgent(herdr.ErrAgentIdentityMismatch) {
		t.Fatal("standing identity mismatch must be treated as non-reusable")
	}
	if !shouldCreateEphemeralTaskAgent(herdr.ErrAgentNotFound) {
		t.Fatal("missing standing agent must create ephemeral task agent")
	}
	if shouldCreateEphemeralTaskAgent(errors.New("herdr unavailable")) {
		t.Fatal("unrelated standing failure must remain fail-closed")
	}
}

func TestPrepareStandingWorktreePropagatesFailure(t *testing.T) {
	lane := &config.LaneDef{Name: "standing", Worktree: "missing-standing-worktree"}
	want := errors.New("worktree add failed")
	err := prepareStandingWorktreeWith(lane, func(string, string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("worktree failure must block lifecycle continuation: got %v", err)
	}
}

func TestStandingEntryPointReturnsPolicyFailureBeforeHerdr(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "bad-standing", Role: "worker", Standing: true, AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation"}}}
	err := runStandingConfig(cfg, true)
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("standing entrypoint must return worker policy failure: %v", err)
	}
}

func TestForgeEntryPointReturnsPolicyFailureBeforeClaim(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "bad-forge", Role: "worker", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation"}}}
	claimed := false
	_, err := forgeLaunchAdmission(cfg, &cfg.Lanes[0], context.Background(), func(*router.LaunchDecision) error {
		claimed = true
		return nil
	})
	if claimed {
		t.Fatal("forbidden forge route reached claim effect")
	}
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("forge entrypoint must return worker policy failure: %v", err)
	}
}

func TestConfiguredRolePoliciesAreComplete(t *testing.T) {
	cases := []config.LaneDef{
		{Name: "worker", Role: "worker", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "forge", Role: "forge-smith", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "recovery-worker", Role: "recovery", AgentKind: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "reviewer", Role: "reviewer", AgentKind: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "qa"},
		{Name: "orchestrator", Role: "orchestrator", AgentKind: "claude", Provider: "claude", Model: "claude-opus-5", Effort: "medium", TaskShape: "coordinator"},
		{Name: "scout-planner", Role: "scout-planner", AgentKind: "claude", Provider: "claude", Model: "claude-opus-5", Effort: "medium", TaskShape: "architecture"},
		{Name: "verification", Role: "verification-gate", AgentKind: "opencode", Provider: "opencode", Model: "opencode/kimi-k3", Effort: "medium", TaskShape: "bounded"},
		{Name: "supervisor", Role: "review-supervisor", AgentKind: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "coordinator"},
		{Name: "harvest", Role: "harvest", AgentKind: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "bounded"},
		{Name: "sentinel", Role: "recovery-sentinel", AgentKind: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "bounded"},
	}
	for _, lane := range cases {
		if err := validateLaneLaunchConfig(&lane); err != nil {
			t.Errorf("%s: %v", lane.Role, err)
		}
	}
}
