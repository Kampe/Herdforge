package broker

import (
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/progress"
)

// THE acceptance case for CHA-3174: a full review slot must not suppress an
// independent dependency-ready builder. Measured live before this seam existed:
// dispatch blocked with 6 healthy-idle lanes and 2 reviews in flight against a
// cap of 3, because review saturation leaked into builder admission.
func TestReviewSaturationDoesNotSuppressAnIndependentBuilder(t *testing.T) {
	d := Decide(Inputs{
		Lane:             "defi-crusader",
		Accepts:          []Kind{KindBuild},
		Queue:            []Task{{Ref: "CHA-100", Kind: KindBuild, Priority: 5}},
		ReviewSaturated:  true,
		ReviewWaitReason: "review capacity is full",
	})
	if err := d.Validate(); err != nil {
		t.Fatalf("decision must be actionable: %v", err)
	}
	if d.Outcome != OutcomeWork {
		t.Fatalf("a review cap must never block a builder: outcome=%s reason=%q", d.Outcome, d.WaitReason)
	}
	if d.Task.Ref != "CHA-100" {
		t.Errorf("wrong task: %q", d.Task.Ref)
	}
}

// Review work IS still gated by review capacity -- the separation must cut both
// ways, or the fix would just remove a real bound.
func TestReviewWorkIsStillGatedByReviewCapacity(t *testing.T) {
	d := Decide(Inputs{
		Lane:             "review-lane",
		Accepts:          []Kind{KindReview},
		Queue:            []Task{{Ref: "CHA-200", Kind: KindReview}},
		ReviewSaturated:  true,
		ReviewWaitReason: "3 reviews in flight >= cap 3",
	})
	if d.Outcome != OutcomeWait {
		t.Fatal("review work must still respect the review cap")
	}
	if !strings.Contains(d.WaitReason, "cap 3") {
		t.Errorf("the wait must name the real bound: %q", d.WaitReason)
	}
}

// A decision must NAME an exact task. A bare count is not representable, which
// is what stops the selector defect recurring through this seam.
func TestADecisionWithoutATaskIdentityIsRejected(t *testing.T) {
	if err := (Decision{Outcome: OutcomeWork}).Validate(); err == nil {
		t.Fatal("a work decision naming no task must be rejected")
	} else if !strings.Contains(err.Error(), "EXACT task ref") {
		t.Errorf("the refusal must say why: %v", err)
	}
	if err := (Decision{Outcome: OutcomeWait}).Validate(); err == nil {
		t.Fatal("a wait naming no event must be rejected")
	}
	if err := (Decision{}).Validate(); err == nil {
		t.Fatal("a decision with no outcome must be rejected")
	}
}

// Dependency readiness blocks, and the block is REPORTED rather than silent.
func TestDependencyBlockingIsExplicitAndNamed(t *testing.T) {
	d := Decide(Inputs{
		Lane:    "defi-crusader",
		Accepts: []Kind{KindBuild},
		Queue: []Task{
			{Ref: "CHA-2796", Kind: KindBuild, Priority: 9, DependsOn: []string{"CHA-3116"}},
			{Ref: "CHA-300", Kind: KindBuild, Priority: 1},
		},
		ClosedTasks: map[string]bool{},
	})
	if d.Outcome != OutcomeWork || d.Task.Ref != "CHA-300" {
		t.Fatalf("a blocked high-priority task must not stall a ready lower one: %+v", d)
	}
	if got := d.Blocked["CHA-2796"]; !strings.Contains(got, "CHA-3116") {
		t.Errorf("the block must NAME the unmet dependency, got %q", got)
	}
	// Closing the dependency releases it, and priority then wins.
	d2 := Decide(Inputs{
		Lane:    "defi-crusader",
		Accepts: []Kind{KindBuild},
		Queue: []Task{
			{Ref: "CHA-2796", Kind: KindBuild, Priority: 9, DependsOn: []string{"CHA-3116"}},
			{Ref: "CHA-300", Kind: KindBuild, Priority: 1},
		},
		ClosedTasks: map[string]bool{"CHA-3116": true},
	})
	if d2.Task == nil || d2.Task.Ref != "CHA-2796" {
		t.Fatalf("closing the dependency must release the higher-priority task: %+v", d2)
	}
}

// An empty queue and a fully-blocked queue are DIFFERENT states and must read
// differently: the first is idle, the second waits on something specific.
func TestAnEmptyQueueReadsDifferentlyFromABlockedOne(t *testing.T) {
	empty := Decide(Inputs{Lane: "x", Accepts: []Kind{KindBuild}})
	if err := empty.Validate(); err != nil {
		t.Fatalf("even an idle decision must be actionable: %v", err)
	}
	if !strings.Contains(empty.WaitReason, "queue is empty, not blocked") {
		t.Errorf("an empty queue must say so: %q", empty.WaitReason)
	}

	blockedQ := Decide(Inputs{
		Lane: "x", Accepts: []Kind{KindBuild},
		Queue:       []Task{{Ref: "CHA-1", Kind: KindBuild, DependsOn: []string{"CHA-0"}}},
		ClosedTasks: map[string]bool{},
	})
	if strings.Contains(blockedQ.WaitReason, "queue is empty") {
		t.Errorf("a blocked queue must not read as empty: %q", blockedQ.WaitReason)
	}
	if !strings.Contains(blockedQ.WaitReason, "CHA-0") {
		t.Errorf("the wait must name what would unblock it: %q", blockedQ.WaitReason)
	}
}

// Two lanes must not collide on one file, and the refusal names the owner.
func TestPathOwnershipPreventsACollision(t *testing.T) {
	d := Decide(Inputs{
		Lane:         "ux-comber",
		Accepts:      []Kind{KindBuild},
		Queue:        []Task{{Ref: "CHA-400", Kind: KindBuild, OwnedPaths: []string{"docs/testing/MASTER-TEST-PLAN.md"}}},
		ClaimedPaths: map[string]string{"docs/testing/MASTER-TEST-PLAN.md": "docs-custodian"},
	})
	if d.Outcome != OutcomeWait {
		t.Fatal("a claimed path must block the task")
	}
	if !strings.Contains(d.Blocked["CHA-400"], "docs-custodian") {
		t.Errorf("the block must name the owning lane: %q", d.Blocked["CHA-400"])
	}
	// The SAME lane holding the path is not a collision with itself.
	same := Decide(Inputs{
		Lane:         "docs-custodian",
		Accepts:      []Kind{KindBuild},
		Queue:        []Task{{Ref: "CHA-400", Kind: KindBuild, OwnedPaths: []string{"docs/testing/MASTER-TEST-PLAN.md"}}},
		ClaimedPaths: map[string]string{"docs/testing/MASTER-TEST-PLAN.md": "docs-custodian"},
	})
	if same.Outcome != OutcomeWork {
		t.Error("a lane must not be blocked by its own claim")
	}
}

// Selection is deterministic: priority descending, then ref ascending. A
// selector whose order depends on map iteration cannot be reproduced.
func TestSelectionIsDeterministic(t *testing.T) {
	q := []Task{
		{Ref: "CHA-9", Kind: KindBuild, Priority: 1},
		{Ref: "CHA-2", Kind: KindBuild, Priority: 5},
		{Ref: "CHA-1", Kind: KindBuild, Priority: 5},
	}
	for i := 0; i < 20; i++ {
		d := Decide(Inputs{Lane: "x", Accepts: []Kind{KindBuild}, Queue: q})
		if d.Task == nil || d.Task.Ref != "CHA-1" {
			t.Fatalf("iteration %d selected %+v; want CHA-1 (priority desc, then ref asc)", i, d.Task)
		}
	}
}

// The decision carries progress classification, so a lane that only polled is
// not reported as having worked.
func TestTheDecisionCarriesProgressClassification(t *testing.T) {
	rec := progress.Record{Lane: "x", TaskRef: "CHA-1"}
	rec, _ = rec.Observe(t0(), progress.ClassProbe, "unchanged")
	d := Decide(Inputs{Lane: "x", Accepts: []Kind{KindBuild}, Progress: rec})
	if d.Progress.UnchangedBeats != 1 {
		t.Fatalf("the decision must carry the lane's progress state, got %+v", d.Progress)
	}
}

func t0() time.Time { return time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC) }
