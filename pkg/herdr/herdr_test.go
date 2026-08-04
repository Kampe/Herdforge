package herdr

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
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

func TestProductionRecoveryRebindsDurableProvisionalByExactPaneProcess(t *testing.T) {
	owner := toolchild.Identity{TabID: "tab-recover", PaneID: "pane-recover", Name: "worker", Provider: "codex", Repository: "repo-recover", TaskRef: "FAC-188", Role: "worker", Lane: "lane", SessionGeneration: 9, LaunchID: "launch", ArgvDigest: "digest", Argv: []string{"codex", "--model", "gpt-5.6-luna", "-c", "model_reasoning_effort=medium"}}
	lc := toolchild.NewLifecycle(owner, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","agent":"codex","pane_id":"pane-recover","tab_id":"tab-recover","agent_session":{"value":"session-recover"}}]}}`, nil
		}
		return `{"result":{"process_info":{"foreground_processes":[{"pid":501,"name":"codex","cwd":"/repo","argv":["codex","--model","gpt-5.6-luna","-c","model_reasoning_effort=medium"]},{"pid":500,"name":"node","cwd":"/repo","argv":["node","/opt/codex"]}]}}}`, nil
	}
	oldToken, oldParent := readPIDStartToken, readPIDParent
	defer func() { readPIDStartToken, readPIDParent = oldToken, oldParent }()
	readPIDStartToken = func(pid int) (string, error) { return fmt.Sprintf("start-%d", pid), nil }
	readPIDParent = func(pid int) (int, error) {
		if pid == 501 {
			return 500, nil
		}
		return 1, nil
	}
	if err := bindRecoveredToolChildLifecycle(lc); err != nil {
		t.Fatal(err)
	}
	if !lc.Bound() || lc.Inventory.Owner.PID != 501 || lc.Inventory.Owner.StartToken != "start-501" || lc.Inventory.Owner.SessionID != "session-recover" {
		t.Fatalf("recovered owner was not exact: %+v", lc.Inventory.Owner)
	}
}

func TestPaneProcessInfoAcceptsOnlyTypedPaneNotFound(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 4 {
			return `{"error":{"code":"pane_not_found","message":"pane no longer exists"}}`, errors.New("exit status 1")
		}
		return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
	}
	if _, err := paneProcesses("old-pane"); !errors.Is(err, ErrPaneNotFound) {
		t.Fatalf("typed absence = %v", err)
	}
	runHerdr = func(args ...string) (string, error) {
		return `{"error":{"code":"transport_failed"}}`, errors.New("exit status 1")
	}
	if _, err := paneProcesses("old-pane"); errors.Is(err, ErrPaneNotFound) || err == nil {
		t.Fatalf("arbitrary failure accepted: %v", err)
	}
}

func TestVerifyHerdrTerminalRequiresExactTabAndAgentAbsence(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "tab" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) == 2 && args[0] == "agent" {
			return `{"result":{"agents":[]}}`, nil
		}
		return `{"error":{"code":"pane_not_found"}}`, errors.New("exit status 1")
	}
	if err := verifyHerdrTerminal("tab-old", "pane-old"); err != nil {
		t.Fatal(err)
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
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		return "{}", nil
	}
	if err := AgentStart("raw", "codex", "pane", "--model", launch.WorkerModel); err == nil {
		t.Fatal("raw/bare AgentStart must fail closed")
	}
	if len(calls) != 0 {
		t.Fatalf("raw rejection invoked process API: %v", calls)
	}
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: "FAC-188", Repository: "repo", Lane: "worker", SessionGeneration: 1, Scope: router.ScopeTask, LeaseGeneration: d.LeaseGeneration}
	if err := AgentStartWithDecision("worker", "codex", "pane", req); err == nil {
		t.Fatal("direct worker start without a prepared lifecycle must fail closed")
	}
	if len(calls) != 0 {
		t.Fatalf("unprepared start reached process API: %v", calls)
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
	req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, Scope: d.Scope, Repository: "herdforge-test", Lane: "worker"}
	owner := toolchild.Identity{PID: 501, StartToken: "agent-start", SessionGeneration: req.LeaseGeneration, LaunchID: launch.DecisionDigest(d), Repository: "herdforge-test", Role: launch.WorkerRole, Lane: "worker", SessionID: "session-1", PaneID: "pane-1", TabID: "tab-1", Provider: launch.WorkerProvider, ArgvDigest: launch.DecisionDigest(d)}
	tree := &toolchild.FakeTree{Nodes: map[int]toolchild.Node{501: {Identity: owner}, 601: {Identity: toolchild.Identity{PID: 601, ParentPID: 501, StartToken: "child-start"}, ParentPID: 501}}}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, tree, &toolchild.MemorySink{})
	restoreFactory := SetToolChildLifecycleFactory(func(launch.Request, string, string) (ToolChildLifecycle, error) { return lc, nil })
	defer restoreFactory()
	calledInfo := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
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
	defer dropToolChild("tab-1", "pane-1")
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

func TestStartPreparedValidationFailureCompensatesProvisionedAuthority(t *testing.T) {
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/receipts.jsonl"
	lc := toolchild.NewLifecycle(toolchild.Identity{}, &toolchild.FakeTree{}, &toolchild.JSONLSink{Path: path})
	restore := SetToolChildLifecycleFactory(func(launch.Request, string, string) (ToolChildLifecycle, error) { return lc, nil })
	defer restore()
	old := runHerdr
	defer func() { runHerdr = old }()
	list := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			list++
			if list == 1 {
				return `{"result":{"agents":[{"name":"worker","agent":"codex","pane_id":"pane","tab_id":"tab","agent_session":{"value":"session"}}]}}`, nil
			}
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
			return `{}`, nil
		}
		if len(args) == 4 && args[0] == "pane" {
			return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
		}
		return `{}`, nil
	}
	req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, Scope: d.Scope, Repository: "repo", Lane: "worker"}
	if err := StartPreparedAgent("tab", "worker", "wrong-provider", "pane", req); err == nil {
		t.Fatal("provider validation unexpectedly passed")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if lc.Bound() {
		t.Fatal("failed pre-bind compensation retained a bound owner")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

type rollbackLifecycle struct {
	bound                                            bool
	events                                           *[]string
	beginErr, reconcileErr, invalidateErr, verifyErr error
}

type failingProvisionSink struct{}

func (failingProvisionSink) Write(toolchild.Receipt) error {
	return errors.New("injected provisional sink failure")
}

func TestProvisioningFailureClosesExactTabWithoutPublishingReservation(t *testing.T) {
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	oldFactory := newToolChildLifecycle
	defer func() { newToolChildLifecycle = oldFactory }()
	closed := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
			closed = true
			return `{}`, nil
		}
		if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) == 4 && args[0] == "pane" {
			return `{"error":{"code":"pane_not_found"}}`, errors.New("exit status 1")
		}
		return `{}`, nil
	}
	newToolChildLifecycle = func(req launch.Request, _ string, _ string) (ToolChildLifecycle, error) {
		return toolchild.NewLifecycle(toolchild.Identity{}, toolchild.SystemTree{}, failingProvisionSink{}), nil
	}
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: "FAC-188", Repository: "repo", Lane: "worker", SessionGeneration: 42, LeaseGeneration: 7, Scope: router.ScopeTask}
	if err := StartPreparedAgent("tab-provision-fail", "worker", "codex", "pane-provision-fail", req); err == nil {
		t.Fatal("provisioning failure was accepted")
	}
	if !closed {
		t.Fatal("exact prepared tab was not closed")
	}
	if lifecycleForTab("tab-provision-fail") != nil || lifecycleForPane("pane-provision-fail") != nil {
		t.Fatal("failed provisioning leaked map reservation")
	}
}

func TestNameOnlyCompensationRejectsAmbiguousAgents(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	closeCalls := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"p1","tab_id":"t1"},{"name":"worker","pane_id":"p2","tab_id":"t2"}]}}`, nil
		}
		if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
			closeCalls++
			return `{}`, nil
		}
		return `{}`, nil
	}
	if err := compensateStartedProcess("worker"); err == nil {
		t.Fatal("ambiguous name-only compensation was accepted")
	}
	if closeCalls != 0 {
		t.Fatal("ambiguous compensation closed a tab")
	}
}

func (l *rollbackLifecycle) Bind(toolchild.Identity) error { l.bound = true; return nil }
func (l *rollbackLifecycle) Bound() bool                   { return l.bound }
func (l *rollbackLifecycle) Begin() error                  { return l.beginErr }
func (l *rollbackLifecycle) Reconcile(string) error {
	*l.events = append(*l.events, "reconcile")
	return l.reconcileErr
}
func (l *rollbackLifecycle) Invalidate(string) error {
	*l.events = append(*l.events, "tombstone")
	return l.invalidateErr
}
func (l *rollbackLifecycle) VerifyTerminal() error {
	*l.events = append(*l.events, "terminal-readback")
	return l.verifyErr
}

func TestRollbackCrashMatrixRetainsAuthorityUntilTerminalTombstone(t *testing.T) {
	cases := []struct {
		name                                             string
		bound                                            bool
		reconcileErr, closeErr, invalidateErr, verifyErr error
		keepLive                                         bool
		wantErr, wantRetained                            bool
	}{
		{name: "after-herdr-start"},
		{name: "after-bind-failure"},
		{name: "after-begin-failure", bound: true, reconcileErr: errors.New("begin failure"), wantErr: true, wantRetained: true},
		{name: "after-launch-receipt-failure", bound: true},
		{name: "after-child-reconcile", bound: true, reconcileErr: errors.New("child reconcile crash"), wantErr: true, wantRetained: true},
		{name: "before-tab-close", closeErr: errors.New("close crash"), wantErr: true, wantRetained: true},
		{name: "after-close-before-readback", keepLive: true, wantErr: true, wantRetained: true},
		{name: "after-terminal-readback-before-tombstone", invalidateErr: errors.New("tombstone crash"), wantErr: true, wantRetained: true},
		{name: "after-tombstone", verifyErr: errors.New("terminal readback crash"), wantErr: true, wantRetained: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := runHerdr
			defer func() { runHerdr = old }()
			var events []string
			lc := &rollbackLifecycle{bound: tc.bound, events: &events, reconcileErr: tc.reconcileErr, invalidateErr: tc.invalidateErr, verifyErr: tc.verifyErr}
			toolChildMu.Lock()
			toolChildByPane["pane-crash"] = lc
			toolChildByTab["tab-crash"] = lc
			toolChildMu.Unlock()
			defer dropToolChild("tab-crash", "pane-crash")
			listCalls := 0
			runHerdr = func(args ...string) (string, error) {
				if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
					return `{"result":{"tabs":[]}}`, nil
				}
				if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
					return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
				}
				if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
					listCalls++
					if tc.keepLive || listCalls == 1 {
						return `{"result":{"agents":[{"name":"worker","pane_id":"pane-crash","tab_id":"tab-crash"}]}}`, nil
					}
					return `{"result":{"agents":[]}}`, nil
				}
				if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
					if tc.closeErr != nil {
						return "", tc.closeErr
					}
					return `{}`, nil
				}
				return `{}`, nil
			}
			err := rollbackToolChild("tab-crash", "pane-crash", lc, tc.name)
			if tc.wantErr != (err != nil) {
				t.Fatalf("error=%v wantErr=%v events=%v", err, tc.wantErr, events)
			}
			toolChildMu.Lock()
			_, retained := toolChildByTab["tab-crash"]
			toolChildMu.Unlock()
			if tc.wantRetained != retained {
				t.Fatalf("retained=%v want=%v events=%v", retained, tc.wantRetained, events)
			}
			if !tc.wantErr && (len(events) == 0 || events[len(events)-1] != "terminal-readback") {
				t.Fatalf("terminal evidence missing: %v", events)
			}
		})
	}
}

// This table enters the real StartPreparedAgent -> AgentStartWithDecision ->
// bind/Begin/receipt/rollback path. Herdr, process identity and lifecycle are
// all fake adapters; no host process or signal is involved.
func TestProductionStartCrashMatrixRetainsAuthorityUntilReadback(t *testing.T) {
	cases := []struct {
		name                                                                          string
		startErr, bindErr, beginErr, reconcileErr, closeErr, invalidateErr, verifyErr bool
		keepLive                                                                      bool
	}{
		{name: "after-preparation-before-start", startErr: true},
		{name: "after-herdr-start-before-bind", bindErr: true},
		{name: "bind-failure", bindErr: true},
		{name: "begin-inventory-failure", beginErr: true},
		{name: "launch-receipt-failure"},
		{name: "child-reap-intent-result-failure", reconcileErr: true},
		{name: "before-raw-tab-close", closeErr: true},
		{name: "after-raw-tab-close-before-readback", keepLive: true},
		{name: "after-terminal-readback-before-tombstone", invalidateErr: true},
		{name: "after-tombstone-readback", verifyErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
			if err != nil {
				t.Fatal(err)
			}
			var events []string
			lc := &rollbackLifecycle{events: &events}
			if tc.beginErr {
				lc.beginErr = errors.New("injected Begin failure")
			}
			if tc.reconcileErr {
				lc.reconcileErr = errors.New("injected reap intent/result failure")
			}
			if tc.invalidateErr {
				lc.invalidateErr = errors.New("injected tombstone boundary failure")
			}
			if tc.verifyErr {
				lc.verifyErr = errors.New("injected terminal readback failure")
			}
			restoreFactory := SetToolChildLifecycleFactory(func(launch.Request, string, string) (ToolChildLifecycle, error) { return lc, nil })
			defer restoreFactory()
			if !tc.startErr {
				t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
			} else {
				t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
			}
			oldRun := runHerdr
			defer func() { runHerdr = oldRun }()
			defer dropToolChild("tab-crash-prod", "pane-crash-prod")
			defer SetPIDParentReader(func(int) (int, error) { return 500, nil })()
			defer SetPIDStartTokenReader(func(int) (string, error) { return "start-agent", nil })()
			listCalls := 0
			closeDone := false
			runHerdr = func(args ...string) (string, error) {
				if len(args) >= 2 && args[0] == "agent" && args[1] == "start" && tc.startErr {
					return "", errors.New("injected Herdr start failure")
				}
				if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
					listCalls++
					if tc.keepLive || listCalls == 1 {
						return `{"result":{"agents":[{"name":"worker","agent":"codex","pane_id":"pane-crash-prod","tab_id":"tab-crash-prod","agent_session":{"value":"session"}}]}}`, nil
					}
					return `{"result":{"agents":[]}}`, nil
				}
				if len(args) == 2 && args[0] == "tab" && args[1] == "list" {
					return `{"result":{"tabs":[]}}`, nil
				}
				if len(args) == 3 && args[0] == "tab" && args[1] == "close" {
					if tc.closeErr {
						return "", errors.New("injected tab close failure")
					}
					closeDone = true
					return `{}`, nil
				}
				if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
					if closeDone && !tc.keepLive {
						return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
					}
					if tc.bindErr {
						return `{"result":{"process_info":{"foreground_processes":[]}}}`, nil
					}
					native := append([]string{"/opt/homebrew/bin/codex"}, d.Argv[1:]...)
					return fmt.Sprintf(`{"result":{"process_info":{"foreground_processes":[{"pid":500,"name":"node","argv":["node","/opt/homebrew/bin/codex"],"cwd":"/repo"},{"pid":501,"name":"codex","argv":%s,"cwd":"/repo"}]}}}`, mustJSON(native)), nil
				}
				return `{}`, nil
			}
			req := launch.Request{Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration, SessionGeneration: 42, Scope: d.Scope, Repository: "repo", Lane: "worker"}
			if tc.name == "launch-receipt-failure" || tc.reconcileErr || tc.closeErr || tc.keepLive || tc.invalidateErr || tc.verifyErr {
				t.Setenv("HERD_LAUNCH_RECEIPTS", "/dev/null/receipts.jsonl")
			}
			err = StartPreparedAgent("tab-crash-prod", "worker", "codex", "pane-crash-prod", req)
			if err == nil {
				t.Fatal("crash boundary unexpectedly succeeded")
			}
			if tc.reconcileErr || tc.closeErr || tc.keepLive || tc.invalidateErr || tc.verifyErr {
				if lifecycleForTab("tab-crash-prod") == nil {
					t.Fatalf("authority dropped before verified terminal state: %v", events)
				}
			}
			if tc.invalidateErr || tc.verifyErr || tc.keepLive || tc.closeErr || tc.reconcileErr {
				if len(events) == 0 {
					t.Fatalf("authority events missing: %v", events)
				}
			}
		})
	}
}

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
			err := AgentStartWithDecision("worker", launch.WorkerProvider, "pane", launch.Request{Decision: d, TaskRef: "FAC-178", Repository: "repo", Lane: "worker", SessionGeneration: 42, LeaseGeneration: generation, Scope: router.ScopeTask})
			if err == nil {
				t.Fatal("zero or mismatched generation must fail before process seam")
			}
			if len(calls) != before {
				t.Fatalf("rejected generation reached process seam: %v", calls[before:])
			}
		})
	}
	if err := AgentStartWithDecision("worker", launch.WorkerProvider, "pane", launch.Request{Decision: d, TaskRef: "FAC-178", Repository: "repo", Lane: "worker", SessionGeneration: 42, LeaseGeneration: 7, Scope: router.ScopeTask}); err == nil {
		t.Fatal("exact generation without prepared lifecycle must still fail closed")
	}
	if len(calls) != 0 {
		t.Fatalf("unprepared exact-generation start reached process seam: %v", calls)
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
	if got, err := ResolveAgentTabWithDecision("standing-worker", launch.Request{Decision: d, TaskRef: "FAC-175", LeaseGeneration: d.LeaseGeneration, Scope: router.ScopeTask}); err == nil || got != "" {
		t.Fatalf("unbound provisional resume must fail closed: %q %v", got, err)
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

func standingResumeFixture(t *testing.T, durable bool) (launch.Request, *toolchild.Lifecycle, AgentEntry, string) {
	t.Helper()
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/launch.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role: router.RoleWorker, Shape: launch.Implementation, TaskRef: "FAC-188",
		LeaseGeneration: 7, RequestedProvider: launch.WorkerProvider,
		RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort,
		Scope:        router.ScopeTask,
		ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := launch.Request{Decision: d, TaskRef: "FAC-188", Name: "forge-worker", PaneID: "pane-standing", LeaseGeneration: 7, Scope: router.ScopeTask, Repository: "repo-standing", Lane: "worker"}
	digest := launch.DecisionDigest(d)
	owner := toolchild.Identity{PID: 900, StartToken: "owner-start", SessionGeneration: 42, LaunchID: digest, Repository: req.Repository, Role: string(d.Role), Lane: req.Lane, SessionID: "herdr-session", PaneID: req.PaneID, TabID: "tab-standing", Provider: d.Provider, ArgvDigest: digest, Argv: append([]string(nil), d.Argv...), TaskRef: req.TaskRef, Name: req.Name}
	lc := toolchild.NewLifecycle(owner, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	path := t.TempDir() + "/toolchild.jsonl"
	if durable {
		sink := &toolchild.JSONLSink{Path: path}
		if err := sink.Write(toolchild.Receipt{Action: "owner", Identity: owner, Reason: "exact launch owner bound"}); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HERD_TOOLCHILD_RECEIPTS", path)
	} else {
		toolChildMu.Lock()
		toolChildByTab[owner.TabID] = lc
		toolChildByPane[owner.PaneID] = lc
		toolChildMu.Unlock()
		t.Cleanup(func() { dropToolChild(owner.TabID, owner.PaneID) })
	}
	started := req
	started.SessionGeneration = owner.SessionGeneration
	if err := launch.RecordStarted(started, nil); err != nil {
		t.Fatal(err)
	}
	agent := AgentEntry{Name: owner.Name, Kind: owner.Provider, Status: "working", PaneID: owner.PaneID, TabID: owner.TabID}
	agent.Session.Value = owner.SessionID
	req.SessionGeneration = 0
	return req, lc, agent, path
}

func TestStandingResumeRecoversGenerationFromTaskLaunchRequestShape(t *testing.T) {
	req, lc, agent, _ := standingResumeFixture(t, false)
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	var calls [][]string
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			b, _ := json.Marshal(struct {
				Result struct {
					Agents []AgentEntry `json:"agents"`
				} `json:"result"`
			}{Result: struct {
				Agents []AgentEntry `json:"agents"`
			}{Agents: []AgentEntry{agent}}})
			return string(b), nil
		}
		return "", fmt.Errorf("unexpected process or tab side effect: %v", args)
	}
	if _, err := ResolveAgentTabWithDecision(agent.Name, req); err != nil {
		t.Fatal(err)
	}
	if lc.Inventory.Owner.SessionGeneration != 42 || len(calls) != 1 {
		t.Fatalf("standing lane was not reused exactly: generation=%d calls=%v", lc.Inventory.Owner.SessionGeneration, calls)
	}
}

func TestStandingResumeRecoversGenerationAfterCoordinatorRestart(t *testing.T) {
	req, _, agent, path := standingResumeFixture(t, true)
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	toolChildMu.Lock()
	toolChildByTab = map[string]ToolChildLifecycle{}
	toolChildByPane = map[string]ToolChildLifecycle{}
	toolChildMu.Unlock()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return fmt.Sprintf(`{"result":{"agents":[{"name":%q,"agent":%q,"agent_status":"working","pane_id":%q,"tab_id":%q,"agent_session":{"value":%q}}]}}`, agent.Name, agent.Kind, agent.PaneID, agent.TabID, agent.Session.Value), nil
		}
		return "", fmt.Errorf("unexpected process or tab side effect: %v", args)
	}
	if _, err := ResolveAgentTabWithDecision(agent.Name, req); err != nil {
		t.Fatalf("restart recovery from %s failed: %v", path, err)
	}
	if lifecycleForTab(agent.TabID) == nil {
		t.Fatal("restart recovery did not retain exact lifecycle authority")
	}
}

func TestStandingResumeRejectsLifecycleTupleMismatches(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*toolchild.Identity, *AgentEntry, *launch.Request)
	}{
		{name: "repository", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.Repository = "other" }},
		{name: "task", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.TaskRef = "FAC-other" }},
		{name: "lane", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.Lane = "other" }},
		{name: "provider", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.Provider = "other" }},
		{name: "digest", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.LaunchID = "other" }},
		{name: "argv", mutate: func(o *toolchild.Identity, _ *AgentEntry, _ *launch.Request) { o.Argv = []string{"codex", "mutated"} }},
		{name: "session", mutate: func(_ *toolchild.Identity, a *AgentEntry, _ *launch.Request) { a.Session.Value = "other" }},
		{name: "pane", mutate: func(o *toolchild.Identity, a *AgentEntry, _ *launch.Request) { o.PaneID, a.PaneID = "other", "other" }},
		{name: "generation", mutate: func(o *toolchild.Identity, _ *AgentEntry, r *launch.Request) {
			r.SessionGeneration = o.SessionGeneration + 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, lc, agent, _ := standingResumeFixture(t, false)
			tc.mutate(&lc.Inventory.Owner, &agent, &req)
			oldRun := runHerdr
			defer func() { runHerdr = oldRun }()
			runHerdr = func(args ...string) (string, error) {
				if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
					return fmt.Sprintf(`{"result":{"agents":[{"name":%q,"agent":%q,"agent_status":"working","pane_id":%q,"tab_id":%q,"agent_session":{"value":%q}}]}}`, agent.Name, agent.Kind, agent.PaneID, agent.TabID, agent.Session.Value), nil
				}
				return "", fmt.Errorf("unexpected side effect: %v", args)
			}
			if _, err := ResolveAgentTabWithDecision(agent.Name, req); !errors.Is(err, ErrAgentIdentityMismatch) {
				t.Fatalf("mismatch %s was not fail-closed: %v", tc.name, err)
			}
		})
	}
}

func TestReceiptFailureClosesAndVerifiesExactTab(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", "/dev/null/launch-receipts.jsonl")
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: launch.Implementation, RequestedProvider: launch.WorkerProvider, RequestedModel: launch.WorkerModel, RequestedEffort: launch.WorkerEffort, TaskRef: "FAC-188", LeaseGeneration: 7, Scope: router.ScopeTask, ProbeResults: map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true}})
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
	if err := AgentStartWithDecision("worker", "codex", "pane", launch.Request{Decision: d, TaskRef: "FAC-188", Repository: "repo", Lane: "worker", SessionGeneration: 42, LeaseGeneration: 7, Scope: router.ScopeTask}); err == nil || !strings.Contains(err.Error(), "prepared tool-child lifecycle") {
		t.Fatalf("unprepared receipt boundary must fail before process API: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("unprepared receipt path reached process API: %v", calls)
	}
}
