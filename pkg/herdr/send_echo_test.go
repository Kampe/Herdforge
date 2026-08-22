package herdr

import "testing"

// TestNonEchoingHarnessCanProveConsumption is the FAC-579 gate.
//
// Delivery verification required the submitted text to reappear in the pane.
// Claude Code renders a compact transcript — "Marinating…", "Ran 9 commands" —
// and never keeps the prompt visible, so every claude delivery was reported
// queued-but-not-consumed while the agent was demonstrably working on it. `herd
// review --pool` read that as a failed launch and closed the reviewer it had
// just started, so the review backlog could not move: 43 candidates queued, 0
// reviewed, every launch discarded at the last step.
func TestNonEchoingHarnessCanProveConsumption(t *testing.T) {
	// Codex echoes, so it keeps the strong proof.
	for _, kind := range []string{"codex", "opencode", "ollama", "lazer", "pi"} {
		if !harnessEchoesPrompt(kind) {
			t.Errorf("%s echoes its prompt and must keep echo-based proof", kind)
		}
	}
	// Claude does not, and neither does anything unrecognised — the default is
	// the strong path, so a new harness is not silently granted the fallback...
	// except that it needs the fallback to work at all. Listing the echoers is
	// what keeps that explicit.
	if harnessEchoesPrompt("claude") {
		t.Error("Claude Code does not echo the submitted prompt")
	}
}

// The fallback must require the pane to have ADVANCED. A pane that ignored the
// input does not change, and accepting an unchanged pane would make the status
// alone sufficient — which is the fail-open this verification exists to prevent.
func TestPaneMustAdvanceForTheFallback(t *testing.T) {
	before := "❯ \n  [Sonnet 5] repo │ main"
	if paneAdvanced(before, before) {
		t.Error("an unchanged pane is not evidence of consumption")
	}
	if paneAdvanced(before, "") {
		t.Error("an empty readback is not evidence of consumption")
	}
	after := before + "\n· Marinating… (12s · ↓ 398 tokens)"
	if !paneAdvanced(before, after) {
		t.Error("a pane that started working must count as advanced")
	}
	// Whitespace-only churn must not count: a status line re-rendering its
	// elapsed timer is not consumption.
	if paneAdvanced("a  b", " a b ") {
		t.Error("whitespace churn is not advancement")
	}
}
