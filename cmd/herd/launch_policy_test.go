package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
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
