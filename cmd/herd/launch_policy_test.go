package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
)

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
	valid, err := router.NewRouter(nil, nil).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
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
	valid, err := router.NewRouter(nil, nil).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
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
