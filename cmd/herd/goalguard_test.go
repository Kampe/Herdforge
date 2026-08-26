package main

import (
	"strings"
	"testing"
)

// FAC-652: the block reason said "Keep working toward the goal" on EVERY
// continuation, with no concept of having nothing to do. A standing lane whose
// queue was momentarily empty could therefore only spin or die, and both were
// observed live: perf-cost-guard emitted near-identical reports every 1-2
// minutes, and herd-smith reached continuation 42 doing 20-minute waits for a
// review cap to move, burning a provider at 8% remaining to produce no artifact.
func TestGoalGuardEarlyContinuationsSayKeepWorking(t *testing.T) {
	for i := 0; i < goalGuardPlateauAfter; i++ {
		got := goalGuardContinueReason("CHA-1", "lane-a", i)
		if !strings.Contains(got, "Keep working toward the goal") {
			t.Errorf("continuation %d must still say keep working: %q", i, got)
		}
		if strings.Contains(got, "PLATEAUED") {
			t.Errorf("continuation %d is too early to call a plateau: %q", i, got)
		}
	}
}

// Past the threshold the instruction must change shape: name the plateau, forbid
// repeating the unchanged probe, and legitimise waiting on an event.
func TestGoalGuardPlateauInstructsEventWaitNotRepolling(t *testing.T) {
	got := goalGuardContinueReason("CHA-1", "lane-a", goalGuardPlateauAfter)
	for _, want := range []string{"PLATEAUED", "do NOT repeat", "WAIT for a real transition", "Waiting on an event IS valid progress"} {
		if !strings.Contains(got, want) {
			t.Errorf("plateau reason must contain %q: %q", want, got)
		}
	}
}

// CHA-2738: a plateaued Stop hook must allow the stop. Blocking it was the
// 1-2 minute identical-snapshot loop: each blocked stop became another pass.
func TestGoalGuardPlateauDoesNotBlockStop(t *testing.T) {
	if !goalGuardBlocksStop(0) || !goalGuardBlocksStop(goalGuardPlateauAfter-1) {
		t.Fatal("early continuations must still block so a real delta keeps working")
	}
	if goalGuardBlocksStop(goalGuardPlateauAfter) || goalGuardBlocksStop(goalGuardPlateauAfter+9) {
		t.Fatal("plateaued continuations must allow stop rather than emit another pass")
	}
}

// A lane that DID produce an artifact must not be told to stand down, or the
// plateau branch would suppress real work.
func TestGoalGuardPlateauExemptsALaneThatProducedAnArtifact(t *testing.T) {
	got := goalGuardContinueReason("CHA-1", "lane-a", goalGuardPlateauAfter+9)
	if !strings.Contains(got, "If you DID produce an artifact") {
		t.Errorf("the plateau instruction must exempt a lane that made progress: %q", got)
	}
	if !strings.Contains(got, "keep going") {
		t.Errorf("a productive lane must be told to continue: %q", got)
	}
}
