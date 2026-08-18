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

func TestSendPressesEnterImmediatelyAfterPrompt(t *testing.T) {
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
			return fmt.Sprintf(`{"result":{"agents":[{"name":"worker","pane_id":"pane-1","agent_status":%q}]}}`, status), nil
		}
		return "{}", nil
	}

	if got, err := Send("worker", "short kick", true, 10*time.Second); err != nil || got != "working" {
		t.Fatalf("Send = %q, %v; want working", got, err)
	}
	if len(calls) < 2 {
		t.Fatalf("herdr calls = %#v; want prompt and immediate Enter", calls)
	}
	if calls[0] != "agent prompt worker short kick" || calls[1] != "agent send-keys worker Enter" {
		t.Fatalf("herdr call order = %#v; want prompt then send-keys Enter", calls)
	}
}
