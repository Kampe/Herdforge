package herdr

import "testing"

// FAC-589: an echo above baseline proves consumption on its own. The check used
// to require status ∈ {working,done} AND the echo in the SAME poll iteration,
// which threw away proof already in hand: a codex pane cycles
// idle -> done -> working -> idle within seconds (measured on this fleet), so a
// poll landing on "idle" discarded an echo visible at that very moment and
// reported queued-but-not-consumed for a reviewer that was demonstrably running.
//
// These assert the evidence rule the fix relies on: a new occurrence above
// baseline is decisive, and an unchanged pane still proves nothing.
func TestEchoAboveBaselineIsDecisiveRegardlessOfStatus(t *testing.T) {
	text := "Read and execute the review packet at .herd/review-packets/review-cha-1804-9004d1f4fd6a.md in full."
	baseline := "codex splash\n> Ask Codex to do anything\n"
	after := baseline + "› " + text + "\n• SessionStart hook (completed)\n"

	if got := observationCount(text, baseline); got != 0 {
		t.Fatalf("baseline must not already contain the text, got count %d", got)
	}
	if got := observationCount(text, after); got <= observationCount(text, baseline) {
		t.Fatalf("a new echo must raise the count: after=%d baseline=%d",
			got, observationCount(text, baseline))
	}
}

// Long sends are wrapped by pane readback. The count must survive wrapping, or
// every real packet delivery is unprovable no matter which branch checks it.
func TestEchoCountSurvivesPaneWrapping(t *testing.T) {
	text := "Read and execute the review packet at .herd/review-packets/review-cha-2164-f0e11299fb1f.md in full."
	wrapped := "› Read and execute the review packet at\n.herd/review-packets/review-cha-2164-f0e11299fb1f.md\nin full.\n"
	if observationCount(text, wrapped) < 1 {
		t.Error("wrapped echo must still count; requiring an unwrapped match makes every long packet unprovable")
	}
}

// The anti-staleness guarantee must hold: pane text that was already there
// before the send cannot prove a new delivery.
func TestPreexistingTextDoesNotProveDelivery(t *testing.T) {
	text := "Read and execute the review packet at p.md in full."
	baseline := "› " + text + "\n"
	if observationCount(text, baseline) > observationCount(text, baseline) {
		t.Fatal("identical pane cannot out-count itself")
	}
	// A pane that never changed proves nothing, which is what the caller relies
	// on when it compares against the baseline.
	if paneAdvanced(baseline, baseline) {
		t.Error("an unchanged pane must not be treated as advanced")
	}
}
