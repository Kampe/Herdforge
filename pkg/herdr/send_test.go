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

// FAC-579: the qualifier no longer names the MECHANISM. Consumption is proven
// by an echoed prompt where the harness echoes it, and by a status transition
// plus an advanced pane where it does not (Claude Code never echoes). Claiming
// "task text observed in pane" made the line a lie on the second path.
func TestFormatSendResultExplainsDeliveryGuarantee(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{name: "working", status: "working", want: "herd send: worker -> working (consumption confirmed)"},
		{name: "done", status: "done", want: "herd send: worker -> done (consumption confirmed)"},
		{name: "queued", status: "queued", want: "herd send: worker -> queued (queued but not consumed; explicit retry or defer required)"},
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
	if got, want := FormatSendResultInWorkspace("wB:p391", "wB", "working"), "herd send: wB:p391 [workspace=wB] -> working (consumption confirmed)"; got != want {
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
	paneReads := 0
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
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			paneReads++
			if paneReads == 1 {
				return `{"result":{"text":"empty pane"}}`, nil
			}
			return `{"result":{"text":"short kick"}}`, nil
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

func TestSendAcceptsBusyLaneOnlyWithTaskTextPaneEvidence(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	paneReads := 0
	var transport []string
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && (args[1] == "send-keys" || args[1] == "prompt") {
			transport = append(transport, strings.Join(args, " "))
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"pane-busy","workspace_id":"wK","agent_status":"working"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			paneReads++
			if paneReads == 1 {
				return `{"result":{"text":"empty pane"}}`, nil
			}
			return `{"result":{"text":"❯ assigned command: go test ./pkg/herdr"}}`, nil
		}
		return "{}", nil
	}

	got, err := Send("worker", "assigned command: go test ./pkg/herdr", true, time.Second)
	if err != nil || got != "working" {
		t.Fatalf("busy delivery = %q, %v; want task-specific pane proof", got, err)
	}
	if len(transport) < 3 || !strings.HasPrefix(transport[0], "agent send-keys worker Escape") ||
		!strings.HasPrefix(transport[1], "agent prompt worker assigned command") ||
		!strings.HasPrefix(transport[2], "agent send-keys worker Enter") {
		t.Fatalf("busy assignment transport = %#v; want Escape, prompt, Enter", transport)
	}
}

func TestSendRefusesAssignmentWhenStandingGoalCannotBePreempted(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"pane-busy","workspace_id":"wK","agent_status":"working"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			return `{"result":{"text":"empty pane"}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "send-keys" {
			return "standing goal could not be interrupted", fmt.Errorf("send-keys refused")
		}
		return "{}", nil
	}

	status, err := Send("worker", "assigned command: go test ./pkg/herdr", true, time.Second)
	if err == nil || status != "deferred" || !strings.Contains(err.Error(), "explicitly deferred") {
		t.Fatalf("preemption refusal = status %q err %v; want explicit deferred failure", status, err)
	}
}

func TestSendRejectsStagedTextEvenWhenPaneStatusLooksHealthy(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	paneReads := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"pane-staged","workspace_id":"wK","agent_status":"idle"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			paneReads++
			if paneReads == 1 {
				return `{"result":{"text":"empty pane"}}`, nil
			}
			return `{"result":{"text":"❯ [Pasted text #1]"}}`, nil
		}
		return "{}", nil
	}

	_, err := Send("worker", "assigned command: go test ./pkg/herdr", true, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "queued-but-not-consumed") || !strings.Contains(err.Error(), "staged/unsubmitted") {
		t.Fatalf("staged delivery error = %v; want explicit staged/unsubmitted failure", err)
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

func TestSendRejectsAmbiguousStandingDigestForms(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	// Two truncated digest forms for the same long standing lane — refuse.
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[
				{"name":"forge-review-harvest-su-aaaaaaaa","pane_id":"pane-a","workspace_id":"wK","agent_status":"idle"},
				{"name":"forge-review-harvest-su-bbbbbbbb","pane_id":"pane-b","workspace_id":"wK","agent_status":"idle"}
			]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if _, err := Send("review-harvest-supervisor", "do not guess", false, time.Second); err == nil {
		t.Fatal("ambiguous standing digest forms must be rejected")
	} else if !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "forge-review-harvest-su-aaaaaaaa") ||
		!strings.Contains(err.Error(), "forge-review-harvest-su-bbbbbbbb") {
		t.Fatalf("error = %v; want ambiguous refusal naming both candidates", err)
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
