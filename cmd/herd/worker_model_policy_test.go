package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/store"
)

// TestGateAcceptsLiveQuotaRoutedIdentities proves FAC-194 does not re-pin
// workers to codex/gpt-5.6-luna/medium. The live waterfall may return high
// effort (shape default) or a non-codex provider when codex is spent.
func TestGateAcceptsLiveQuotaRoutedIdentities(t *testing.T) {
	cases := []struct {
		provider, model, effort string
	}{
		{"codex", "gpt-5.6-luna", "medium"},
		{"codex", "gpt-5.6-luna", "high"}, // EffortFor("implementation") default
		{"claude", "claude-sonnet-5", "medium"},
		{"grok", "grok-4.5", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.model+"/"+tc.effort, func(t *testing.T) {
			if err := rejectProductionWorkerModelIdentity(tc.provider, tc.model, tc.effort); err != nil {
				t.Fatalf("coherent routed identity rejected: %v", err)
			}
			mr, err := modelRouterFromWorkerIdentity(tc.provider, tc.model, tc.effort)
			if err != nil {
				t.Fatalf("modelRouterFromWorkerIdentity: %v", err)
			}
			got, err := mr.SelectProvider(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tc.provider || got.Model != tc.model || got.ReasoningEffort != tc.effort {
				t.Fatalf("identity = %s/%s/%s, want %s/%s/%s",
					got.Name, got.Model, got.ReasoningEffort, tc.provider, tc.model, tc.effort)
			}
		})
	}
}

// TestGateRejectsForbiddenWorkerIdentities covers missing, OpenCode, Ollama,
// DeepSeek, coordinator-tier, and multi-candidate fallbacks.
func TestGateRejectsForbiddenWorkerIdentities(t *testing.T) {
	cases := []struct {
		name, provider, model, effort string
	}{
		{"opencode provider", "opencode", "deepseek-v4-flash", "medium"},
		{"ollama provider", "ollama", "litellm/ollama/glm-5.2:cloud", "medium"},
		{"deepseek model", "codex", "deepseek-v4-flash", "medium"},
		{"coordinator sol", "codex", "gpt-5.6-sol", "medium"},
		{"coordinator terra", "codex", "gpt-5.6-terra", "high"},
		{"coordinator fable", "claude", "claude-fable-5", "medium"},
		{"missing provider", "", "gpt-5.6-luna", "medium"},
		{"missing model", "codex", "", "medium"},
		{"missing effort", "codex", "gpt-5.6-luna", ""},
		{"invalid effort", "codex", "gpt-5.6-luna", "ultra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rejectProductionWorkerModelIdentity(tc.provider, tc.model, tc.effort)
			if !errors.Is(err, ErrWorkerModelPolicy) {
				t.Fatalf("reject(%s,%s,%s) = %v", tc.provider, tc.model, tc.effort, err)
			}
		})
	}
	_, err := modelRouterFromCandidates([]*router.ModelCandidate{
		{Name: "codex", Type: router.ProviderOpenAI, Model: "gpt-5.6-luna", ReasoningEffort: "medium"},
		{Name: "opencode", Type: router.ProviderOllama, Model: "deepseek-v4-flash"},
	})
	if !errors.Is(err, ErrWorkerModelPolicy) {
		t.Fatalf("multi-candidate: %v", err)
	}
	_, err = modelRouterFromCandidates([]*router.ModelCandidate{
		{Name: "opencode", Type: router.ProviderOllama, Model: "deepseek-v4-flash", ReasoningEffort: "medium"},
	})
	if !errors.Is(err, ErrWorkerModelPolicy) {
		t.Fatalf("embedded OpenCode candidate: %v", err)
	}
}

func TestMissingLaunchDecisionFailsClosed(t *testing.T) {
	_, err := modelRouterFromLaunchDecision(nil)
	if !errors.Is(err, ErrWorkerModelPolicy) {
		t.Fatalf("nil decision: %v", err)
	}
}

// TestApplyWorkerModelRouterBeforeClaimIsProductionSeam exercises the real
// forge/spawn ordering helper: gate first, claim only on success. Positive
// control proves counters move; OpenCode mutant proves they do not.
func TestApplyWorkerModelRouterBeforeClaimIsProductionSeam(t *testing.T) {
	var claim, worktree, tab, process, prompt int
	effect := func(mr *router.ModelRouter) error {
		if mr == nil {
			return errors.New("nil router after gate")
		}
		claim++
		worktree++
		tab++
		process++
		prompt++
		return nil
	}

	// Positive: shape-default high effort (live EffortFor implementation).
	high := &router.LaunchDecision{
		Role: router.RoleWorker, Shape: launch.Implementation,
		Provider: "codex", Model: "gpt-5.6-luna", Effort: "high",
	}
	if err := applyWorkerModelRouterBeforeClaim(high, effect); err != nil {
		t.Fatalf("high-effort routed decision: %v", err)
	}
	if claim != 1 || worktree != 1 || tab != 1 || process != 1 || prompt != 1 {
		t.Fatalf("positive control counters stuck: claim=%d worktree=%d tab=%d process=%d prompt=%d",
			claim, worktree, tab, process, prompt)
	}

	// Positive: non-codex waterfall pick.
	claim, worktree, tab, process, prompt = 0, 0, 0, 0, 0
	claude := &router.LaunchDecision{
		Role: router.RoleWorker, Shape: launch.Implementation,
		Provider: "claude", Model: "claude-sonnet-5", Effort: "medium",
	}
	if err := applyWorkerModelRouterBeforeClaim(claude, effect); err != nil {
		t.Fatalf("claude routed decision: %v", err)
	}
	if claim != 1 {
		t.Fatalf("claude claim counter = %d", claim)
	}

	// Mutant: historical embedded OpenCode/DeepSeek candidate.
	claim, worktree, tab, process, prompt = 0, 0, 0, 0, 0
	mutant := &router.LaunchDecision{
		Role: router.RoleWorker, Shape: launch.Implementation,
		Provider: "opencode", Model: "deepseek-v4-flash", Effort: "medium",
	}
	err := applyWorkerModelRouterBeforeClaim(mutant, effect)
	if err == nil || !errors.Is(err, ErrWorkerModelPolicy) {
		t.Fatalf("opencode mutant must fail with ErrWorkerModelPolicy, got %v", err)
	}
	if claim|worktree|tab|process|prompt != 0 {
		t.Fatalf("mutant reached claim seams: claim=%d worktree=%d tab=%d process=%d prompt=%d",
			claim, worktree, tab, process, prompt)
	}
}

// TestLaunchAdmissionLifecycleComposesModelGateBeforeClaim composes the two
// real seams — launchAdmissionWithLifecycle wrapping
// applyWorkerModelRouterBeforeClaim — and proves a quota-routed decision is
// admitted, reaches every lifecycle counter, and hands the effect a router
// whose identity matches the decision exactly.
//
// It deliberately does NOT assert the rejection ordering: fakeLaunchLifecycle
// increments its counters on Run entry, before calling effect, so they cannot
// witness a gate that fires inside the effect. That ordering is proven by
// TestApplyWorkerModelRouterBeforeClaimIsProductionSeam, which counts inside
// the effect body where a rejection can actually suppress the increment.
func TestLaunchAdmissionLifecycleComposesModelGateBeforeClaim(t *testing.T) {
	lane := config.LaneDef{
		Name: "worker", Role: "worker", AgentKind: router.PiHarness, Harness: router.PiHarness,
		Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort,
		TaskShape: launch.Implementation,
	}
	cfg := &config.Config{Lanes: []config.LaneDef{lane}}

	// Valid high-effort decision the live router can produce when effort is
	// shape-defaulted. Build via Decide without RequestedEffort.
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	r := testLaunchRouter(t)
	// RequestedEffort empty → EffortFor("implementation") = high when env unset.
	t.Setenv("HERD_EFFORT_IMPLEMENTATION", "")
	high, err := r.Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel,
		// no RequestedEffort — shape default
		TaskRef: "worker", Scope: router.ScopeLane,
		ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Shape default is high; env may coerce medium. Any other value means the
	// router produced an effort the worker gate is not specified against.
	if high.Effort != "high" && high.Effort != "medium" {
		t.Fatalf("routed effort = %q, want high or medium", high.Effort)
	}

	rec := &fakeLaunchLifecycle{}
	got, err := launchAdmissionWithLifecycle(rec, cfg, lane.Role, true,
		func(*config.LaneDef) (*router.LaunchDecision, error) { return high, nil },
		func(d *router.LaunchDecision) error {
			return applyWorkerModelRouterBeforeClaim(d, func(mr *router.ModelRouter) error {
				c, selErr := mr.SelectProvider(context.Background())
				if selErr != nil {
					return selErr
				}
				if c.Name != high.Provider || c.Model != high.Model || c.ReasoningEffort != high.Effort {
					return errors.New("router identity drifted from decision")
				}
				return nil
			})
		},
	)
	if err != nil {
		t.Fatalf("admission+gate must accept routed decision: %v", err)
	}
	if got != high || rec.decision != high {
		t.Fatal("decision identity not preserved through lifecycle")
	}
	if rec.claim != 1 || rec.worktree != 1 || rec.tab != 1 || rec.process != 1 || rec.prompt != 1 {
		t.Fatalf("lifecycle counters not exercised: %+v", rec)
	}
}

// TestForgeAdmissionRejectsBeforeClaimWhenLaneForbidden keeps using the real
// forgeLaunchAdmission entrypoint: bad lane config never reaches the claim
// effect (pre-existing launch policy; still holds after FAC-194).
func TestForgeAdmissionRejectsBeforeClaimWhenLaneForbidden(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{{
		Name: "bad-forge", Role: "worker", AgentKind: router.PiHarness, Harness: router.PiHarness,
		Provider: "codex", Model: "gpt-5.6-sol", Effort: "medium", TaskShape: "implementation",
	}}}
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

func TestWorkerModelPolicyBlockedEvidenceIsDurable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "herdforge.db")
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	reason := "mutant opencode/deepseek-v4-flash restored"
	if err := recordWorkerModelPolicyBlocked(st, "forge", reason); err != nil {
		t.Fatal(err)
	}
	history, err := st.BlockedSelectionHistory(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("expected durable BLOCKED evidence")
	}
	found := false
	for _, rec := range history {
		if rec.Code == "worker_model_policy" && rec.Entrypoint == "forge" && strings.Contains(rec.Reason, reason) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing worker_model_policy evidence: %+v", history)
	}
}

func TestMissingStoreBlocksWorkerModelPolicyEvidence(t *testing.T) {
	err := recordWorkerModelPolicyBlocked(nil, "pulse", "missing route")
	if err == nil || !strings.Contains(err.Error(), "durable BLOCKED") {
		t.Fatalf("error = %v", err)
	}
}

// TestProductionMainHasNoEmbeddedOpenCodeDeepSeekRouter source-audits main.go.
// Bare "deepseek-v4-flash" is the durable check (gofmt-stable).
func TestProductionMainHasNoEmbeddedOpenCodeDeepSeekRouter(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		src, err = os.ReadFile(filepath.Join("cmd", "herd", "main.go"))
		if err != nil {
			t.Fatal(err)
		}
	}
	body := string(src)
	if strings.Contains(body, `"deepseek-v4-flash"`) {
		t.Fatal(`production main.go must not name "deepseek-v4-flash"`)
	}
	if strings.Contains(body, `ProviderOllama`) && strings.Contains(body, `deepseek`) {
		// Residual historical pairing.
		if strings.Contains(body, `Type: router.ProviderOllama`) {
			t.Fatal("production main.go still pairs ProviderOllama with a ModelRouter candidate construction")
		}
	}
}

// TestModelRouterFromDecisionPreservesRoutedArgv builds a real Decide result
// and checks the gate preserves provider/model/effort for dispatch capture.
func TestModelRouterFromDecisionPreservesRoutedArgv(t *testing.T) {
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	t.Setenv("HERD_EFFORT_IMPLEMENTATION", "")
	decision, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel,
		TaskRef: "worker", Scope: router.ScopeLane,
		ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	mr, err := modelRouterFromLaunchDecision(decision)
	if err != nil {
		t.Fatalf("gate rejected legitimate Decide result effort=%s: %v", decision.Effort, err)
	}
	got, err := mr.SelectProvider(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != decision.Provider || got.Model != decision.Model || got.ReasoningEffort != decision.Effort {
		t.Fatalf("router %s/%s/%s != decision %s/%s/%s",
			got.Name, got.Model, got.ReasoningEffort,
			decision.Provider, decision.Model, decision.Effort)
	}
	if len(decision.HarnessArgv) == 0 || decision.HarnessArgv[0] != "pi" {
		t.Fatalf("decision harness argv not captured: %v", decision.HarnessArgv)
	}
}
