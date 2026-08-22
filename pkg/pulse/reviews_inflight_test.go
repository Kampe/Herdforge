package pulse

import (
	"strings"
	"testing"
)

// TestReviewsInFlightIsVisibleWhenNothingNeedsOpening is the FAC-562 gate.
//
// open_review is an ACTION count: lanes that still need a review OPENED. A lane
// already pinned out for review is deliberately excluded, so a busy conveyor
// legitimately reports open_review=0. A consumer read that as "no review work"
// while an admitted FAIL was under repair and other exact reviews were in
// flight, and the summary offered no way to tell the two apart.
func TestReviewsInFlightIsVisibleWhenNothingNeedsOpening(t *testing.T) {
	agents := []AgentObservation{
		// Out for review: nothing to open, but the conveyor IS moving.
		{Name: "defi", Status: StatusHealthyIdle, CommittedWork: true, SafeRef: "herd/defi-1"},
		// Awaiting a verdict it must act on: also review activity.
		{Name: "api", Status: StatusHealthyIdle, AwaitingVerdict: true},
		// Genuinely idle with nothing under review.
		{Name: "idle", Status: StatusHealthyIdle},
	}
	c := CountActions(agents, nil)
	if c.OpenReview != 0 {
		t.Fatalf("fixture assumption: nothing should need opening, got open_review=%d", c.OpenReview)
	}
	if c.ReviewsInFlight != 2 {
		t.Fatalf("reviews_in_flight = %d, want 2 (one pinned out, one awaiting a verdict)", c.ReviewsInFlight)
	}
}

// A lane that needs review opened is not yet in flight; the two counts describe
// different things and must not double-count.
func TestOpenReviewAndInFlightAreDistinct(t *testing.T) {
	// A finished lane with committed work and no SafeRef is what open_review
	// plans for; it is NOT yet in flight.
	needsOpening := []AgentObservation{
		{Name: "finished", Status: StatusDone, CommittedWork: true, TabID: "wK:t1"},
	}
	planned := []Action{{Kind: ActionOpenReview, Target: "wK:t1"}}
	c := CountActions(needsOpening, planned)
	if c.OpenReview != 1 {
		t.Fatalf("open_review = %d, want 1", c.OpenReview)
	}
	if c.ReviewsInFlight != 0 {
		t.Errorf("a lane not yet out for review is not in flight, got %d", c.ReviewsInFlight)
	}
}

// The human summary must carry it, or an operator reading the terminal still
// cannot tell a quiet conveyor from a busy one.
func TestHumanSummaryReportsReviewsInFlight(t *testing.T) {
	snap := Snapshot{Counts: Counts{ReviewsInFlight: 3}}
	out := FormatHuman(snap)
	if !strings.Contains(out, "reviews_in_flight=3") {
		t.Errorf("human counts must include reviews_in_flight, got:\n%s", out)
	}
}
