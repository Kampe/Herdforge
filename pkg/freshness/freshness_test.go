package freshness

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)

// FAC-662: a Kaneo ListTasks timeout produced an empty task list, and an empty
// task list is indistinguishable from a drained queue -- so a selector reported
// "0 claimable" and lanes went idle against a board that was full. This is the
// same absence-read-as-a-negative this codebase has produced in eight other
// places; the difference is the absence comes from OUTSIDE, so it cannot be
// fixed by being careful at each call site.
func TestATimeoutWithNoPriorAnswerIsUnknownNotEmpty(t *testing.T) {
	var none Reading[[]string]
	got := Degrade(none, "kaneo", errors.New("ListTasks: deadline exceeded after 30s"), "retry; check the board provider")

	if got.State != StateUnknown {
		t.Fatalf("state = %q, want UNKNOWN", got.State)
	}
	// The critical property: a consumer that ignores ok must NOT receive a
	// plausible empty list.
	tasks, ok := got.Value()
	if ok {
		t.Fatal("UNKNOWN must not be usable; an empty task list must be unreachable by forgetting to check")
	}
	if len(tasks) != 0 {
		t.Error("the zero value must be returned, never a fabricated one")
	}
	msg := got.MustExplain(t0)
	if !strings.Contains(msg, "NOT the same as nothing being there") {
		t.Errorf("the explanation must refuse the empty reading explicitly: %q", msg)
	}
	if !strings.Contains(msg, "deadline exceeded") {
		t.Errorf("the cause must be reported, not just the posture: %q", msg)
	}
}

// With a prior answer, a failure degrades to STALE and stays usable -- but only
// with its age stated, never silently.
func TestAFailureAfterASuccessIsStaleAndKeepsTheLastAnswer(t *testing.T) {
	prev := Fresh("kaneo", t0, []string{"CHA-1", "CHA-2"})
	got := Degrade(prev, "kaneo", errors.New("timeout"), "retry")

	if got.State != StateStale {
		t.Fatalf("state = %q, want STALE", got.State)
	}
	tasks, ok := got.Value()
	if !ok || len(tasks) != 2 {
		t.Fatalf("a stale answer must remain usable, got %v ok=%v", tasks, ok)
	}
	msg := got.MustExplain(t0.Add(90 * time.Second))
	if !strings.Contains(msg, "STALE by 1m30s") {
		t.Errorf("staleness must state its AGE so a consumer can judge it: %q", msg)
	}
	if !strings.Contains(msg, "not a current one") {
		t.Errorf("a stale answer must never read as current: %q", msg)
	}
}

// A genuinely empty answer is different from both, and must stay usable.
func TestAGenuinelyEmptyAnswerIsFreshAndUsable(t *testing.T) {
	got := Fresh("kaneo", t0, []string{})
	tasks, ok := got.Value()
	if !ok {
		t.Fatal("a source that answered 'nothing is there' gave a real answer")
	}
	if len(tasks) != 0 {
		t.Error("an empty answer is empty")
	}
	if got.State != StateFresh {
		t.Errorf("state = %q, want FRESH", got.State)
	}
}

// A caller needing certainty must be able to refuse rather than act on history.
func TestStaleBeyondLetsACallerRefuseOldEvidence(t *testing.T) {
	prev := Fresh("kaneo", t0, 42)
	stale := Degrade(prev, "kaneo", errors.New("timeout"), "")
	if stale.StaleBeyond(t0.Add(time.Minute), 5*time.Minute) {
		t.Error("one minute old is within a five minute limit")
	}
	if !stale.StaleBeyond(t0.Add(10*time.Minute), 5*time.Minute) {
		t.Error("ten minutes old exceeds a five minute limit and must be refusable")
	}
	// FRESH is never stale, whatever the limit.
	if Fresh("kaneo", t0, 1).StaleBeyond(t0.Add(time.Hour), time.Second) {
		t.Error("a fresh reading is not stale")
	}
}

// Recovery is advice the operator acts on, never something the adapter performs.
func TestRecoveryIsReportedNotPerformed(t *testing.T) {
	var none Reading[int]
	got := Degrade(none, "colima", errors.New("socket missing"), "colima start")
	if !strings.Contains(got.MustExplain(t0), "Recovery: colima start") {
		t.Error("the narrow recovery action must be surfaced to the operator")
	}
}
