package pulse

import (
	"strings"
	"testing"
)

// TestWorkingOpenCodePaneIsNotReaped is the FAC-418 gate.
//
// herdr's screen detection does not parse the OpenCode 1.18.x TUI busy
// indicator, so a genuinely working pane reports agent_status=idle. That is
// upstream and not fixable here — but the CONSEQUENCE is ours: pulse plans a
// reap directly from that status, so the misreport becomes destroyed work.
func TestWorkingOpenCodePaneIsNotReaped(t *testing.T) {
	// The exact reported condition: opencode, reported idle, live process, and
	// reap evidence present (ticket done) so the old code would have closed it.
	agents := []AgentObservation{{
		Name: "reviewer", Kind: "opencode", Status: StatusHealthyIdle,
		PaneID: "wB:p1", TabID: "wB:t1", ForegroundProcess: "opencode",
		TicketDone: true,
	}}
	snap := beatFor(t, agents)
	for _, act := range snap.Actions {
		if act.Kind == ActionReapLane {
			t.Fatalf("a pane whose idle cannot be corroborated must not be reaped: %+v", act)
		}
		if act.Safe && strings.Contains(act.WouldRun, "reap_lane") {
			t.Fatalf("a withheld reap must not be Safe: %+v", act)
		}
	}
	// It must still be REPORTED, with the way to corroborate it — silently
	// dropping the lane would leak a pane nobody knows about.
	var reported bool
	for _, act := range snap.Actions {
		if strings.Contains(act.WouldRun, "reap_lane") {
			reported = true
			for _, want := range []string{"WITHHELD", "herdr pane read", "wB:p1"} {
				if !strings.Contains(act.Reason, want) {
					t.Errorf("the withheld reap must mention %q, got: %s", want, act.Reason)
				}
			}
		}
	}
	if !reported {
		t.Error("a withheld reap must still be surfaced, not silently skipped")
	}
}

// A harness with trustworthy idle detection must still be reaped, or this fix
// turns into a fleet-wide resource leak.
func TestReliableHarnessStillReaped(t *testing.T) {
	agents := []AgentObservation{{
		Name: "worker", Kind: "claude", Status: StatusHealthyIdle,
		PaneID: "wK:p1", TabID: "wK:t1", ForegroundProcess: "claude",
		TicketDone: true,
	}}
	snap := beatFor(t, agents)
	var reaped bool
	for _, act := range snap.Actions {
		if act.Kind == ActionReapLane || strings.Contains(act.WouldRun, "reap_lane") {
			reaped = true
			if strings.Contains(act.Reason, "WITHHELD") {
				t.Errorf("a reliable harness must not be withheld: %s", act.Reason)
			}
		}
	}
	if !reaped {
		t.Error("a reliable idle harness with reap evidence must still be reaped")
	}
}

// With no live foreground process there is a second signal, so even an
// unreliable harness is corroborated and reapable.
func TestNoForegroundProcessCorroboratesIdle(t *testing.T) {
	a := AgentObservation{Kind: "opencode", ForegroundProcess: ""}
	if !a.IdleCorroborated() {
		t.Error("absence of a live process corroborates idle even for an unreliable harness")
	}
	b := AgentObservation{Kind: "opencode", ForegroundProcess: "opencode"}
	if b.IdleCorroborated() {
		t.Error("a live process contradicts idle for an unreliable harness")
	}
	c := AgentObservation{Kind: "claude", ForegroundProcess: "claude"}
	if !c.IdleCorroborated() {
		t.Error("a reliable harness needs no corroboration")
	}
}

// beatFor plans a beat over the given agents in observe mode.
func beatFor(t *testing.T, agents []AgentObservation) Snapshot {
	t.Helper()
	snap, err := Plan(Observation{
		Herdr: HerdrObservation{Known: true, Agents: agents},
	}, Options{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return snap
}
