package herdr

import "testing"

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
