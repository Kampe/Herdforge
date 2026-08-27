package beat

import (
	"strings"
	"testing"
)

// FAC-706: fleet health was being read off resident tab counts. Eleven lanes
// were present and doing nothing while every pane-counting summary reported a
// healthy fleet.
func TestIdleLaneWithWorkIsAFailedBeat(t *testing.T) {
	u := ProjectUtilization([]LaneState{
		{Lane: "docs-custodian", Status: "idle", WorkAvailable: true},
	})
	if u.FailedBeats != 1 {
		t.Fatalf("an idle lane with queued work was not a failed beat: %+v", u)
	}
	l := u.Lanes[0]
	// A failed beat that names no wake is only a complaint.
	if l.NextWake == "" {
		t.Fatal("failed beat carries no next wake action")
	}
	if l.Blocker == "" {
		t.Fatal("failed beat carries no blocker")
	}
}

func TestIdleLaneWithNoWorkIsNotAFailure(t *testing.T) {
	// A resting lane with an empty queue is doing the right thing. Counting it
	// as a failure makes the number meaningless.
	u := ProjectUtilization([]LaneState{
		{Lane: "scout-planner", Status: "idle", WorkAvailable: false},
	})
	if u.FailedBeats != 0 {
		t.Fatalf("a lane with nothing to do was reported as failing: %+v", u)
	}
}

func TestHeldLaneIsNeverAFailedBeat(t *testing.T) {
	// A held lane is doing exactly what an operator asked. Reporting it as a
	// failure would train everyone to ignore the count.
	u := ProjectUtilization([]LaneState{
		{Lane: "defi-crusader", Status: "idle", Held: true, WorkAvailable: true},
	})
	if u.FailedBeats != 0 {
		t.Fatalf("a deliberately held lane was reported as failing: %+v", u)
	}
	if u.Held != 1 {
		t.Fatalf("held lane not counted as held: %+v", u)
	}
}

func TestWorkingLaneIsNotAFailedBeat(t *testing.T) {
	u := ProjectUtilization([]LaneState{
		{Lane: "orchestrator", Status: "working", WorkAvailable: true},
	})
	if u.FailedBeats != 0 || u.Working != 1 {
		t.Fatalf("a working lane was misclassified: %+v", u)
	}
}

func TestSummaryLeadsWithTheFailureCount(t *testing.T) {
	// A summary that leads with "14 lanes" invites the reading that fourteen
	// lanes are working. That is exactly how a dead fleet went unnoticed.
	u := ProjectUtilization([]LaneState{
		{Lane: "a", Status: "idle", WorkAvailable: true},
		{Lane: "b", Status: "working"},
	})
	s := u.Summarize()
	if !strings.HasPrefix(s, "utilization: 1 FAILED BEAT") {
		t.Fatalf("summary buries the failure: %s", s)
	}
}

func TestEmptyRosterIsNotAHealthyFleet(t *testing.T) {
	// Zero lanes resolved means the roster did not resolve, not that the fleet
	// is fine. Reporting "no failed beats" here is the FAC-604 defect.
	s := ProjectUtilization(nil).Summarize()
	if !strings.Contains(s, "unresolved roster") {
		t.Fatalf("an empty roster read as a healthy fleet: %s", s)
	}
}

func TestFailedBeatsSortFirst(t *testing.T) {
	u := ProjectUtilization([]LaneState{
		{Lane: "zzz", Status: "working"},
		{Lane: "aaa", Status: "idle", WorkAvailable: true},
	})
	if u.Lanes[0].Lane != "aaa" {
		t.Fatalf("failed beats are not surfaced first: %+v", u.Lanes)
	}
}
