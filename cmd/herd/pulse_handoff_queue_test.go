package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

func commitsN(prefix string, n int) []harvest.UnlandedCommit {
	out := make([]harvest.UnlandedCommit, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, harvest.UnlandedCommit{
			SHA:     fmt.Sprintf("%s%038d", prefix, i),
			Subject: fmt.Sprintf("feat: %s %d", prefix, i),
		})
	}
	return out
}

// TestBothHandoffsSurvive is the FAC-567 regression, in the reported shape.
//
// Sequential pane delivery let a later handoff supersede an earlier one, so the
// API queue (29 commits) and the DeFi queue (2 commits) could not both be
// delivered to the single supervisor. Consumption is not retention.
func TestBothHandoffsSurvive(t *testing.T) {
	mailPath := filepath.Join(t.TempDir(), "control-mail.jsonl")
	const supervisor = "forge-review-supervisor"

	api := commitsN("a", 29)
	defi := commitsN("d", 2)

	for _, h := range []struct {
		lane    string
		commits []harvest.UnlandedCommit
	}{{"forge-api-crusader", api}, {"forge-defi-crusader", defi}} {
		queued, err := enqueueReviewHandoff(context.Background(), mailPath, "pulse", supervisor,
			"PULSE REVIEW HANDOFF "+h.lane, "body for "+h.lane,
			handoffEnvelopeID(h.lane, h.commits))
		if err != nil {
			t.Fatalf("%s: %v", h.lane, err)
		}
		if !queued {
			t.Fatalf("%s must be newly queued", h.lane)
		}
	}

	pending, err := PendingReviewHandoffs(mailPath, supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("both handoffs must be retained, got %d", len(pending))
	}
	// Ordering preserved: the API handoff was queued first.
	if pending[0].Subject > pending[1].Subject && pending[0].Sequence > pending[1].Sequence {
		t.Fatalf("append order must be preserved: %+v", pending)
	}
}

// A repeated beat with an UNCHANGED candidate set must not duplicate. This is
// the starvation loop: pulse --act re-emitting the same work every pass.
func TestRepeatedBeatDoesNotDuplicate(t *testing.T) {
	mailPath := filepath.Join(t.TempDir(), "control-mail.jsonl")
	const supervisor = "sup"
	commits := commitsN("a", 3)
	id := handoffEnvelopeID("lane", commits)

	first, err := enqueueReviewHandoff(context.Background(), mailPath, "pulse", supervisor, "s", "b", id)
	if err != nil || !first {
		t.Fatalf("first append must succeed: %v %v", first, err)
	}
	second, err := enqueueReviewHandoff(context.Background(), mailPath, "pulse", supervisor, "s", "b", id)
	if err != nil {
		t.Fatalf("a repeated identical handoff must not error: %v", err)
	}
	if second {
		t.Fatal("a repeated identical handoff must not be reported as newly queued")
	}
	pending, err := PendingReviewHandoffs(mailPath, supervisor)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("identical handoff must not duplicate, got %d", len(pending))
	}
}

// A CHANGED candidate set is new work and must enqueue separately, or new
// commits would be silently suppressed by the dedupe.
func TestChangedCandidateSetIsNewWork(t *testing.T) {
	mailPath := filepath.Join(t.TempDir(), "control-mail.jsonl")
	const supervisor = "sup"

	idA := handoffEnvelopeID("lane", commitsN("a", 2))
	idB := handoffEnvelopeID("lane", commitsN("a", 3))
	if idA == idB {
		t.Fatal("a different candidate set must produce a different id")
	}
	for _, id := range []string{idA, idB} {
		if queued, err := enqueueReviewHandoff(context.Background(), mailPath, "pulse", supervisor, "s", "b", id); err != nil || !queued {
			t.Fatalf("id %s must queue: %v %v", id, queued, err)
		}
	}
	pending, _ := PendingReviewHandoffs(mailPath, supervisor)
	if len(pending) != 2 {
		t.Fatalf("new work must not be suppressed by dedupe, got %d", len(pending))
	}
}

// The id must not depend on candidate ORDER: the same set discovered in a
// different order is the same handoff.
func TestIDIsOrderIndependent(t *testing.T) {
	a := []harvest.UnlandedCommit{{SHA: "111"}, {SHA: "222"}}
	b := []harvest.UnlandedCommit{{SHA: "222"}, {SHA: "111"}}
	if handoffEnvelopeID("lane", a) != handoffEnvelopeID("lane", b) {
		t.Fatal("candidate order must not change handoff identity")
	}
}
