package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolchild"
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

func TestIsAvailable(t *testing.T) {
	// On this machine, herdr should be installed
	available := IsAvailable()
	if !available {
		t.Log("herdr not found in PATH — available=false is expected on CI")
	}
}

func TestAgentListDecodesExactSessionValue(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) != 2 || args[0] != "agent" || args[1] != "list" {
			t.Fatalf("unexpected args: %v", args)
		}
		return `{"result":{"agents":[{"name":"forge-worker","agent":"gpt-5.6-luna","agent_status":"working","pane_id":"pane-1","tab_id":"tab-1","workspace_id":"wF","agent_session":{"value":"session-actual"}}]}}`, nil
	}
	agents, err := AgentList()
	if err != nil || len(agents) != 1 {
		t.Fatalf("AgentList: %#v %v", agents, err)
	}
	if agents[0].Session.Value != "session-actual" {
		t.Fatalf("session=%q", agents[0].Session.Value)
	}
}

func TestHerdrCLINotFound(t *testing.T) {
	// Verify that the error path works when herdr is missing
	// by temporarily modifying PATH
	t.Setenv("PATH", "/dev/null")

	available := IsAvailable()
	if available {
		t.Skip("herdr still found in PATH despite override")
	}
}

func TestEnsureHerdforgeLabel_Prefixes(t *testing.T) {
	got := EnsureHerdforgeLabel("worker")
	want := "Herdforge · worker"
	if got != want {
		t.Errorf("EnsureHerdforgeLabel(\"worker\") = %q, want %q", got, want)
	}
}

func TestEnsureHerdforgeLabel_AlreadyPrefixed(t *testing.T) {
	got := EnsureHerdforgeLabel("Herdforge · worker")
	if got != "Herdforge · worker" {
		t.Errorf("already-prefixed label was modified: %q", got)
	}
}

func TestEnsureHerdforgeLabel_PrefixWithSuffix(t *testing.T) {
	// Already starts with the prefix; extra suffix must not re-prefix.
	got := EnsureHerdforgeLabel("Herdforge · worker (FAC-141)")
	if got != "Herdforge · worker (FAC-141)" {
		t.Errorf("label starting with prefix was modified: %q", got)
	}
}

func TestEnsureHerdforgeLabel_MidStringStillPrefixed(t *testing.T) {
	// Non-vacuous HasPrefix contract: mid-string "Herdforge · " must NOT
	// count as already-prefixed. Mutation of HasPrefix→Contains fails this.
	in := "review of Herdforge · thing"
	got := EnsureHerdforgeLabel(in)
	want := "Herdforge · review of Herdforge · thing"
	if got != want {
		t.Errorf("EnsureHerdforgeLabel(%q) = %q, want %q", in, got, want)
	}
}

func TestAgentStartBoundaryRejectsRawAndRequiresDecision(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "{}", nil
	}
	if err := AgentStart("raw", "codex", "pane", "--model", launch.WorkerModel); err == nil {
		t.Fatal("raw/bare AgentStart must fail closed")
	}
	if len(calls) != 0 {
		t.Fatalf("raw rejection invoked process API: %v", calls)
	}
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d}
	if err := AgentStartWithDecision("worker", "codex", "pane", req); err != nil {
		t.Fatal(err)
	}
	want := []string{"agent", "start", "worker", "--kind", "codex", "--pane", "pane", "--", "--model", launch.WorkerModel, "-c", "model_reasoning_effort=medium", "-a", "never", "-c", "mcp_servers.code-review-graph.enabled=false"}
	if !reflect.DeepEqual(calls, [][]string{want}) {
		t.Fatalf("argv calls = %v, want %v", calls, [][]string{want})
	}
}

func TestPreparedStartUsesPaneProcessInfoAndExactRoutedOwner(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/launch.jsonl")
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	defer SetPIDParentReader(func(int) (int, error) { return 500, nil })()
	defer SetPIDStartTokenReader(func(int) (string, error) { return "agent-start", nil })()
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, Scope: d.Scope}
	owner := toolchild.Identity{PID: 501, StartToken: "agent-start", SessionGeneration: req.LeaseGeneration, LaunchID: launch.DecisionDigest(d), Repository: "herdforge", Role: launch.WorkerRole, Lane: "worker", SessionID: "session-1", PaneID: "pane-1", Provider: launch.WorkerProvider, ArgvDigest: launch.DecisionDigest(d)}
	tree := &toolchild.FakeTree{Nodes: map[int]toolchild.Node{501: {Identity: owner}, 601: {Identity: toolchild.Identity{PID: 601, ParentPID: 501, StartToken: "child-start"}, ParentPID: 501}}}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, tree, &toolchild.MemorySink{})
	restoreFactory := SetToolChildLifecycleFactory(func(launch.Request, string, string) (ToolChildLifecycle, error) { return lc, nil })
	defer restoreFactory()
	calledInfo := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "start" {
			return `{}`, nil
		}
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","agent":"codex","agent_status":"working","pane_id":"pane-1","tab_id":"tab-1","agent_session":{"value":"session-1"}}]}}`, nil
		}
		if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
			calledInfo = true
			native := append([]string{"/opt/homebrew/bin/codex"}, d.Argv[1:]...)
			return fmt.Sprintf(`{"result":{"process_info":{"foreground_processes":[{"pid":500,"name":"node","cwd":"/repo","argv":["node","/opt/homebrew/bin/codex"]},{"pid":501,"name":"codex","cwd":"/repo","argv":%s}]}}}`, mustJSON(native)), nil
		}
		return `{}`, nil
	}
	if err := StartPreparedAgent("tab-1", "worker", "codex", "pane-1", req); err != nil {
		t.Fatal(err)
	}
	if !calledInfo {
		t.Fatal("agent-list-only path did not query exact pane process-info")
	}
	if err := ReconcileToolChild("tab-1", "done"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(tree.Reaped) != 1 || tree.Reaped[0] != 601 {
		t.Fatalf("reaped=%v", tree.Reaped)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestAgentStartRequiresExactClaimGenerationBeforeProcess(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "{}", nil
	}
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel,
		RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-178", LeaseGeneration: 7,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, generation := range map[string]int64{"zero": 0, "mismatch": 6} {
		t.Run(name, func(t *testing.T) {
			before := len(calls)
			err := AgentStartWithDecision("worker", launch.WorkerProvider, "pane", launch.Request{Decision: d, TaskRef: "FAC-178", LeaseGeneration: generation, Scope: router.ScopeTask})
			if err == nil {
				t.Fatal("zero or mismatched generation must fail before process seam")
			}
			if len(calls) != before {
				t.Fatalf("rejected generation reached process seam: %v", calls[before:])
			}
		})
	}
	before := len(calls)
	if err := AgentStartWithDecision("worker", launch.WorkerProvider, "pane", launch.Request{Decision: d, TaskRef: "FAC-178", LeaseGeneration: 7, Scope: router.ScopeTask}); err != nil {
		t.Fatalf("exact generation must reach injected process seam: %v", err)
	}
	if len(calls) != before+1 {
		t.Fatalf("exact generation did not reach process seam: %v", calls)
	}
}

func TestResumeUsesDurableClientIdentityNotHerdrMetadata(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	d, err = router.RebindDecision(d, "FAC-175", 7)
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: "FAC-175", Name: "standing-worker", PaneID: "pane-1", LeaseGeneration: 7, Scope: router.ScopeTask}
	if err := launch.RecordStarted(req, nil); err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"standing-worker","pane_id":"pane-1","tab_id":"tab-1"}]}}`, nil
		}
		return "{}", nil
	}
	if got, err := ResolveAgentTabWithDecision("standing-worker", launch.Request{Decision: d, TaskRef: "FAC-175", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask}); err != nil || got != "standing-worker" {
		t.Fatalf("durable resume failed: %q %v", got, err)
	}
	if _, err := ResolveAgentTabWithDecision("standing-worker", launch.Request{Decision: d, TaskRef: "other", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask}); err == nil {
		t.Fatal("different task identity must fail closed before resume")
	}
	if _, err := ResolveAgentTabWithDecision("missing", launch.Request{Decision: d, TaskRef: "FAC-175", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("expected typed not-found, got %v", err)
	}
}

func TestStandingReceiptCannotAuthorizeClaimedTaskAssignment(t *testing.T) {
	receiptPath := t.TempDir() + "/receipts.jsonl"
	t.Setenv("HERD_LAUNCH_RECEIPTS", receiptPath)
	standing, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation,
		RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel,
		RequestedEffort: launch.WorkerEffort, TaskRef: "worker",
		Scope:        router.ScopeLane,
		ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launch.RecordStarted(launch.Request{Decision: standing, TaskRef: "worker", Name: "forge-worker", PaneID: "pane-1", LeaseGeneration: 0}, nil); err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return `{"result":{"agents":[{"name":"forge-worker","pane_id":"pane-1","tab_id":"tab-1"}]}}`, nil
	}
	for _, tc := range []struct {
		name string
		ref  string
		gen  int64
	}{
		{name: "FAC-A", ref: "FAC-A", gen: 7},
		{name: "FAC-B", ref: "FAC-B", gen: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision, err := router.RebindDecision(standing, tc.ref, tc.gen)
			if err != nil {
				t.Fatal(err)
			}
			before := len(calls)
			_, err = ResolveAgentTabWithDecision("forge-worker", launch.Request{Decision: decision, TaskRef: tc.ref, LeaseGeneration: tc.gen, Scope: router.ScopeTask})
			if !errors.Is(err, ErrAgentIdentityMismatch) {
				t.Fatalf("lane receipt must not authorize %s/g%d: %v", tc.ref, tc.gen, err)
			}
			if len(calls) != before+1 || calls[before][0] != "agent" || calls[before][1] != "list" {
				t.Fatalf("assignment rejection must inspect only the live agent: %v", calls[before:])
			}
		})
	}
}

func TestResumeRejectsStoredCoordinatorTierDecisionWithoutPrompt(t *testing.T) {
	receiptPath := t.TempDir() + "/receipts.jsonl"
	t.Setenv("HERD_LAUNCH_RECEIPTS", receiptPath)
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	d, err = router.RebindDecision(d, "FAC-175", 7)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := &router.LaunchDecision{Role: router.RoleWorker, Shape: launch.Implementation, Provider: launch.WorkerProvider, Model: "gpt-5.6-sol", Effort: "ultra", Argv: []string{"codex", "--model", "gpt-5.6-sol", "-c", "model_reasoning_effort=ultra", "-a", "never"}}
	historical := launch.Receipt{TaskRef: "FAC-175", Role: launch.WorkerRole, TaskShape: launch.Implementation, Provider: launch.WorkerProvider, Model: forbidden.Model, Effort: forbidden.Effort, DecisionDigest: launch.DecisionDigest(forbidden), Argv: forbidden.Argv, Accepted: true, Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: 7}
	if err := (&launch.JSONLSink{Path: receiptPath}).Write(historical); err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return `{"result":{"agents":[{"name":"stored-worker","pane_id":"pane-1","tab_id":"tab-1"}]}}`, nil
	}
	_, err = ResolveAgentTabWithDecision("stored-worker", launch.Request{Decision: d, TaskRef: "FAC-175", Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask})
	if !errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("stored Sol/Ultra session must be blocked by durable identity mismatch, got %v", err)
	}
	if len(calls) != 1 || calls[0][0] != "agent" || calls[0][1] != "list" {
		t.Fatalf("blocked resume must only inspect the live agent: %v", calls)
	}
}

func TestResumePreservesMalformedCurrentDecisionError(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	d, err = router.RebindDecision(d, "FAC-175", 7)
	if err != nil {
		t.Fatal(err)
	}
	d.Model = "gpt-5.6-sol"
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls int
	runHerdr = func(args ...string) (string, error) {
		calls++
		return `{}`, nil
	}
	_, err = ResolveAgentTabWithDecision("stored-worker", launch.Request{Decision: d, TaskRef: "FAC-175", Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask})
	if err == nil || errors.Is(err, ErrAgentIdentityMismatch) {
		t.Fatalf("malformed current decision must preserve validation error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("malformed current decision must not inspect a live agent: %d calls", calls)
	}
}

func TestResumeRejectsMissingAndStaleReceiptsWithoutProcessOrPrompt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		populate func(string) error
	}{
		{name: "missing", populate: func(string) error { return nil }},
		{name: "lease-mismatch", populate: func(path string) error {
			d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
			if err != nil {
				return err
			}
			d, err = router.RebindDecision(d, "FAC-175", 6)
			if err != nil {
				return err
			}
			return (&launch.JSONLSink{Path: path}).Write(launch.Receipt{TaskRef: "FAC-175", Role: launch.WorkerRole, TaskShape: launch.Implementation, Provider: launch.WorkerProvider, Model: launch.WorkerModel, Effort: launch.WorkerEffort, DecisionDigest: launch.DecisionDigest(d), Argv: d.Argv, Accepted: true, Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: 6})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/receipts.jsonl"
			t.Setenv("HERD_LAUNCH_RECEIPTS", path)
			if err := tc.populate(path); err != nil {
				t.Fatal(err)
			}
			d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
			if err != nil {
				t.Fatal(err)
			}
			d, err = router.RebindDecision(d, "FAC-175", 7)
			if err != nil {
				t.Fatal(err)
			}
			old := runHerdr
			defer func() { runHerdr = old }()
			var calls [][]string
			runHerdr = func(args ...string) (string, error) {
				calls = append(calls, append([]string(nil), args...))
				return `{"result":{"agents":[{"name":"stored-worker","pane_id":"pane-1","tab_id":"tab-1"}]}}`, nil
			}
			_, err = ResolveAgentTabWithDecision("stored-worker", launch.Request{Decision: d, TaskRef: "FAC-175", Name: "stored-worker", PaneID: "pane-1", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask})
			if !errors.Is(err, ErrAgentIdentityMismatch) {
				t.Fatalf("%s receipt must be blocked, got %v", tc.name, err)
			}
			if len(calls) != 1 || calls[0][0] != "agent" || calls[0][1] != "list" {
				t.Fatalf("%s resume must not start or prompt: %v", tc.name, calls)
			}
		})
	}
}

func TestReceiptFailureClosesAndVerifiesExactTab(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", "/dev/null/launch-receipts.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	old := runHerdr
	defer func() { runHerdr = old }()
	listCalls := 0
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			listCalls++
			if listCalls == 1 {
				return `{"result":{"agents":[{"name":"worker","pane_id":"pane","tab_id":"tab"}]}}`, nil
			}
			return `{"result":{"agents":[]}}`, nil
		}
		return "{}", nil
	}
	if err := AgentStartWithDecision("worker", "codex", "pane", launch.Request{Decision: d}); err == nil || !strings.Contains(err.Error(), "process stopped") {
		t.Fatalf("receipt failure must be hard and compensated: %v", err)
	}
	if len(calls) < 4 || !reflect.DeepEqual(calls[2], []string{"tab", "close", "tab"}) {
		t.Fatalf("cleanup calls = %v", calls)
	}
}
