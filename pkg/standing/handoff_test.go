package standing

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateRefusedHandoffIsLoudAndActionable(t *testing.T) {
	tests := []struct {
		name      string
		lane      string
		handoff   string
		authority string
	}{
		{name: "review harvest", lane: "forge-review-harvest-supervisor", handoff: "NEEDS_REVIEW sha-123", authority: "read-only monitor"},
		{name: "verdict", lane: "forge-review-supervisor", handoff: "PASS sha-456", authority: "observation-only goal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRefusedHandoff(tc.lane, tc.handoff, tc.authority)
			if err == nil || !errors.Is(err, ErrConfiguration) {
				t.Fatalf("refusal error = %v, want configuration error", err)
			}
			for _, want := range []string{tc.lane, tc.handoff, tc.authority, "re-route or escalate"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
		})
	}
}

func TestObserveRefusalCannotBecomeIdlePolling(t *testing.T) {
	tracker := NewHandoffTracker()
	observation, err := tracker.ObserveRefusal("forge-review-supervisor", "NEEDS_REVIEW sha-123", "read-only monitor")
	if err == nil || observation.Lane != "forge-review-supervisor" || !observation.Progress {
		t.Fatalf("refused handoff = observation=%+v err=%v; want recorded progress and hard error", observation, err)
	}
}

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
