package herdr

import (
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// TestAgentReadyForToolChildBind_RequiresSessionPath locks caf589a2: an agent
// row without agent_session.value is NOT ready, because that value is the Pi
// session path routedProcessCandidates attests. Tab-only readiness (b81bf2e)
// was superseded — see TestBindToolChildLifecycle_EmptySessionWaitsNotAborts
// for the failure it reintroduces.
func TestAgentReadyForToolChildBind_RequiresSessionPath(t *testing.T) {
	ready := AgentEntry{
		Name:   "task-fac-x",
		Kind:   router.PiHarness,
		Status: "idle",
		TabID:  "wF:t1",
		PaneID: "wF:p1",
	}
	ready.Session.Value = "/sessions/signed.jsonl"
	if !AgentReadyForToolChildBind(ready) {
		t.Fatal("agent with tab and session path must be ready for tool-child bind")
	}
	noTab := ready
	noTab.TabID = ""
	if AgentReadyForToolChildBind(noTab) {
		t.Fatal("empty TabID must not be ready")
	}
	noSession := ready
	noSession.Session.Value = ""
	if AgentReadyForToolChildBind(noSession) {
		t.Fatal("empty agent_session.value must not be ready: it is the Pi session path")
	}
}

// TestBindToolChildLifecycle_EmptySessionWaitsNotAborts drives the real poll
// loop through the startup window caf589a2 protects: the pi process is already
// listed in the pane inventory but agent_session.value has not landed yet.
//
// The loop must keep WAITING (and time out reporting session_present=false).
// With a tab-only readiness gate it instead reaches routedProcessCandidates
// with sessionPath="", openSecurePiSession rejects the empty path with
// ErrPiSessionRouteMismatch, and the loop hard-returns that error immediately —
// a retryable wait turned into a launch abort.
func TestBindToolChildLifecycle_EmptySessionWaitsNotAborts(t *testing.T) {
	defer SetOwnerBindTimingForTest(30*time.Millisecond, time.Millisecond)()
	d, err := testLaunchRouter(t).Decide(router.LaunchRequest{
		Role:              router.RoleWorker,
		Shape:             launch.Implementation,
		RequestedProvider: launch.WorkerProvider,
		RequestedModel:    launch.WorkerModel,
		RequestedEffort:   launch.WorkerEffort,
		TaskRef:           "FAC-133",
		LeaseGeneration:   7,
		Scope:             router.ScopeTask,
		ProbeResults:      map[string]bool{router.ProbeKey(launch.WorkerProvider, launch.WorkerModel): true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Harness != router.PiHarness {
		t.Fatalf("fixture must route through pi, got %q", d.Harness)
	}
	req := launch.Request{
		Decision: d, TaskRef: d.TaskRef, LeaseGeneration: d.LeaseGeneration,
		SessionGeneration: 1, Scope: d.Scope, Repository: "repo", Lane: "worker",
	}
	lc := toolchild.NewLifecycle(toolchild.Identity{}, &toolchild.FakeTree{}, &toolchild.MemorySink{})
	toolChildMu.Lock()
	toolChildByPane["pane-nosession"] = lc
	toolChildByTab["tab-nosession"] = lc
	toolChildMu.Unlock()
	defer dropToolChild("tab-nosession", "pane-nosession")

	// The pane already shows the exact routed pi process; only the session
	// value is missing. Argv must match d.HarnessArgv so the pi branch of
	// routedProcessCandidates is genuinely reachable — otherwise this test
	// would pass for the wrong reason.
	argv := strings.Join(quoteArgvForJSON(d.HarnessArgv), ",")
	oldRun := runHerdr
	defer func() { runHerdr = oldRun }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker-nosession","agent":"pi","agent_status":"starting","pane_id":"pane-nosession","tab_id":"tab-nosession","agent_session":{"value":""}}]}}`, nil
		}
		if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"foreground_processes":[{"pid":777,"name":"pi","argv":[` + argv + `]}]}}}`, nil
		}
		return `{}`, nil
	}

	err = bindToolChildLifecycle("pane-nosession", "worker-nosession", req)
	if err == nil {
		t.Fatal("expected the bounded wait to time out, got nil")
	}
	if strings.Contains(err.Error(), ErrPiSessionRouteMismatch.Error()) {
		t.Fatalf("empty session aborted the launch instead of waiting: %v", err)
	}
	if !strings.Contains(err.Error(), "session_present=false") {
		t.Fatalf("timeout must report the missing session as the wait reason, got: %v", err)
	}
}

func quoteArgvForJSON(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		out = append(out, `"`+strings.ReplaceAll(a, `"`, `\"`)+`"`)
	}
	return out
}
