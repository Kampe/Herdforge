package herdr

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestStatusFromList(t *testing.T) {
	agents := []AgentEntry{
		{Name: "forge-worker", PaneID: "w3:p3", Status: "working"},
		{Name: "", PaneID: "w3:p9", Status: "idle"},
	}
	if got := StatusFromList(agents, "w3:p3"); got != "working" {
		t.Errorf("by pane: got %q", got)
	}
	if got := StatusFromList(agents, "forge-worker"); got != "working" {
		t.Errorf("by name: got %q", got)
	}
	if got := StatusFromList(agents, "w3:p9"); got != "idle" {
		t.Errorf("unnamed pane: got %q", got)
	}
	if got := StatusFromList(agents, "ghost"); got != "" {
		t.Errorf("missing target must be empty, got %q", got)
	}
}

func TestFormatSendResultExplainsDeliveryGuarantee(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{name: "working", status: "working", want: "herd send: worker -> working (delivery confirmed)"},
		{name: "done", status: "done", want: "herd send: worker -> done (delivery confirmed)"},
		{name: "submitted", status: "submitted", want: "herd send: worker -> submitted (UNVERIFIED: --no-verify)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FormatSendResult("worker", tc.status); got != tc.want {
				t.Fatalf("FormatSendResult(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestFormatSendResultInWorkspaceAuditsAuthorizedRoute(t *testing.T) {
	if got, want := FormatSendResultInWorkspace("wB:p391", "wB", "working"), "herd send: wB:p391 [workspace=wB] -> working (delivery confirmed)"; got != want {
		t.Fatalf("FormatSendResultInWorkspace() = %q, want %q", got, want)
	}
}

func TestSendInWorkspaceAllowsExplicitCrossWorkspacePeer(t *testing.T) {
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"forge-orchestrator","pane_id":"wB:p391","workspace_id":"wB","agent_status":"idle"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if got, err := SendInWorkspace("wB:p391", "authorized coordinator packet", false, time.Second, "wB"); err != nil || got != "submitted" {
		t.Fatalf("SendInWorkspace() = %q, %v; want submitted", got, err)
	}
	if !prompted {
		t.Fatal("authorized peer must be prompted")
	}
}

func TestSendPressesEnterImmediatelyAfterPrompt(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	oldStatus := statusProbe
	t.Cleanup(func() {
		runHerdr = oldRun
		statusProbe = oldStatus
	})

	var calls []string
	statusCalls := 0
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			statusCalls++
			status := "working"
			if statusCalls == 1 {
				status = "idle"
			}
			return fmt.Sprintf(`{"result":{"agents":[{"name":"worker","pane_id":"pane-1","workspace_id":"wK","agent_status":%q}]}}`, status), nil
		}
		return "{}", nil
	}

	if got, err := Send("worker", "short kick", true, 10*time.Second); err != nil || got != "working" {
		t.Fatalf("Send = %q, %v; want working", got, err)
	}
	var transportCalls []string
	for _, call := range calls {
		if strings.HasPrefix(call, "agent prompt") || strings.HasPrefix(call, "agent send-keys") {
			transportCalls = append(transportCalls, call)
		}
	}
	if len(transportCalls) < 2 || transportCalls[0] != "agent prompt worker short kick" || transportCalls[1] != "agent send-keys worker Enter" {
		t.Fatalf("herdr transport call order = %#v; all calls = %#v", transportCalls, calls)
	}
}

func TestSendRejectsCrossWorkspaceDuplicateName(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[
				{"name":"worker","pane_id":"pane-a","workspace_id":"wK","agent_status":"idle"},
				{"name":"worker","pane_id":"pane-b","workspace_id":"wB","agent_status":"idle"}
			]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if _, err := Send("worker", "do not misroute", false, time.Second); err == nil {
		t.Fatal("cross-workspace duplicate must be rejected")
	} else if !strings.Contains(err.Error(), `workspace "wB"`) || !strings.Contains(err.Error(), `workspace "wK"`) {
		t.Fatalf("error = %v; want both workspace IDs", err)
	}
	if prompted {
		t.Fatal("cross-workspace target must be rejected before prompt")
	}
}

func TestSendRejectsAmbiguousBareLabelAcrossForgeDerivation(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[
				{"name":"scout-planner","pane_id":"pane-chainseer","workspace_id":"wC","agent_status":"idle"},
				{"name":"forge-scout-planner","pane_id":"pane-herdforge","workspace_id":"wK","agent_status":"idle"}
			]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if _, err := Send("scout-planner", "do not misdeliver", false, time.Second); err == nil {
		t.Fatal("bare label with exact and forge-derived live agents must be rejected")
	} else if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "forge-scout-planner") {
		t.Fatalf("error = %v; want an explicit ambiguous forge-derived candidate error", err)
	}
	if prompted {
		t.Fatal("ambiguous target must be rejected before prompt")
	}
}

func TestSendAllowsSameWorkspaceDelivery(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"pane-a","workspace_id":"wK","agent_status":"idle"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if got, err := Send("worker", "same workspace", false, time.Second); err != nil || got != "submitted" {
		t.Fatalf("Send = %q, %v; want submitted", got, err)
	}
	if !prompted {
		t.Fatal("same-workspace target must be prompted")
	}
}
