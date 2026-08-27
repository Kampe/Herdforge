package pulse

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/kick"
)

// FAC-619: the resume verb is an INPUT, not a state, and treating it as
// evidence made the FAC-614 fix re-trigger itself.
//
// Live evidence from installed 719b792: forge-orchestrator reported
// status=paused with raw_status=working while its pane showed
// "Working (1m 28s)", and the plan sat at "resume attempt 2 of 3". The pane
// contained "/goal resume" in scrollback -- typed there by an earlier manual
// resume, then re-armed by every automated one.
//
// The loop: resume sends "/goal resume" -> the string is in the pane forever ->
// the next read matches it -> the lane is classified paused -> resume fires
// again. A fix that manufactures its own triggering evidence.
//
// This is the false-positive class the operator asked to be pinned.

// THE regression. A working pane whose scrollback holds a previously-sent
// resume verb must NOT be classified paused.
func TestAWorkingPaneWithAResumeVerbInScrollbackIsNotPaused(t *testing.T) {
	// Reconstructed from the live pane: our own resume echoed above a lane
	// that is plainly working.
	pane := "" +
		"› /goal resume\n" +
		"\n" +
		"• Working (1m 28s • esc to interrupt) · 1 background terminal running\n" +
		"  gpt-5.6-sol high · ~/Personal/chainseer-orchestrator · Main [default]   Pursuing goal (23m)\n"

	if got := ClassifyStatusWithPane("working", false, pane); got != StatusBusy {
		t.Fatalf("status = %q, want busy.\n"+
			"An echo of the resume verb we sent was treated as evidence the lane is paused, "+
			"so every resume re-arms the next one and a working lane is resumed forever.", got)
	}
}

// The predicate itself must not accept the verb. Pinned at the kick layer too,
// because that is where the marker vocabulary lives and where a future edit
// would most plausibly re-add it.
func TestTheResumeVerbIsNotAPausedMarker(t *testing.T) {
	if kick.ContainsPausedGoalMarker(kick.GoalResumeVerb()) {
		t.Fatal("the resume verb is treated as a paused marker; it is an input we SEND, " +
			"never a state the harness reports, and matching it makes the resume self-triggering")
	}
}

// The genuine harness-reported states must still be detected -- removing the
// verb must not blunt the real signal.
func TestGenuineTerminalGoalStatesAreStillDetected(t *testing.T) {
	for _, pane := range []string{
		"Goal paused (/goal resume)", // the real status line still contains the verb
		"Goal stalled",
		"Goal achieved",
		"Goal blocked",
	} {
		if got := ClassifyStatusWithPane("working", false, pane); got != StatusPaused {
			t.Fatalf("pane %q classified %q, want paused; the real signal was blunted", pane, got)
		}
	}
}

// The subtle one: the live status line is "Goal paused (/goal resume)". It must
// still match -- on "Goal paused", not on the verb inside it. If someone
// "fixes" this by stripping the verb from the text, this goes red.
func TestTheRealStatusLineStillMatchesOnItsStateWord(t *testing.T) {
	if got := ClassifyStatusWithPane("working", false, "Goal paused (/goal resume)"); got != StatusPaused {
		t.Fatalf("the harness's own paused status line classified %q, want paused", got)
	}
}
