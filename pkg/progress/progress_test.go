package progress

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

// FAC-661: the audit's requirement, stated as a test. Sleeps, duplicate reports
// and unchanged probes must NOT refresh progress, while commits, verdicts and
// merges must. Measured on the live fleet: perf-cost-guard emitted near-identical
// reports every 1-2 minutes and herd-smith reached continuation 42 waiting on a
// review cap, and both counted as busy.
func TestProbingAndWaitingNeverAdvanceALane(t *testing.T) {
	r := Record{Lane: "perf-cost-guard", TaskRef: "CHA-1"}
	for i := 1; i <= 5; i++ {
		var advanced bool
		r, advanced = r.Observe(t0, ClassProbe, "same-report")
		if advanced {
			t.Fatalf("beat %d: observing unchanged state must never advance a lane", i)
		}
		if r.UnchangedBeats != i {
			t.Errorf("beat %d: unchanged beats = %d, want %d", i, r.UnchangedBeats, i)
		}
	}
	if !r.Plateaued(3) {
		t.Error("five unchanged beats must register as a plateau")
	}
}

// A build beat that re-emits the SAME artifact is the same unchanged report
// wearing a more productive label.
func TestReEmittingTheSameArtifactIsNotProgress(t *testing.T) {
	r := Record{Lane: "docs-custodian", TaskRef: "CHA-2"}
	r, first := r.Observe(t0, ClassBuild, "candidate-abc123")
	if !first {
		t.Fatal("a new candidate must advance the lane")
	}
	r, again := r.Observe(t0.Add(time.Minute), ClassBuild, "candidate-abc123")
	if again {
		t.Fatal("re-emitting the same candidate must not advance the lane")
	}
	if r.UnchangedBeats != 1 {
		t.Errorf("unchanged beats = %d, want 1", r.UnchangedBeats)
	}
}

// Real work must advance and must reset the plateau, or the rule would starve
// productive lanes.
func TestRealWorkAdvancesAndClearsThePlateau(t *testing.T) {
	r := Record{Lane: "chain-indexer", TaskRef: "CHA-3"}
	for i := 0; i < 4; i++ {
		r, _ = r.Observe(t0, ClassProbe, "nothing")
	}
	if !r.Plateaued(3) {
		t.Fatal("precondition: the lane should be plateaued")
	}
	r, advanced := r.Observe(t0.Add(time.Hour), ClassReview, "verdict-sha-999")
	if !advanced {
		t.Fatal("a new verdict must advance the lane")
	}
	if r.UnchangedBeats != 0 {
		t.Errorf("a real advance must clear the plateau, got %d", r.UnchangedBeats)
	}
	if r.Plateaued(3) {
		t.Error("a lane that just advanced is not plateaued")
	}
	if got := r.ProgressAge(t0.Add(time.Hour)); got != 0 {
		t.Errorf("progress age right after an advance must be zero, got %v", got)
	}
}

// A lane that has NEVER produced anything must not look freshly productive.
func TestALaneThatNeverAdvancedReportsMaximumAge(t *testing.T) {
	r := Record{Lane: "new-lane", TaskRef: "CHA-4"}
	if got := r.ProgressAge(t0); got < 24*time.Hour {
		t.Fatalf("a lane with no advance must report a large age, got %v", got)
	}
}

// A record naming no task is the selector defect in miniature: "four claimable"
// with no identities can be counted but not dispatched.
func TestARecordWithoutATaskRefIsNotActionable(t *testing.T) {
	ok, why := Record{Lane: "x"}.Actionable()
	if ok {
		t.Fatal("a lane reported busy without naming its task must not be actionable")
	}
	if why == "" {
		t.Error("the refusal must say why")
	}
	if ok, _ := (Record{Lane: "x", TaskRef: "CHA-1"}).Actionable(); !ok {
		t.Error("a lane with an exact task ref must be actionable")
	}
}

// A wait with no named event is indistinguishable from a spin, which is the
// behaviour goal-guard's plateau rule exists to replace.
func TestWaitingMustNameItsEvent(t *testing.T) {
	r := Record{Lane: "defi-crusader", TaskRef: "CHA-5", Action: ClassWait}
	if ok, why := r.Actionable(); ok || why == "" {
		t.Fatal("a wait with no named event must be refused; it cannot be told apart from a spin")
	}
	r.WaitReason = "CHA-3116 to close"
	if ok, _ := r.Actionable(); !ok {
		t.Error("a wait on a NAMED event is legitimate for a standing lane")
	}
}
