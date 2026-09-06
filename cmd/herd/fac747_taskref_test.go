package main

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/pulse"
)

// FAC-747: the planner gates open_review on TaskRef, so the evidence pass MUST
// populate it for dispatched lanes. If it does not, the guard silently
// suppresses every open_review in production — a worse failure than the
// misselection it replaces, and one no planner-only test can catch.
func TestApplyReapEvidenceCarriesTaskRef(t *testing.T) {
	got := applyReapEvidence(pulse.AgentObservation{Name: "task-fac-218"}, "FAC-218", reapEvidence{})
	if got.TaskRef != "FAC-218" {
		t.Fatalf("TaskRef = %q, want %q — the open_review guard depends on this", got.TaskRef, "FAC-218")
	}
}

// The lanes that caused the incident carry no ref, which is exactly what makes
// them ineligible for open_review.
func TestApplyReapEvidenceLeavesTaskRefEmptyForNonDispatchedLanes(t *testing.T) {
	for _, name := range []string{
		"review-fac-746-b3da4492845e",
		"forge-scout-planner-39a9827d2b",
	} {
		ref := taskRefFromAgentName(name)
		if ref != "" {
			t.Fatalf("taskRefFromAgentName(%q) = %q, want empty: it was never dispatched for a task", name, ref)
		}
		got := applyReapEvidence(pulse.AgentObservation{Name: name}, ref, reapEvidence{
			committed: map[string]bool{name: true},
		})
		if got.TaskRef != "" {
			t.Fatalf("TaskRef = %q for %q, want empty", got.TaskRef, name)
		}
		// CommittedWork stays keyed on the agent name on purpose: a standing
		// lane's committed work is real and worth reporting. Selection, not
		// observation, is what must exclude it.
		if !got.CommittedWork {
			t.Fatalf("CommittedWork must remain observable for %q", name)
		}
	}
}

// A dispatched builder lane keeps the exact name shape the ref is derived from.
func TestTaskRefFromDispatchedLaneName(t *testing.T) {
	if got := taskRefFromAgentName("task-fac-745"); got != "FAC-745" {
		t.Fatalf("taskRefFromAgentName = %q, want FAC-745", got)
	}
}
