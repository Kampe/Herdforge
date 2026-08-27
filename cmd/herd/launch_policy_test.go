package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// Authorized fixed builder tuple for launch-policy tests.
const (
	testWorkerProvider = "codex"
	testWorkerModel    = "gpt-5.6-luna"
	testWorkerEffort   = "medium"
)

func testLaunchRouter(t *testing.T) *router.SurfaceRouter {
	t.Helper()
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{
		CLIPresent: func(cli string) bool { return cli == router.PiHarness },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	return r
}

func TestBuilderWorkspaceUsesRegisteredBinding(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	configData := []byte("version: \"1\"\nproject:\n  name: Herdforge\ntask_provider:\n  type: kaneo\n  project_id: project\nfleet:\n  herdr_workspace: wK\n")
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), configData, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		return `{"result":{"workspaces":[{"workspace_id":"wF","label":"focused","focused":true},{"workspace_id":"wK","label":"other"}]}}`, nil
	})
	t.Cleanup(restore)
	t.Setenv("HERD_WORKSPACE", "")

	got, err := resolveBuilderWorkspace(root)
	if err != nil {
		t.Fatalf("resolveBuilderWorkspace: %v", err)
	}
	if got != "wK" {
		t.Fatalf("builder workspace = %q, want registered workspace wK instead of focused wF", got)
	}
}

func TestWorkerConfigDriftRejectsBeforeLaunch(t *testing.T) {
	lane := &config.LaneDef{Name: "mutant", Role: "worker", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation"}
	err := validateLaneLaunchConfig(lane)
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("drift must fail at worker policy boundary, got %v", err)
	}
}

// Assayer is a valid launch role (same task_shape as reviewer: "qa"). A lane
// carrying it with the wrong task_shape must be rejected — this test would
// have passed vacuously before assayer was added to the expectedShapes map,
// because the role was simply absent and every shape was rejected. Now it
// must fail on the shape mismatch, not on the role being unknown.
func TestAssayerRoleRejectsWrongTaskShape(t *testing.T) {
	lane := &config.LaneDef{Name: "ci-warden", Role: launch.AssayerRole, AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "implementation"}
	err := validateLaneLaunchConfig(lane)
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("assayer with wrong task_shape must fail at worker policy boundary, got %v", err)
	}
	if !strings.Contains(err.Error(), launch.AssayerRole) {
		t.Fatalf("error must name the role %q: %v", launch.AssayerRole, err)
	}
}

func TestConfiguredCustomRoleAcceptsKnownTaskShape(t *testing.T) {
	tests := []struct {
		name      string
		role      string
		shape     string
		wantError bool
	}{
		{name: "bounded", role: "docs-custodian", shape: "bounded"},
		{name: "qa", role: "api-crusader", shape: "qa"},
		{name: "unknown shape", role: "docs-custodian", shape: "not-a-task-shape", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lane := &config.LaneDef{Name: tt.role, Role: tt.role, AgentKind: "codex", Harness: "codex", Provider: "codex", Model: testWorkerModel, Effort: testWorkerEffort, TaskShape: tt.shape}
			err := validateLaneLaunchConfig(lane)
			if (err != nil) != tt.wantError {
				t.Fatalf("validateLaneLaunchConfig() error = %v, wantError %t", err, tt.wantError)
			}
		})
	}
}

func TestCustomStandingRoleUsesExplicitNativeWorkerPolicy(t *testing.T) {
	lane := &config.LaneDef{
		Name: "docs-custodian", Role: "docs-custodian", Standing: true,
		StandingRolePolicy: &config.StandingRolePolicy{NativeRole: launch.WorkerRole},
		AgentKind:          "codex", Harness: "codex", Provider: testWorkerProvider,
		Model: testWorkerModel, Effort: testWorkerEffort, TaskShape: "implementation",
	}
	if err := validateLaneLaunchConfig(lane); err != nil {
		t.Fatalf("custom standing role should validate through its explicit native policy: %v", err)
	}
	role, err := nativeLaunchRole(lane)
	if err != nil || role != router.RoleWorker {
		t.Fatalf("native role = %q, err = %v; want worker", role, err)
	}
}

func TestCustomStandingRoleRejectsMalformedNativeWorkerTuple(t *testing.T) {
	lane := &config.LaneDef{
		Name: "docs-custodian", Role: "docs-custodian", Standing: true,
		StandingRolePolicy: &config.StandingRolePolicy{NativeRole: launch.WorkerRole},
		AgentKind:          "codex", Harness: "codex", Provider: testWorkerProvider,
		Model: "gpt-5.6-sol", Effort: testWorkerEffort, TaskShape: "implementation",
	}
	err := validateLaneLaunchConfig(lane)
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("malformed native worker tuple must fail worker policy, got %v", err)
	}
}

func TestCustomStandingRoleRequiresExplicitPolicy(t *testing.T) {
	lane := &config.LaneDef{
		Name: "docs-custodian", Role: "docs-custodian", Standing: true,
		AgentKind: "codex", Harness: "codex", Provider: testWorkerProvider,
		Model: testWorkerModel, Effort: testWorkerEffort, TaskShape: "implementation",
	}
	err := validateLaneLaunchConfig(lane)
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("custom standing role without policy must fail closed, got %v", err)
	}
}

func TestCustomStandingRoleRoutesThroughNativeWorkerAuthority(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	pinHealthyQuota(t, dir, "codex")
	lane := &config.LaneDef{
		Name: "docs-custodian", Role: "docs-custodian", Standing: true,
		StandingRolePolicy: &config.StandingRolePolicy{NativeRole: launch.WorkerRole},
		AgentKind:          "codex", Harness: "codex", Provider: testWorkerProvider,
		Model: testWorkerModel, Effort: testWorkerEffort, TaskShape: "implementation",
	}
	decision, err := laneLaunchDecisionWithProbe(context.Background(), lane, nil, func(_ context.Context, _, model, _ string) herdr.ProbeResult {
		return herdr.ProbeResult{Model: model, Available: true}
	})
	if err != nil {
		t.Fatalf("custom standing route rejected: %v", err)
	}
	if decision.Role != router.RoleWorker || decision.Shape != launch.Implementation {
		t.Fatalf("native standing decision = role %q shape %q; want worker/implementation", decision.Role, decision.Shape)
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
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "mutant", Role: "worker", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation"}}}
	rec := &fakeLaunchLifecycle{}
	valid, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "worker", Scope: router.ScopeLane, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
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
	lane := config.LaneDef{Name: "worker", Role: "worker", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"}
	cfg := &config.Config{Lanes: []config.LaneDef{lane}}
	valid, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, TaskRef: "worker", Scope: router.ScopeLane, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
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

func TestLaunchAdmissionValidatesDecisionContextByScope(t *testing.T) {
	lane := config.LaneDef{Name: "reviewer", Role: "reviewer", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "qa"}
	cfg := &config.Config{Lanes: []config.LaneDef{lane}}
	newDecision := func(t *testing.T, taskRef string) *router.LaunchDecision {
		t.Helper()
		decision, err := testLaunchRouter(t).Decide(router.LaunchRequest{
			Role: router.RoleAssayer, Shape: "qa", RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium",
			AuthorFamily: "deepseek", AuthorModel: "deepseek-v4-pro", CandidateSHA: "sha-fac-153", TaskRef: taskRef, Scope: router.ScopeCandidate,
			ProbeResults: map[string]bool{router.ProbeKey("codex", "gpt-5.6-luna"): true},
		})
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}

	t.Run("candidate context reaches effect once", func(t *testing.T) {
		decision := newDecision(t, "FAC-153")
		effects := 0
		_, err := launchAdmissionWithLifecycle(&fakeLaunchLifecycle{}, cfg, lane.Role, true, func(*config.LaneDef) (*router.LaunchDecision, error) { return decision, nil }, func(*router.LaunchDecision) error {
			effects++
			return nil
		})
		if err != nil || effects != 1 {
			t.Fatalf("candidate launch effect count = %d, err = %v; want 1, nil", effects, err)
		}
	})

	t.Run("mismatched candidate context is rejected before effect", func(t *testing.T) {
		decision := newDecision(t, "FAC-153")
		decision.TaskRef = "FAC-154"
		effects := 0
		_, err := launchAdmissionWithLifecycle(&fakeLaunchLifecycle{}, cfg, lane.Role, true, func(*config.LaneDef) (*router.LaunchDecision, error) { return decision, nil }, func(*router.LaunchDecision) error {
			effects++
			return nil
		})
		if err == nil || effects != 0 {
			t.Fatalf("mismatched candidate launch effect count = %d, err = %v; want 0 and an error", effects, err)
		}
	})

	t.Run("lane context remains lane name", func(t *testing.T) {
		decision, err := testLaunchRouter(t).Decide(router.LaunchRequest{
			Role: router.RoleReviewer, Shape: "qa", RequestedProvider: "codex", RequestedModel: "gpt-5.6-luna", RequestedEffort: "medium", TaskRef: lane.Name, Scope: router.ScopeLane,
			AuthorFamily: "deepseek", AuthorModel: "deepseek-v4-pro", CandidateSHA: "sha-fac-153", ProbeResults: map[string]bool{router.ProbeKey("codex", "gpt-5.6-luna"): true},
		})
		if err != nil {
			t.Fatal(err)
		}
		effects := 0
		_, err = launchAdmissionWithLifecycle(&fakeLaunchLifecycle{}, cfg, lane.Role, true, func(*config.LaneDef) (*router.LaunchDecision, error) { return decision, nil }, func(*router.LaunchDecision) error {
			effects++
			return nil
		})
		if err != nil || effects != 1 {
			t.Fatalf("lane launch effect count = %d, err = %v; want 1, nil", effects, err)
		}
	})
}

// A builder whose only reachable model has no passing tool-probe must be
// rejected before ANY lifecycle side effect. (Previously this asserted a
// codex/luna vendor tuple; builders are no longer vendor-pinned, so the
// surviving invariant is the probe gate plus side-effect ordering.)
func TestWorkerUnprobedFallbackRejectsBeforeAnySideEffect(t *testing.T) {
	roles := []string{launch.WorkerRole, launch.ForgeSmithRole, launch.RecoveryRole}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
			lane := config.LaneDef{Name: role, Role: role, AgentKind: "codex", Harness: "codex", Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort, TaskShape: launch.Implementation}
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
			r.Probes = &router.Probes{CLIPresent: func(cli string) bool { return cli == router.PiHarness }, Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}
			rec := &fakeLaunchLifecycle{}
			decision, err := launchAdmissionWithLifecycle(rec, cfg, role, true, func(lane *config.LaneDef) (*router.LaunchDecision, error) {
				return r.Decide(router.LaunchRequest{
					Role:              router.Role(role),
					Shape:             launch.Implementation,
					RequestedProvider: testWorkerProvider,
					RequestedModel:    testWorkerModel,
					RequestedEffort:   testWorkerEffort,
					TaskRef:           lane.Name,
					Scope:             router.ScopeLane,
					// No passing probe for any reachable model: fail closed.
					ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, "gpt-5.3-codex-spark"): false},
				})
			}, func(*router.LaunchDecision) error {
				t.Fatal("rejected launch reached lifecycle side effects")
				return nil
			})
			if decision != nil {
				t.Fatalf("rejected launch must not produce a LaunchDecision: %+v", decision)
			}
			if err == nil {
				t.Fatal("no healthy builder surface must fail closed")
			}
			if *rec != (fakeLaunchLifecycle{}) {
				t.Fatalf("rejected launch caused side effects: %+v", rec)
			}
		})
	}
}

func TestTaskLaunchRequestCarriesExactReboundGeneration(t *testing.T) {
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel,
		RequestedEffort: testWorkerEffort, TaskRef: "FAC-B", LeaseGeneration: 7,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
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

func TestEphemeralTaskAuthorizationAllowsOnlyNotFound(t *testing.T) {
	if shouldCreateEphemeralTaskAgent(herdr.ErrAgentIdentityMismatch) {
		t.Fatal("standing identity mismatch must fail closed")
	}
	if !shouldCreateEphemeralTaskAgent(herdr.ErrAgentNotFound) {
		t.Fatal("missing standing agent must create ephemeral task agent")
	}
	if shouldCreateEphemeralTaskAgent(errors.New("herdr unavailable")) {
		t.Fatal("unrelated standing failure must remain fail-closed")
	}
}

func TestStandingIdentityMismatchCannotReachEphemeralSideEffects(t *testing.T) {
	tabCreate, agentStart, prompt := 0, 0, 0
	resolveErr := herdr.ErrAgentIdentityMismatch
	if gateErr := authorizeEphemeralTaskAgent(resolveErr); gateErr == nil {
		// This is the caller-level branch shared by pulse, review, and forge:
		// all side effects are downstream of the authorization result.
		tabCreate++
		agentStart++
		prompt++
	}
	if tabCreate != 0 || agentStart != 0 || prompt != 0 {
		t.Fatalf("identity mismatch reached ephemeral side effects: tab=%d start=%d prompt=%d", tabCreate, agentStart, prompt)
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
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "bad-standing", Role: "worker", Standing: true, AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation"}}}
	err := runStandingConfig(cfg, true)
	if !errors.Is(err, ErrWorkerConfigPolicy) {
		t.Fatalf("standing entrypoint must return worker policy failure: %v", err)
	}
}

func TestForgeEntryPointReturnsPolicyFailureBeforeClaim(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "bad-forge", Role: "worker", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation"}}}
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

func TestUnsupportedHarnessConfigRejectsBeforeLifecycle(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{{Name: "pi-mutant", Role: launch.WorkerRole, AgentKind: router.PiHarness, Harness: router.PiHarness, Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort, TaskShape: launch.Implementation}}}
	valid, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "worker", Scope: router.ScopeLane, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	rec := &fakeLaunchLifecycle{}
	_, err = launchAdmissionWithLifecycle(rec, cfg, launch.WorkerRole, true, func(*config.LaneDef) (*router.LaunchDecision, error) { return valid, nil }, func(*router.LaunchDecision) error {
		t.Fatal("unsupported harness reached lifecycle")
		return nil
	})
	if !errors.Is(err, ErrHarnessConfigPolicy) {
		t.Fatalf("unsupported harness error = %v", err)
	}
	if *rec != (fakeLaunchLifecycle{}) {
		t.Fatalf("unsupported harness caused lifecycle effects: %+v", rec)
	}
}

func TestConfiguredRolePoliciesAreComplete(t *testing.T) {
	cases := []config.LaneDef{
		{Name: "worker", Role: "worker", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "claude-worker", Role: "worker", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "implementation"},
		{Name: "forge", Role: "forge-smith", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "recovery-worker", Role: "recovery", AgentKind: "codex", Harness: "codex", Provider: "codex", Model: "gpt-5.6-luna", Effort: "medium", TaskShape: "implementation"},
		{Name: "reviewer", Role: "reviewer", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "qa"},
		{Name: "assayer", Role: "assayer", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "qa"},
		{Name: "orchestrator", Role: "orchestrator", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-opus-5", Effort: "medium", TaskShape: "coordinator"},
		{Name: "scout-planner", Role: "scout-planner", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-opus-5", Effort: "medium", TaskShape: "architecture"},
		{Name: "verification", Role: "verification-gate", AgentKind: "opencode", Harness: "opencode", Provider: "opencode", Model: "opencode/kimi-k3", Effort: "medium", TaskShape: "bounded"},
		{Name: "supervisor", Role: "review-supervisor", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "coordinator"},
		{Name: "harvest", Role: "harvest", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "bounded"},
		{Name: "sentinel", Role: "recovery-sentinel", AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "bounded"},
	}
	for _, lane := range cases {
		if err := validateLaneLaunchConfig(&lane); err != nil {
			t.Errorf("%s: %v", lane.Role, err)
		}
	}
}

func TestLaneLaunchConfigVendorHarnesses(t *testing.T) {
	for _, harness := range []string{"codex", "claude", "grok", "agy", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			lane := &config.LaneDef{Name: harness + "-reviewer", Role: launch.ReviewerRole, AgentKind: harness, Harness: harness, Provider: harness, Model: "test-model", Effort: "medium", TaskShape: "qa"}
			if err := validateLaneLaunchConfig(lane); err != nil {
				t.Fatalf("supported harness rejected: %v", err)
			}
		})
	}

	pi := &config.LaneDef{Name: "pi-reviewer", Role: launch.ReviewerRole, AgentKind: router.PiHarness, Harness: router.PiHarness, Provider: "codex", Model: "test-model", Effort: "medium", TaskShape: "qa"}
	if err := validateLaneLaunchConfig(pi); !errors.Is(err, ErrHarnessConfigPolicy) {
		t.Fatalf("Pi must fail closed as an unsupported harness: %v", err)
	}
}

func TestLaneLaunchDecisionBindsConfiguredCodexHarnessWithoutPi(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	pinHealthyQuota(t, dir, "codex")
	lane := &config.LaneDef{Name: "smith", Role: launch.WorkerRole, AgentKind: "codex", Harness: "codex", Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort, TaskShape: launch.Implementation}

	decision, err := laneLaunchDecisionWithProbe(context.Background(), lane, nil, func(_ context.Context, _, model, _ string) herdr.ProbeResult {
		return herdr.ProbeResult{Model: model, Available: true}
	})
	if err != nil {
		t.Fatalf("configured Codex route rejected: %v", err)
	}
	if decision.Provider != launch.WorkerProvider || decision.Model != launch.WorkerModel || decision.Effort != launch.WorkerEffort || decision.Harness != "codex" {
		t.Fatalf("routed tuple drifted: %+v", decision)
	}
	wantHarnessArgv := router.ArgvFor("codex", launch.WorkerModel, launch.WorkerEffort)
	if strings.Join(decision.HarnessArgv, "\n") != strings.Join(wantHarnessArgv, "\n") {
		t.Fatalf("harness argv = %q, want %q", decision.HarnessArgv, wantHarnessArgv)
	}
	if err := launch.Validate(launch.Request{Decision: decision, TaskRef: lane.Name, Scope: router.ScopeLane}, nil); err != nil {
		t.Fatalf("direct Codex launch decision must validate: %v", err)
	}
}

func TestLaneLaunchDecisionReportsConfiguredProbeFailure(t *testing.T) {
	dir := t.TempDir()
	codexMarker := filepath.Join(dir, "codex.called")
	pi := "#!/bin/sh\necho NOT_PROBE_OK\n"
	codex := "#!/bin/sh\necho called > \"" + codexMarker + "\"\nexit 91\n"
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(pi), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HERD_MODE", "production")
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	lane := &config.LaneDef{Name: "smith", Role: launch.WorkerRole, AgentKind: "codex", Harness: "codex", Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort, TaskShape: launch.Implementation}

	_, err := laneLaunchDecision(context.Background(), lane, nil)
	if err == nil {
		t.Fatal("expected configured probe failure")
	}
	msg := err.Error()
	for _, want := range []string{lane.Name, "codex/gpt-5.6-luna", "no exact probe output"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "no healthy launch candidate") {
		t.Fatalf("must attribute configured probe failure, not generic candidate failure: %q", msg)
	}
	if _, err := os.Stat(codexMarker); !os.IsNotExist(err) {
		t.Fatalf("failed Pi probe invoked native codex: %v", err)
	}
}

// TestLaneLaunchDecisionFailsOnMissingHarnessBinary proves the harness binary
// is checked at config-eval time (before probing), producing an actionable
// error naming the missing binary — not a confusing probe-exec failure.
// The test sets PATH to an empty dir so the configured Codex harness is absent.
func TestLaneLaunchDecisionFailsOnMissingHarnessBinary(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	lane := &config.LaneDef{
		Name: "mender", Role: launch.WorkerRole, AgentKind: "codex", Harness: "codex",
		Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort, TaskShape: launch.Implementation,
	}
	_, err := laneLaunchDecision(context.Background(), lane, nil)
	if !errors.Is(err, ErrHarnessBinaryMissing) {
		t.Fatalf("missing harness binary must fail with ErrHarnessBinaryMissing, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{lane.Name, "codex", "not found in $PATH"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

// TestLaneLaunchDecisionSucceedsWithHarnessBinary proves the check does not
// false-positive when the configured binary IS present. Its injected healthy
// probe keeps this launch-surface test hermetic and independent of Pi.
func TestLaneLaunchDecisionSucceedsWithHarnessBinary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	pinHealthyQuota(t, dir, "codex")
	lane := &config.LaneDef{
		Name: "smith", Role: launch.WorkerRole, AgentKind: "codex", Harness: "codex",
		Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort, TaskShape: launch.Implementation,
	}
	decision, err := laneLaunchDecisionWithProbe(context.Background(), lane, nil, func(_ context.Context, _, model, _ string) herdr.ProbeResult {
		return herdr.ProbeResult{Model: model, Available: true}
	})
	if err != nil {
		t.Fatalf("harness binary present must not block route: %v", err)
	}
	if decision == nil {
		t.Fatal("expected a launch decision")
	}
	if decision.Harness != "codex" || len(decision.HarnessArgv) == 0 || decision.HarnessArgv[0] != "codex" {
		t.Fatalf("decision must launch configured Codex harness: %+v", decision)
	}
}

func TestReviewerLaunchDecisionBindsReroutedVendorHarness(t *testing.T) {
	dir := t.TempDir()
	for _, harness := range []string{"claude", "grok"} {
		if err := os.WriteFile(filepath.Join(dir, harness), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_USE_PI", "0")
	routeDir := t.TempDir()
	t.Setenv("HERDR_ROUTE_STATE_DIR", routeDir)
	pinHealthyQuota(t, dir, "claude", "grok")
	if err := os.WriteFile(filepath.Join(routeDir, "claude.cooldown.json"), []byte(`{"expiresAt":4102444800,"provider":"claude","reason":"exhausted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_ERA_PROVIDERS", "claude grok")
	lane := &config.LaneDef{Name: "reviewer", Role: launch.ReviewerRole, AgentKind: "claude", Harness: "claude", Provider: "claude", Model: "claude-sonnet-5", Effort: "medium", TaskShape: "qa"}
	task := &provider.Task{Ref: "FAC-REROUTE", Labels: []string{"author-family:anthropic", "author-model:claude-sonnet-5", "candidate-sha:" + strings.Repeat("a", 40)}}

	decision, err := laneLaunchDecisionWithProbe(context.Background(), lane, task, func(_ context.Context, _, model, _ string) herdr.ProbeResult {
		return herdr.ProbeResult{Model: model, Available: true}
	})
	if err != nil {
		t.Fatalf("reviewer reroute rejected: %v", err)
	}
	if decision.Provider != "grok" || decision.Harness != "grok" || decision.Model != router.ModelFor("grok", "qa") {
		t.Fatalf("rerouted reviewer tuple is incoherent: %+v", decision)
	}
}

func TestStandingGrokLaneCannotRerouteToClaude(t *testing.T) {
	dir := t.TempDir()
	// BOTH harnesses must be present. With only grok on PATH the waterfall had
	// nothing else to pick, so this test passed even with the standing family
	// boundary disabled outright -- it asserted a property it never exercised.
	// claude is here so a released boundary is actually reachable and fails.
	for _, harness := range []string{"grok", "claude"} {
		if err := os.WriteFile(filepath.Join(dir, harness), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// FAC-684: this test read the MACHINE's live cooldowns and live quota, so
	// it passed or failed by time of day -- it failed on a day grok was
	// genuinely exhausted, reporting a routing regression that did not exist
	// while hiding whether the boundary logic still held. Pin both.
	t.Setenv("PATH", dir)
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	pinHealthyQuota(t, dir, "grok", "claude")
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_USE_PI", "0")
	lane := &config.LaneDef{
		Name: "scout-planner", Role: launch.ScoutPlannerRole, AgentKind: "grok", Harness: "grok",
		Provider: "grok", Model: "grok-4.6", Effort: "medium", TaskShape: "architecture", Standing: true,
	}
	d, err := laneLaunchDecisionWithProbe(context.Background(), lane, nil, func(_ context.Context, _, model, _ string) herdr.ProbeResult {
		return herdr.ProbeResult{Model: model, Available: true}
	})
	if err != nil {
		t.Fatalf("standing Grok route rejected: %v", err)
	}
	if d.Provider != "grok" || d.Model != "grok-4.6" || d.Harness != "grok" {
		t.Fatalf("standing route was rerouted from its explicit tuple: %+v", d)
	}
}

func TestStandingQuotaAdmissionFailsClosedForExhaustedPinnedPool(t *testing.T) {
	lane := &config.LaneDef{Name: "grok-lane", Provider: "grok", Model: "grok-4.6"}
	computed := map[string]usage.BurnState{
		"grok": {Available: false, Reason: "exhausted", Used: 100, Remaining: 0},
	}
	if err := admitStandingQuotaState(lane, computed); err == nil || !strings.Contains(err.Error(), "grok/default") {
		t.Fatalf("exhausted pinned pool must be refused, got %v", err)
	}
}

func TestStandingQuotaAdmissionFailsClosedForUnknownPinnedPool(t *testing.T) {
	lane := &config.LaneDef{Name: "grok-lane", Provider: "grok", Model: "grok-4.6"}
	if err := admitStandingQuotaState(lane, nil); err == nil || !strings.Contains(err.Error(), "unknown quota") {
		t.Fatalf("unknown pinned pool must be refused, got %v", err)
	}
}

// FAC-642: a standing lane's provider/model is a PREFERENCE -- launchStandingLane
// sends it as PreferredProvider/PreferredModel, so the router may route the lane
// elsewhere. Refusing at admission because the preferred pool is spent killed the
// lane before the router could, which is why operators hand-edited pins in
// .herd/herd.yaml during a crunch (chainseer PR #3210).
func TestStandingQuotaAdmissionAdmitsSpentPreferenceWhenAnotherSurfaceHasCapacity(t *testing.T) {
	lane := &config.LaneDef{Name: "grok-lane", Provider: "grok", Model: "grok-4.6"}
	computed := map[string]usage.BurnState{
		"grok":  {Available: false, Reason: "exhausted", Used: 100, Remaining: 0},
		"codex": {Available: true, Used: 42, Remaining: 58},
	}
	if err := admitStandingQuotaState(lane, computed); err != nil {
		t.Fatalf("a spent PREFERENCE must not refuse the lane while another surface has capacity; the router is the real gate: %v", err)
	}
}

// The whole fleet being spent is still a genuine refusal, so the fix cannot be
// satisfied by always admitting.
func TestStandingQuotaAdmissionStillRefusesWhenEverySurfaceIsSpent(t *testing.T) {
	lane := &config.LaneDef{Name: "grok-lane", Provider: "grok", Model: "grok-4.6"}
	computed := map[string]usage.BurnState{
		"grok":  {Available: false, Reason: "exhausted"},
		"codex": {Available: false, Reason: "exhausted"},
	}
	err := admitStandingQuotaState(lane, computed)
	if err == nil {
		t.Fatal("a genuinely spent fleet must still be refused")
	}
	if !strings.Contains(err.Error(), "no other surface has capacity") {
		t.Errorf("the refusal must say the alternatives were checked, not just that the pin was spent: %v", err)
	}
}

// FAC-643: found by herd-smith. reviewLedgerPath was cwd-relative while
// readPulseReview's own inbox sweep resolves the PROJECT root, so from a standing
// worktree the gating stat missed the real ledger, took the absent branch, and
// never swept -- reporting inbox_uningested=0 next to 123 waiting verdict files.
func TestReviewLedgerPathResolvesProjectRootNotCwd(t *testing.T) {
	t.Setenv("HERD_REVIEW_LEDGER", "")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A nested directory stands in for a lane worktree: the ledger is at the
	// project root, the caller is somewhere below it.
	nested := filepath.Join(root, "pkg", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got := reviewLedgerPath()
	if got == filepath.Join(".herd", "review-ledger.jsonl") {
		t.Fatal("reviewLedgerPath returned a bare cwd-relative path; a caller outside the project root resolves the wrong ledger (or none)")
	}
	if !filepath.IsAbs(got) {
		t.Errorf("expected a root-anchored path so every ledger consumer agrees, got %q", got)
	}
}

// FAC-684 end-to-end: the exact live report -- "standing launch admitted a spent
// Grok preference and started forge-herd-smith into 0% weekly quota even though
// dry-run said Claude/default was available. After cooling Grok, standing
// refused no healthy candidate."
//
// Standing admission ADMITS a spent preference (FAC-642) on the stated grounds
// that the router will reroute. This proves the router actually does.
func TestStandingLaneWithSpentPreferenceReroutesInsteadOfLaunchingIntoZero(t *testing.T) {
	dir := t.TempDir()
	for _, harness := range []string{"grok", "claude"} {
		if err := os.WriteFile(filepath.Join(dir, harness), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// CI runners do not have grok/claude on PATH. The fixtures above are the
	// harnesses; put them first so harness_binary_missing cannot fire before
	// the quota reroute under test.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pinQuota(t, dir, map[string]float64{"grok": 100, "claude": 10})
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("HERD_MODE", "local")
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("HERD_ERA_PROVIDERS", "claude grok")

	lane := &config.LaneDef{
		Name: "scout-planner", Role: launch.ScoutPlannerRole, AgentKind: "grok", Harness: "grok",
		Provider: "grok", Model: "grok-4.6", Effort: "medium", TaskShape: "architecture", Standing: true,
	}
	d, err := laneLaunchDecisionWithProbe(context.Background(), lane, nil, func(_ context.Context, _, model, _ string) herdr.ProbeResult {
		return herdr.ProbeResult{Model: model, Available: true}
	})
	if err != nil {
		t.Fatalf("standing lane refused rather than rerouted off its spent preference: %v", err)
	}
	if d.Provider == "grok" {
		t.Fatalf("standing lane launched into its EXHAUSTED preferred provider: %+v", d)
	}
}

// pinHealthyQuota makes a lane-launch test independent of the MACHINE's live
// provider quota.
//
// FAC-684: laneLaunchDecisionWithProbe used to build its router with no quota
// data at all, so these tests could not see quota even in principle. Fixing that
// blindness -- the actual production defect -- exposed that they had never
// controlled it: they began failing on a day grok and codex were genuinely
// exhausted, reporting a routing regression that did not exist. A test whose
// verdict depends on the time of day is not evidence either way.
func pinHealthyQuota(t *testing.T, dir string, providers ...string) {
	t.Helper()
	used := map[string]float64{}
	for _, name := range providers {
		used[name] = 10
	}
	pinQuota(t, dir, used)
}

// pinQuota pins each named provider's weekly window to an exact used percent.
func pinQuota(t *testing.T, dir string, used map[string]float64) {
	t.Helper()
	res := func(used float64) map[string]any {
		return map[string]any{"kind": "consumption", "limit": 100, "remaining": 100 - used,
			"resetsAt": "2099-01-01T00:00:00Z", "unit": "percent", "used": used,
			"utilization": used / 100, "windowSeconds": 604800}
	}
	entries := map[string]any{}
	for name, pct := range used {
		entries[name] = map[string]any{"displayName": name, "stale": false,
			"resources": map[string]any{"weekly": res(pct)}}
	}
	body, err := json.Marshal(map[string]any{
		"generatedAt": "2026-08-26T00:00:00.000Z", "schema": "openusage.limits.v1",
		"providers": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "openusage")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat <<'JSON'\n"+string(body)+"\nJSON\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_OPENUSAGE_BIN", stub)
	// The snapshot cache is per-USER and persisted to disk (FAC-679), so a stub
	// binary alone does not isolate anything: the real machine's cached quota
	// is served before the stub is ever run. Point the cache somewhere empty.
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(t.TempDir(), "quota.json"))
	// The cache is also in-memory and PROCESS-wide, so whichever test ran first
	// pins the real machine's quota for every test after it. Drop it on entry
	// and on exit, or this helper isolates nothing after the first call.
	usage.InvalidateSnapshotCache()
	t.Cleanup(usage.InvalidateSnapshotCache)
	// The reroute target must not additionally trip the real machine's claude
	// hook-policy pin, which is not what these tests are about.
	t.Setenv("HOME", t.TempDir())
}
