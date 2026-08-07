package herdr

import "testing"

// TestAgentReadyForToolChildBind_SessionOptional locks b81bf2e: grok-shaped
// inventory (empty agent_session.value) with a real tab must pass the bind
// readiness gate. Requiring Session.Value here is a silent fleet pin to claude.
func TestAgentReadyForToolChildBind_SessionOptional(t *testing.T) {
	grok := AgentEntry{
		Name:   "task-fac-x",
		Kind:   "grok",
		Status: "idle",
		TabID:  "wF:t1",
		PaneID: "wF:p1",
		// Session.Value intentionally empty
	}
	if !AgentReadyForToolChildBind(grok) {
		t.Fatal("grok with tab+pane and empty session must be ready for tool-child bind")
	}
	noTab := grok
	noTab.TabID = ""
	if AgentReadyForToolChildBind(noTab) {
		t.Fatal("empty TabID must not be ready (tab is identity, session is not)")
	}
	claude := grok
	claude.Kind = "claude"
	claude.Session.Value = "ses_real_abc"
	if !AgentReadyForToolChildBind(claude) {
		t.Fatal("claude with session must still be ready")
	}
}
