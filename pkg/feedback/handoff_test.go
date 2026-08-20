package feedback

import "testing"

func TestHandoffTrackerIsAvailableAtFeedbackBoundary(t *testing.T) {
	tracker := NewHandoffTracker()
	tracker.Observe("lane", "report: no change")
	got := tracker.Observe("lane", "report: no change")
	if !got.InformationFree || !got.Refocus || got.Progress {
		t.Fatalf("feedback handoff = %+v, want one refocus and no progress", got)
	}
}
