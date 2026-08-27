package pulse

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAC-614: these exist because an independent review FAILed the first version
// of this change with: "the policy helpers are real; the shipped beat does not
// use them. A paused orchestrator is still classified busy, Plan emits no
// resume_goal, and Apply never calls ResumeGoal."
//
// Every test below drives Plan or Apply -- the functions the beat actually
// runs -- rather than DecideResume, which was already well covered and proved
// nothing about whether anything called it. Deleting any one of the three
// wirings turns one of these red.

func pausedObs(name string, seq int64) Observation {
	return Observation{
		Herdr: HerdrObservation{
			Known: true,
			Agents: []AgentObservation{{
				Name:     name,
				Status:   StatusPaused,
				Raw:      "working",
				PaneID:   "wB:p45J",
				StateSeq: seq,
			}},
		},
	}
}

func planOpts(t *testing.T, act bool) Options {
	t.Helper()
	return Options{
		Act:      act,
		Now:      time.Date(2026, 8, 27, 16, 0, 0, 0, time.UTC),
		StateDir: t.TempDir(),
	}
}

// WIRING 1: Plan must emit a resume action for a paused lane.
func TestPlanEmitsResumeGoalForAPausedLane(t *testing.T) {
	snap, err := Plan(pausedObs("forge-orchestrator", 10), planOpts(t, true))
	if err != nil {
		t.Fatal(err)
	}

	for _, a := range snap.Actions {
		if a.Kind == ActionResumeGoal && a.Target == "forge-orchestrator" {
			return
		}
	}
	t.Fatalf("Plan emitted no resume_goal for a paused lane; the policy exists and the beat does not use it.\nactions=%+v", snap.Actions)
}

// WIRING 2: Apply must actually call the actor.
func TestApplyCallsResumeGoal(t *testing.T) {
	opts := planOpts(t, true)
	snap, err := Plan(pausedObs("forge-orchestrator", 10), opts)
	if err != nil {
		t.Fatal(err)
	}
	actor := &recordingActor{}

	if _, err := Apply(context.Background(), snap, actor); err != nil {
		t.Fatal(err)
	}
	if len(actor.resumed) == 0 {
		t.Fatal("Apply never called ResumeGoal; a planned resume that is never executed leaves the lane paused")
	}
	if actor.resumed[0] != "forge-orchestrator" {
		t.Fatalf("resumed %v, want forge-orchestrator", actor.resumed)
	}
}

// WIRING 3: a paused lane must be COUNTED as paused, not folded into busy or
// dropped into unknown. Falling through to the default case is what made the
// stall invisible originally.
func TestAPausedLaneIsCountedAsPaused(t *testing.T) {
	snap, err := Plan(pausedObs("forge-orchestrator", 10), planOpts(t, true))
	if err != nil {
		t.Fatal(err)
	}

	if snap.Counts.Paused != 1 {
		t.Fatalf("paused count = %d, want 1", snap.Counts.Paused)
	}
	if snap.Counts.Busy != 0 {
		t.Fatalf("busy count = %d; a paused lane folded into busy is invisible again", snap.Counts.Busy)
	}
	if snap.Counts.Unknown != 0 {
		t.Fatalf("unknown count = %d; StatusPaused fell through to the default case", snap.Counts.Unknown)
	}
}

// Observe mode must not resume. A dry beat that mutates panes is not a dry beat.
func TestObserveModePlansButDoesNotResume(t *testing.T) {
	snap, err := Plan(pausedObs("forge-orchestrator", 10), planOpts(t, false))
	if err != nil {
		t.Fatal(err)
	}

	for _, a := range snap.Actions {
		if a.Kind == ActionResumeGoal {
			t.Fatal("observe mode planned an executable resume; --act is the gate for pane mutation")
		}
	}
	var sawWouldRun bool
	for _, a := range snap.Actions {
		if a.Kind == ActionWouldRun && a.WouldRun == "resume_goal forge-orchestrator" {
			sawWouldRun = true
		}
	}
	if !sawWouldRun {
		t.Fatal("observe mode did not even report that it WOULD resume; the operator cannot see the pending action")
	}
}

// The cadence bound has to survive between beats, or every beat is the first
// beat and the throttle never binds. This drives two Plans against one StateDir.
func TestThrottleSurvivesBetweenBeats(t *testing.T) {
	opts := planOpts(t, true)

	first, err := Plan(pausedObs("l", 10), opts)
	if err != nil {
		t.Fatal(err)
	}
	var firstResumed bool
	for _, a := range first.Actions {
		if a.Kind == ActionResumeGoal {
			firstResumed = true
		}
	}
	if !firstResumed {
		t.Fatal("first beat did not resume")
	}

	// Same StateDir, same clock: the second beat must be throttled.
	second, err := Plan(pausedObs("l", 10), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range second.Actions {
		if a.Kind == ActionResumeGoal {
			t.Fatal("second beat resumed the same lane immediately; cadence state did not survive, so every beat re-sends the verb")
		}
	}
}

func TestResumeStatePathHonoursTheStateDir(t *testing.T) {
	dir := t.TempDir()
	if got := ResumeStatePath(dir); got != filepath.Join(dir, ".herd", "run", "resume-state.json") {
		t.Fatalf("path = %s, want it under the supplied state dir", got)
	}
}

// WIRING 4: the human output must show paused and resume_goal. The original
// independent FAIL listed this explicitly -- "human output omits
// paused/resume_goal" -- and a count that exists only in JSON is invisible to
// the operator reading a beat, which is how the stall went unnoticed.
func TestHumanOutputShowsPausedAndResumeGoal(t *testing.T) {
	snap, err := Plan(pausedObs("forge-orchestrator", 10), planOpts(t, true))
	if err != nil {
		t.Fatal(err)
	}
	out := FormatHuman(snap)

	if !strings.Contains(out, "paused=1") {
		t.Fatalf("human output omits paused=1; the operator cannot see a paused lane:\n%s", out)
	}
	if !strings.Contains(out, "resume_goal=1") {
		t.Fatalf("human output omits resume_goal=1; the planned wake is invisible:\n%s", out)
	}
}
