package pulse

import "testing"

// FAC-614: an orchestrator sat at "Goal paused (/goal resume)" while herdr
// reported agent_status=working, because the PROCESS was alive -- it was the
// goal loop that had stopped. Every status surface called it healthy. The
// operator concluded it had "failed to do shit"; sending /goal resume restored
// it instantly.
//
// A paused lane must be nameable before an automatic resume is safe to act on.

func TestAPausedGoalIsNotReportedAsBusy(t *testing.T) {
	// Exactly what the live orchestrator pane showed.
	pane := "gpt-5.6-sol high · ~/Personal/chainseer-orchestrator · Main [default]   Goal paused (/goal resume)"

	if got := ClassifyStatusWithPane("working", false, pane); got != StatusPaused {
		t.Fatalf("status = %q, want paused; a paused lane reported as busy is invisible to every census", got)
	}
}

func TestOtherTerminalGoalStatesAlsoCountAsPaused(t *testing.T) {
	// kick owns this vocabulary; pulse must agree with it rather than keep a
	// second list that drifts the first time a harness renames a marker.
	// FAC-619: "/goal resume" was in this list and its removal is the fix, not
	// a weakening. The verb is an INPUT we send; treating it as a paused state
	// made every resume re-arm the next one, so a working lane was resumed
	// forever. Observed live on forge-orchestrator at "attempt 2 of 3" while
	// its pane read "Working (1m 28s)".
	//
	// The harness's real status line is "Goal paused (/goal resume)" and still
	// matches -- on "Goal paused", pinned by
	// TestTheRealStatusLineStillMatchesOnItsStateWord.
	for _, pane := range []string{
		"Goal achieved",
		"Goal blocked",
		"Goal stalled",
	} {
		if got := ClassifyStatusWithPane("working", false, pane); got != StatusPaused {
			t.Fatalf("pane %q classified %q, want paused", pane, got)
		}
	}
}

// THE safety property. kick established it and this must not weaken it:
// guessing "paused" sends a resume verb into a healthy working lane.
func TestAnUnreadablePaneIsNeverTreatedAsPaused(t *testing.T) {
	for _, pane := range []string{"", "   ", "\n"} {
		if got := ClassifyStatusWithPane("working", false, pane); got != StatusBusy {
			t.Fatalf("unreadable pane %q classified %q; unknown is not paused", pane, got)
		}
	}
}

func TestAGenuinelyWorkingPaneStaysBusy(t *testing.T) {
	pane := "• Working (30s • esc to interrupt) · 1 background terminal running\n  Pursuing goal (23m)"

	if got := ClassifyStatusWithPane("working", false, pane); got != StatusBusy {
		t.Fatalf("a pursuing-goal lane classified %q, want busy", got)
	}
}

// Only a BUSY lane may be reclassified. Idle, done and blocked mean something
// else, and a pane that happens to contain a marker must not override them --
// scrollback can hold an old "Goal achieved" long after the lane moved on.
func TestNonBusyStatusesAreNeverReclassified(t *testing.T) {
	pane := "Goal paused (/goal resume)"
	for raw, want := range map[string]AgentStatus{
		"idle":    StatusHealthyIdle,
		"done":    StatusDone,
		"blocked": StatusBlocked,
	} {
		if got := ClassifyStatusWithPane(raw, false, pane); got != want {
			t.Fatalf("raw %q with a paused marker classified %q, want %q", raw, got, want)
		}
	}
}

// Staleness still wins: a stale lane is stale regardless of what its pane says.
func TestStaleStillOutranksPaused(t *testing.T) {
	if got := ClassifyStatusWithPane("working", true, "Goal paused (/goal resume)"); got != StatusStale {
		t.Fatalf("stale lane classified %q, want stale", got)
	}
}
