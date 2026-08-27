package beat

import (
	"fmt"
	"sort"
	"strings"
)

// LaneState is one lane's live utilization, published rather than inferred.
//
// FAC-706: fleet health was being read off resident tab counts. Eleven lanes
// were "present" and doing nothing, and every summary that counted panes
// reported a healthy fleet. Presence is not activity, and a report that cannot
// tell them apart is worse than no report, because it ends the investigation.
type LaneState struct {
	Lane string `json:"lane"`
	// Agent is the live agent name serving this lane, empty when none is.
	Agent  string `json:"agent,omitempty"`
	Status string `json:"status"`
	// Action is the exact thing this lane is doing right now. Empty is
	// meaningful: a lane with no action is not working, whatever its status
	// says.
	Action string `json:"action,omitempty"`
	// Blocker is why the lane is not acting. Required for a failed beat.
	Blocker string `json:"blocker,omitempty"`
	// Held marks a lane an operator deliberately parked. A held lane is NOT a
	// failed beat: it is doing exactly what was asked.
	Held bool `json:"held"`
	// WorkAvailable is whether the queue has something this lane could take.
	WorkAvailable bool `json:"work_available"`
	// FailedBeat is the whole point: idle, unheld, with work available.
	FailedBeat bool `json:"failed_beat"`
	// NextWake is the concrete action that would move this lane.
	NextWake string `json:"next_wake,omitempty"`
}

// Utilization is the fleet-wide beat.
type Utilization struct {
	Lanes []LaneState `json:"lanes"`
	// Counts are derived, never authored, so the summary cannot disagree with
	// the rows underneath it.
	Working     int `json:"working"`
	Held        int `json:"held"`
	FailedBeats int `json:"failed_beats"`
	Total       int `json:"total"`
}

// Summarize renders a one-line beat that leads with the failure count.
//
// A summary that leads with "14 lanes" invites the reading that fourteen lanes
// are working. It leads with what is wrong.
func (u Utilization) Summarize() string {
	if u.Total == 0 {
		return "utilization: NO LANES RESOLVED — this is an unresolved roster, not an empty fleet"
	}
	if u.FailedBeats == 0 {
		return fmt.Sprintf("utilization: %d lane(s), %d working, %d held, no failed beats", u.Total, u.Working, u.Held)
	}
	return fmt.Sprintf("utilization: %d FAILED BEAT(S) of %d lane(s) — idle with work available (%d working, %d held)",
		u.FailedBeats, u.Total, u.Working, u.Held)
}

// ProjectUtilization builds the beat from live lane facts.
//
// isWorking, isHeld and hasWork are supplied by the caller so this decision is
// testable without a fleet, and so the definition of "working" lives with the
// census that observes it rather than being guessed here.
func ProjectUtilization(lanes []LaneState) Utilization {
	u := Utilization{Lanes: append([]LaneState(nil), lanes...)}
	for i := range u.Lanes {
		l := &u.Lanes[i]
		working := strings.EqualFold(l.Status, "working") || strings.EqualFold(l.Status, "starting")
		switch {
		case l.Held:
			u.Held++
		case working:
			u.Working++
		}
		// A failed beat is precise: unheld, not working, and there IS work.
		// Idle with an empty queue is a resting lane and must not be reported
		// as a failure, or the count stops meaning anything.
		l.FailedBeat = !l.Held && !working && l.WorkAvailable
		if l.FailedBeat {
			u.FailedBeats++
			if strings.TrimSpace(l.NextWake) == "" {
				// A failed beat that names no wake is only a complaint. The
				// report must always carry the action that would fix it.
				l.NextWake = "herd kick --lane " + l.Lane
			}
			if strings.TrimSpace(l.Blocker) == "" {
				l.Blocker = "idle with queued work and no stated reason"
			}
		}
	}
	u.Total = len(u.Lanes)
	sort.SliceStable(u.Lanes, func(i, j int) bool {
		if u.Lanes[i].FailedBeat != u.Lanes[j].FailedBeat {
			return u.Lanes[i].FailedBeat
		}
		return u.Lanes[i].Lane < u.Lanes[j].Lane
	})
	return u
}
