package pulse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAC-614: naming a paused goal was half the fix. This is the policy that makes
// acting on it safe. Resuming forever is its own failure -- a lane oscillating
// between paused and working looks busy in aggregate, which is strictly worse
// than the visible stall it replaced.

var t0 = time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)

func TestAFreshPausedLaneIsResumed(t *testing.T) {
	d := DecideResume(ResumeRecord{}, "forge-orchestrator", 100, t0)
	if !d.Resume {
		t.Fatalf("a never-resumed paused lane was not resumed: %s", d.Reason)
	}
	if d.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", d.Attempt)
	}
}

// The cadence bound. Without it, every beat re-sends the resume verb.
func TestAJustResumedLaneIsThrottled(t *testing.T) {
	rec := ResumeRecord{Lane: "l", LastAttempt: t0.Add(-10 * time.Second), Consecutive: 1}
	d := DecideResume(rec, "l", 100, t0)

	if d.Resume {
		t.Fatal("a lane resumed 10s ago was resumed again; every beat would re-send the verb")
	}
	if d.NextAfter <= 0 {
		t.Fatal("throttled decision does not say when the next attempt is due")
	}
	if !strings.Contains(d.Reason, "next attempt in") {
		t.Fatalf("reason is not actionable: %s", d.Reason)
	}
}

func TestThrottleExpiresAndTheLaneIsResumedAgain(t *testing.T) {
	rec := ResumeRecord{Lane: "l", LastAttempt: t0.Add(-2 * ResumeCadence), Consecutive: 1}
	if d := DecideResume(rec, "l", 100, t0); !d.Resume {
		t.Fatalf("lane past its cadence window was not resumed: %s", d.Reason)
	}
}

// THE property that keeps this from becoming a hot loop.
func TestRepeatedResumesWithoutProgressEscalateInsteadOfLooping(t *testing.T) {
	rec := ResumeRecord{
		Lane:            "stuck",
		LastAttempt:     t0.Add(-2 * ResumeCadence),
		Consecutive:     ResumeAttemptLimit,
		LastProgressSeq: 500,
	}
	d := DecideResume(rec, "stuck", 500, t0) // seq unchanged: no progress

	if d.Resume {
		t.Fatal("a lane that never progressed was resumed again; that is an invisible hot loop")
	}
	if !d.Escalate {
		t.Fatal("exhausted lane did not escalate; the stall becomes silent")
	}
	if !strings.Contains(d.Reason, "cannot fix this") {
		t.Fatalf("escalation does not explain why resuming stopped: %s", d.Reason)
	}
}

// Progress must clear the counter, or a lane that pauses once a day eventually
// exhausts its attempts and is escalated for no reason.
func TestProgressResetsTheAttemptCounter(t *testing.T) {
	rec := ResumeRecord{
		Lane:            "healthy",
		LastAttempt:     t0.Add(-time.Hour),
		Consecutive:     ResumeAttemptLimit,
		LastProgressSeq: 500,
	}
	d := DecideResume(rec, "healthy", 900, t0) // seq advanced

	if d.Escalate {
		t.Fatal("a lane that advanced was escalated as stuck")
	}
	if !d.Resume {
		t.Fatalf("a lane that advanced was not resumed: %s", d.Reason)
	}
	if d.Attempt != 1 {
		t.Fatalf("attempt = %d after progress, want the counter reset to 1", d.Attempt)
	}
}

func TestRecordAndClearRoundTrip(t *testing.T) {
	st := ResumeState{}
	st = RecordResume(st, "l", 10, t0)
	if st.Lanes["l"].Consecutive != 1 {
		t.Fatalf("consecutive = %d, want 1", st.Lanes["l"].Consecutive)
	}
	st = RecordResume(st, "l", 10, t0.Add(time.Minute))
	if st.Lanes["l"].Consecutive != 2 {
		t.Fatalf("consecutive = %d, want 2 with no progress", st.Lanes["l"].Consecutive)
	}
	st = RecordResume(st, "l", 99, t0.Add(2*time.Minute)) // progress
	if st.Lanes["l"].Consecutive != 1 {
		t.Fatalf("consecutive = %d after progress, want 1", st.Lanes["l"].Consecutive)
	}
	st = ClearResume(st, "l")
	if _, ok := st.Lanes["l"]; ok {
		t.Fatal("cleared lane still present")
	}
}

func TestEscalatedLanesAreNamedNotCounted(t *testing.T) {
	st := ResumeState{Lanes: map[string]ResumeRecord{
		"b-stuck": {Consecutive: ResumeAttemptLimit},
		"a-stuck": {Consecutive: ResumeAttemptLimit + 1},
		"fine":    {Consecutive: 1},
	}}
	got := EscalatedLanes(st)

	if len(got) != 2 || got[0] != "a-stuck" || got[1] != "b-stuck" {
		t.Fatalf("escalated = %v, want [a-stuck b-stuck] sorted; a count is not dispatchable", got)
	}
}

// A missing state file is an empty state. A CORRUPT one is too: losing throttle
// history costs one extra resume, while refusing to beat costs the whole fleet.
func TestMissingOrCorruptStateDoesNotStopTheBeat(t *testing.T) {
	dir := t.TempDir()

	if st := LoadResumeState(filepath.Join(dir, "absent.json")); len(st.Lanes) != 0 || st.Lanes == nil {
		t.Fatal("missing state did not yield an empty usable map")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := LoadResumeState(corrupt); st.Lanes == nil {
		t.Fatal("corrupt state yielded a nil map; the next write would panic")
	}
}

func TestStateSurvivesTheProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st := RecordResume(ResumeState{}, "l", 42, t0)
	if err := SaveResumeState(path, st); err != nil {
		t.Fatal(err)
	}

	back := LoadResumeState(path)
	if back.Lanes["l"].Consecutive != 1 || back.Lanes["l"].LastProgressSeq != 42 {
		t.Fatalf("state did not round-trip: %+v", back.Lanes["l"])
	}
}
