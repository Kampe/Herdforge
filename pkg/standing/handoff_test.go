package standing

import "testing"

func TestHandoffTrackerDetectsCosmeticRepeatedReportsAndKicksOnce(t *testing.T) {
	tracker := NewHandoffTracker()
	first := tracker.Observe("qa", "timestamp=2026-08-20T17:00:00Z poll=1\nblocker: none\ncapacity: none")
	if !first.Progress || first.InformationFree || first.Refocus {
		t.Fatalf("first handoff = %+v, want healthy progress without refocus", first)
	}
	second := tracker.Observe("qa", "timestamp=2026-08-20T17:05:00Z poll=2\ncapacity: none\nblocker: none")
	if second.Progress || !second.InformationFree || !second.Refocus {
		t.Fatalf("repeated handoff = %+v, want idle information-free report and one refocus", second)
	}
	third := tracker.Observe("qa", "timestamp=2026-08-20T17:06:00Z poll=3\nblocker: none\ncapacity: none")
	if third.Progress || !third.InformationFree || third.Refocus {
		t.Fatalf("post-refocus repeat = %+v, want no progress and no kick loop", third)
	}
}

func TestHandoffTrackerRequiresStateChangeBeforeRefocusAgain(t *testing.T) {
	tracker := NewHandoffTracker()
	tracker.Observe("smith", "nothing changed")
	if got := tracker.Observe("smith", "nothing changed"); !got.Refocus {
		t.Fatal("first repeated handoff must refocus")
	}
	if got := tracker.Observe("smith", "new evidence: checked queue"); !got.Progress || got.InformationFree {
		t.Fatalf("changed handoff = %+v, want progress", got)
	}
	tracker.Observe("smith", "nothing changed")
	if got := tracker.Observe("smith", "nothing changed"); !got.Refocus {
		t.Fatalf("a repeated report after state change must refocus once again")
	}
}

func TestHandoffTrackerAcceptsParkedUntilAsTerminal(t *testing.T) {
	tracker := NewHandoffTracker()
	first := tracker.Observe("ops", "parked-until: 2026-08-21T09:00:00Z; no actionable work")
	if !first.Parked || first.Refocus || first.Progress {
		t.Fatalf("parked handoff = %+v, want terminal no-kick result", first)
	}
	second := tracker.Observe("ops", "parked until 2026-08-21T09:00:00Z: no actionable work")
	if !second.Parked || second.Refocus {
		t.Fatalf("repeated parked handoff = %+v, want terminal no-kick result", second)
	}
}
