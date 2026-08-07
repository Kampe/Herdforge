package launch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolprobe"
)

func passReceipt(t *testing.T, d *router.LaunchDecision, now time.Time) *toolprobe.Receipt {
	t.Helper()
	id, err := toolprobe.IdentityFromDecision(d)
	if err != nil {
		t.Fatal(err)
	}
	r, err := toolprobe.NewReceipt(id, toolprobe.StatusPASS, "", "sha256:test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &r
}

func TestAdmitRequiresDecisionAndProbe(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	req := good(t)
	d := req.Decision
	// Scope/task binding for Validate when lease is zero is OK for lane fixtures.
	sink := &MemorySink{}

	if _, err := Admit(BoundarySpec{Now: func() time.Time { return now }, Sink: sink}); err == nil {
		t.Fatal("missing decision must fail")
	}
	if _, err := Admit(BoundarySpec{Decision: d, Request: req, Now: func() time.Time { return now }, Sink: sink}); err == nil {
		t.Fatal("missing probe must fail for write-capable worker")
	}
	plan, err := Admit(BoundarySpec{
		Decision: d,
		Request:  req,
		Probe:    passReceipt(t, d, now),
		Cwd:      "/tmp/wt",
		Label:    "worker",
		Now:      func() time.Time { return now },
		Sink:     sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Model != d.Model || plan.Effort != d.Effort || plan.Provider != d.Provider {
		t.Fatalf("plan drifted from decision: %+v vs %+v", plan, d)
	}
	if len(plan.Argv) == 0 || strings.Join(plan.Argv, " ") != strings.Join(d.Argv, " ") {
		t.Fatalf("plan argv must equal decision argv: %v vs %v", plan.Argv, d.Argv)
	}
	if plan.CallbackContract == "" {
		t.Fatal("callback contract required")
	}
}

func TestAdmitRejectsStaleAndIncapableProbe(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	req := good(t)
	d := req.Decision
	id, err := toolprobe.IdentityFromDecision(d)
	if err != nil {
		t.Fatal(err)
	}

	stale, err := toolprobe.NewReceipt(id, toolprobe.StatusPASS, "", "sha256:x", now.Add(-2*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Admit(BoundarySpec{Decision: d, Request: req, Probe: &stale, Now: func() time.Time { return now }}); err == nil {
		t.Fatal("stale probe must fail")
	}

	incapable, err := toolprobe.NewReceipt(id, toolprobe.StatusINCAPABLE, "no file", "", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Admit(BoundarySpec{Decision: d, Request: req, Probe: &incapable, Now: func() time.Time { return now }}); err == nil {
		t.Fatal("INCAPABLE probe must fail")
	}
}

func TestAdmitRejectsCrossProviderProbe(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	req := good(t)
	d := req.Decision
	wrong := toolprobe.Identity{
		Provider: "opencode", Model: d.Model, Harness: "pi",
		Recipe: toolprobe.RecipeArtifactWrite, Toolchain: toolprobe.ToolchainV1,
	}
	r, err := toolprobe.NewReceipt(wrong, toolprobe.StatusPASS, "", "sha256:x", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Admit(BoundarySpec{Decision: d, Request: req, Probe: &r, Now: func() time.Time { return now }}); err == nil {
		t.Fatal("cross-provider probe must fail")
	}
}

func TestAdmitEnforcesReadAuthority(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	req := good(t)
	d := req.Decision
	lane := &config.LaneDef{Name: "observer", Role: "worker", Authority: config.AuthorityRead}
	if _, err := Admit(BoundarySpec{
		Decision: d, Request: req, Probe: passReceipt(t, d, now), Lane: lane,
		Now: func() time.Time { return now },
	}); err == nil {
		t.Fatal("read authority must block write-capable launch")
	}
}

func TestOpenFailsBeforeTabWhenProbeMissing(t *testing.T) {
	req := good(t)
	d := req.Decision
	opened := 0
	opener := tabOpenerFunc(func(workspace, label, cwd string, noFocus bool, env ...string) (string, string, error) {
		opened++
		return "tab-1", "pane-1", nil
	})
	_, _, _, err := Open(opener, BoundarySpec{
		Decision: d, Request: req,
		Workspace: "w1", Label: "worker", Cwd: "/tmp/wt", NoFocus: true,
		Now: func() time.Time { return time.Unix(1_800_000_000, 0).UTC() },
	})
	if err == nil {
		t.Fatal("expected probe failure")
	}
	if opened != 0 {
		t.Fatalf("OpenTab must not run when probe absent; opened=%d", opened)
	}
}

func TestOpenCapturesDecisionArgvInPlan(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	req := good(t)
	d := req.Decision
	var gotWS, gotLabel, gotCwd string
	var gotEnv []string
	opener := tabOpenerFunc(func(workspace, label, cwd string, noFocus bool, env ...string) (string, string, error) {
		gotWS, gotLabel, gotCwd, gotEnv = workspace, label, cwd, append([]string(nil), env...)
		return "tab-x", "pane-x", nil
	})
	plan, tabID, paneID, err := Open(opener, BoundarySpec{
		Decision: d, Request: req, Probe: passReceipt(t, d, now),
		Workspace: "wABC", Label: "task-fac-139", Cwd: "/tmp/wt-fac-139",
		Env: []string{"PATH=/wrap:/usr/bin"}, NoFocus: true,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if tabID != "tab-x" || paneID != "pane-x" {
		t.Fatalf("tab identity: %s %s", tabID, paneID)
	}
	if gotWS != "wABC" || gotLabel != "task-fac-139" || gotCwd != "/tmp/wt-fac-139" {
		t.Fatalf("opener args: %s %s %s", gotWS, gotLabel, gotCwd)
	}
	if len(gotEnv) != 1 || gotEnv[0] != "PATH=/wrap:/usr/bin" {
		t.Fatalf("env: %v", gotEnv)
	}
	if plan.Model != d.Model || plan.Effort != d.Effort {
		t.Fatalf("plan model/effort: %s/%s want %s/%s", plan.Model, plan.Effort, d.Model, d.Effort)
	}
	// Mutation guard: if someone later hardcodes a default model into Plan, fail.
	if plan.Model == "" || plan.Effort == "" {
		t.Fatal("plan must not inherit empty harness defaults")
	}
}

type tabOpenerFunc func(workspace, label, cwd string, noFocus bool, env ...string) (string, string, error)

func (f tabOpenerFunc) OpenTab(workspace, label, cwd string, noFocus bool, env ...string) (string, string, error) {
	return f(workspace, label, cwd, noFocus, env...)
}

func TestDecideWithToolProbeFailovers(t *testing.T) {
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	now := time.Unix(1_800_000_000, 0).UTC()
	r := testRouter(t)

	// Hermetic CLI surface is Pi-only (codex). Seed router probe map so Decide
	// can rank luna, then tool-probe marks it INCAPABLE and the next Decide
	// with that key forced false must fail closed — never invent a PASS.
	req := router.LaunchRequest{
		Role:              router.RoleWorker,
		Shape:             Implementation,
		RequestedProvider: testWorkerProvider,
		RequestedModel:    testWorkerModel,
		RequestedEffort:   testWorkerEffort,
		ProbeResults:      map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
		TaskRef:           "FAC-139",
		Scope:             router.ScopeTask,
		LeaseGeneration:   1,
	}

	calls := 0
	runner := toolprobe.FuncRunner(func(_ context.Context, id toolprobe.Identity) toolprobe.Receipt {
		calls++
		rec, err := toolprobe.NewReceipt(id, toolprobe.StatusINCAPABLE, "no file created", "", now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	})

	d, receipt, err := DecideWithToolProbe(context.Background(), r, req, toolprobe.NewMemoryCache(), runner, now)
	if err == nil || d != nil {
		t.Fatalf("tool-incapable surface must fail closed, got d=%+v err=%v", d, err)
	}
	if receipt == nil || receipt.Status != toolprobe.StatusINCAPABLE {
		t.Fatalf("must surface classified INCAPABLE receipt, got %+v", receipt)
	}
	if calls < 1 {
		t.Fatal("must invoke artifact tool-probe at least once")
	}
	if receipt.Status.WriteCapable() {
		t.Fatal("INCAPABLE must not be write-capable")
	}
}

func TestDecideWithToolProbePassAdmits(t *testing.T) {
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	now := time.Unix(1_800_000_000, 0).UTC()
	r := testRouter(t)
	req := router.LaunchRequest{
		Role:              router.RoleWorker,
		Shape:             Implementation,
		RequestedProvider: testWorkerProvider,
		RequestedModel:    testWorkerModel,
		RequestedEffort:   testWorkerEffort,
		ProbeResults:      map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true},
		TaskRef:           "FAC-139",
		Scope:             router.ScopeTask,
		LeaseGeneration:   1,
	}
	runner := toolprobe.FuncRunner(func(_ context.Context, id toolprobe.Identity) toolprobe.Receipt {
		rec, err := toolprobe.NewReceipt(id, toolprobe.StatusPASS, "", "sha256:ok", now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	})
	d, receipt, err := DecideWithToolProbe(context.Background(), r, req, toolprobe.NewMemoryCache(), runner, now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Model != testWorkerModel || d.Effort != testWorkerEffort {
		t.Fatalf("decision tuple: %s/%s", d.Model, d.Effort)
	}
	if !receipt.Passes(now) {
		t.Fatal("receipt must PASS")
	}
}

func TestDecideWithToolProbeRetriesNextCandidate(t *testing.T) {
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	now := time.Unix(1_800_000_000, 0).UTC()
	// Allow pi for codex and a second CLI so failover can leave the first surface.
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{
		CLIPresent: func(cli string) bool { return cli == router.PiHarness || cli == "grok" },
		Now:        func() time.Time { return now },
	}
	// Soft preference for luna (probe-gated). After INCAPABLE, Decide should
	// rank another healthy non-probe-gated candidate (grok).
	req := router.LaunchRequest{
		Role:              router.RoleWorker,
		Shape:             Implementation,
		PreferredProvider: "codex",
		PreferredModel:    "gpt-5.6-luna",
		ProbeResults: map[string]bool{
			router.ProbeKey("codex", "gpt-5.6-luna"): true,
		},
		TaskRef:         "FAC-139",
		Scope:           router.ScopeTask,
		LeaseGeneration: 1,
	}
	order := []string{}
	runner := toolprobe.FuncRunner(func(_ context.Context, id toolprobe.Identity) toolprobe.Receipt {
		order = append(order, id.Provider+"|"+id.Model)
		status := toolprobe.StatusPASS
		reason := ""
		if strings.Contains(id.Model, "luna") {
			status = toolprobe.StatusINCAPABLE
			reason = "no file"
		}
		rec, err := toolprobe.NewReceipt(id, status, reason, "sha256:x", now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	})
	d, receipt, err := DecideWithToolProbe(context.Background(), r, req, toolprobe.NewMemoryCache(), runner, now)
	if err != nil {
		t.Fatalf("failover should admit next candidate: %v (order=%v)", err, order)
	}
	if strings.Contains(d.Model, "luna") {
		t.Fatalf("must not admit tool-incapable luna, got %s (order=%v)", d.Model, order)
	}
	if !receipt.Passes(now) {
		t.Fatal("admitted receipt must PASS")
	}
	if len(order) < 2 {
		t.Fatalf("expected probe of failed then success candidate, got %v", order)
	}
}

func TestOpenMutationGuard_DefaultModelNotAccepted(t *testing.T) {
	// Hand-built decision without proof must fail Validate inside Admit.
	d := &router.LaunchDecision{
		Role: router.RoleWorker, Shape: Implementation,
		Provider: testWorkerProvider, Model: "gpt-5.6-sol", Effort: "ultra",
		Argv: []string{"codex", "--model", "gpt-5.6-sol", "-c", "model_reasoning_effort=ultra", "-a", "never"},
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	id := toolprobe.Identity{Provider: d.Provider, Model: d.Model, Harness: "pi", Recipe: toolprobe.RecipeArtifactWrite, Toolchain: toolprobe.ToolchainV1}
	r, err := toolprobe.NewReceipt(id, toolprobe.StatusPASS, "", "sha256:x", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	opened := 0
	_, _, _, err = Open(tabOpenerFunc(func(string, string, string, bool, ...string) (string, string, error) {
		opened++
		return "t", "p", nil
	}), BoundarySpec{
		Decision: d, Request: Request{Decision: d}, Probe: &r,
		Workspace: "w", Label: "x", Cwd: "/tmp/x",
		Now: func() time.Time { return now },
	})
	if err == nil {
		t.Fatal("forged decision must fail before tab")
	}
	if opened != 0 {
		t.Fatal("tab must not open")
	}
	if !errors.Is(err, err) && err == nil {
		t.Fatal("unreachable")
	}
}
