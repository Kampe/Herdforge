package pulse

import "testing"

// FAC-747: only a lane that was DISPATCHED for a task can have work to review.
// A reviewer pane's worktree is a checkout of the candidate under review, so its
// unlanded commits are the candidate's, not the reviewer's — CommittedWork is
// legitimately true and must not select it. A standing lane's resident home
// likewise accumulates its own commits.
//
// Live shapes are used verbatim: these are the exact names and tabs the beat of
// 2026-09-05 planned six open_review actions against, all six wrong.
func fac747Obs() Observation {
	return Observation{
		Provider: ProviderObservation{Known: true},
		Herdr: HerdrObservation{
			Known: true,
			Agents: []AgentObservation{
				// Dispatched builder lane, genuinely finished. MUST be selected.
				{Name: "task-fac-200", TaskRef: "FAC-200", Raw: "idle", CommittedWork: true, TabID: "wK:t10", Workspace: "wK"},
				// Ephemeral reviewer panes. No task ref: never dispatched for one.
				{Name: "review-fac-746-b3da4492845e", Raw: "idle", CommittedWork: true, TabID: "wK:t0C", Workspace: "wK"},
				{Name: "review-fac-736-dfde53c30418", Raw: "idle", CommittedWork: true, TabID: "wK:tYK", Workspace: "wK"},
				// Standing lane in its resident home.
				{Name: "forge-scout-planner-39a9827d2b", Raw: "idle", CommittedWork: true, TabID: "wK:tZ1", Workspace: "wK"},
			},
		},
		Review:   ReviewObservation{Known: true},
		Quota:    QuotaObservation{Known: true},
		WindDown: WindDownObservation{Known: true, Enabled: false},
	}
}

func TestPlanDoesNotOpenReviewForReviewerPanesOrStandingLanes(t *testing.T) {
	snap, err := Plan(fac747Obs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Actions {
		if a.Kind != ActionOpenReview {
			continue
		}
		switch a.Target {
		case "wK:t0C", "wK:tYK":
			t.Fatalf("must not open_review a reviewer pane; its worktree holds the candidate under review: %+v", a)
		case "wK:tZ1":
			t.Fatalf("must not open_review a standing lane: %+v", a)
		}
	}
	if snap.Counts.OpenReview != 1 {
		t.Fatalf("expected exactly 1 open_review (the dispatched lane wK:t10), got %d: %+v", snap.Counts.OpenReview, snap.Actions)
	}
}

// The fix must not silence the planner: a real dispatched lane with unreviewed
// committed work is still selected. Without this, "select nothing" would pass
// the test above.
func TestPlanStillOpensReviewForDispatchedLane(t *testing.T) {
	snap, err := Plan(fac747Obs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range snap.Actions {
		if a.Kind == ActionOpenReview && a.Target == "wK:t10" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dispatched lane with committed work must still be open_reviewed: %+v", snap.Actions)
	}
}

// Reap is governed by TicketDone/SafeRef, which are already ref-gated, so the
// selection fix must leave it exactly as it was: these lanes stay un-reaped.
// Without this, a "fix" that cleared CommittedWork could make a standing lane
// reap-eligible, which is far worse than the bug being fixed.
func TestPlanStillDoesNotReapReviewerPanesOrStandingLanes(t *testing.T) {
	snap, err := Plan(fac747Obs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range snap.Actions {
		if a.Kind == ActionReapLane {
			t.Fatalf("no lane in this fixture is reap-eligible (none has TicketDone or SafeRef): %+v", a)
		}
	}
}

// fac747ReapObs gives a reviewer pane and a standing lane a STRAY TicketDone/
// SafeRef -- evidence that today's identity asymmetry makes structurally
// unreachable in production, but the reap planner must not depend on that
// asymmetry never being fixed or worked around elsewhere. A genuine dispatched
// lane with the same evidence is included so the guard cannot pass by
// silencing the planner outright.
func fac747ReapObs() Observation {
	return Observation{
		Provider: ProviderObservation{Known: true},
		Herdr: HerdrObservation{
			Known: true,
			Agents: []AgentObservation{
				// Genuine dispatched lane, ticket landed. MUST still be reaped.
				{Name: "task-fac-201", TaskRef: "FAC-201", Raw: "done", TicketDone: true, TabID: "wK:t30", Workspace: "wK"},
				// Reviewer pane with a stray TicketDone. Must NOT be reaped.
				{Name: "review-fac-746-b3da4492845e", TaskRef: "", Raw: "done", TicketDone: true, TabID: "wK:t0C", Workspace: "wK"},
				// Standing lane with a stray SafeRef. Must NOT be reaped.
				{Name: "forge-scout-planner-39a9827d2b", TaskRef: "", Raw: "idle", SafeRef: "refs/herd/safe/fac-746", TabID: "wK:tZ1", Workspace: "wK"},
			},
		},
		Review:   ReviewObservation{Known: true},
		Quota:    QuotaObservation{Known: true},
		WindDown: WindDownObservation{Known: true, Enabled: false},
	}
}

func TestPlanReapsGenuineTaskLaneButNotReviewerOrStandingLaneWithStrayEvidence(t *testing.T) {
	snap, err := Plan(fac747ReapObs(), Options{Act: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	sawGenuineReap := false
	for _, a := range snap.Actions {
		if a.Kind != ActionReapLane {
			continue
		}
		switch a.Target {
		case "wK:t0C":
			t.Fatalf("must not reap a reviewer pane even with a stray TicketDone: %+v", a)
		case "wK:tZ1":
			t.Fatalf("must not reap a standing lane even with a stray SafeRef: %+v", a)
		case "wK:t30":
			sawGenuineReap = true
		}
	}
	if !sawGenuineReap {
		t.Fatalf("fix must not silence the reap planner: a genuine dispatched lane with TicketDone must still be reaped: %+v", snap.Actions)
	}
	if snap.Counts.ReapLanes != 1 {
		t.Fatalf("expected exactly 1 reap (the dispatched lane wK:t30), got %d: %+v", snap.Counts.ReapLanes, snap.Actions)
	}
}
