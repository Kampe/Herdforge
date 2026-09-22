package herdr

import (
	"strings"
	"testing"
)

func TestReviewAbortLiveConflictExactDoneIsAllowed(t *testing.T) {
	focused := false
	agents := []AgentEntry{{
		Name: "review-abort-fixture", Status: "done", PaneID: "wK:pABORTTEST", TabID: "wK:tABORTTEST",
		Focused: &focused, Session: AgentSession{Value: "sess-abort-fixture"},
	}}
	if err := ReviewAbortLiveConflict(agents, "review-abort-fixture", "wK:pABORTTEST", "wK:tABORTTEST", "sess-abort-fixture"); err != nil {
		t.Fatal(err)
	}
}

func TestReviewAbortLiveConflictSameNameDifferentPane(t *testing.T) {
	agents := []AgentEntry{{
		Name: "review-abort-fixture", Status: "done", PaneID: "wK:pOTHER", TabID: "wK:tOTHER",
		Session: AgentSession{Value: "sess-abort-fixture"},
	}}
	err := ReviewAbortLiveConflict(agents, "review-abort-fixture", "wK:pABORTTEST", "wK:tABORTTEST", "sess-abort-fixture")
	if err == nil || !strings.Contains(err.Error(), "conflicting live identity") {
		t.Fatalf("want conflicting live identity, got %v", err)
	}
}

func TestReviewAbortLiveConflictSamePaneDifferentName(t *testing.T) {
	agents := []AgentEntry{{
		Name: "other-reviewer", Status: "done", PaneID: "wK:pABORTTEST", TabID: "wK:tABORTTEST",
		Session: AgentSession{Value: "sess-abort-fixture"},
	}}
	err := ReviewAbortLiveConflict(agents, "review-abort-fixture", "wK:pABORTTEST", "wK:tABORTTEST", "sess-abort-fixture")
	if err == nil || !strings.Contains(err.Error(), "conflicting live identity") {
		t.Fatalf("want conflicting live identity, got %v", err)
	}
}

func TestReviewAbortLiveConflictSameSessionDifferentPane(t *testing.T) {
	agents := []AgentEntry{{
		Name: "other-reviewer", Status: "done", PaneID: "wK:pOTHER", TabID: "wK:tOTHER",
		Session: AgentSession{Value: "sess-abort-fixture"},
	}}
	err := ReviewAbortLiveConflict(agents, "review-abort-fixture", "wK:pABORTTEST", "wK:tABORTTEST", "sess-abort-fixture")
	if err == nil || !strings.Contains(err.Error(), "conflicting live identity") {
		t.Fatalf("want conflicting live identity, got %v", err)
	}
}

func TestReviewAbortLiveConflictExactWorkingRefused(t *testing.T) {
	focused := false
	agents := []AgentEntry{{
		Name: "review-abort-fixture", Status: "working", PaneID: "wK:pABORTTEST", TabID: "wK:tABORTTEST",
		Focused: &focused, Session: AgentSession{Value: "sess-abort-fixture"},
	}}
	err := ReviewAbortLiveConflict(agents, "review-abort-fixture", "wK:pABORTTEST", "wK:tABORTTEST", "sess-abort-fixture")
	if err == nil || !strings.Contains(err.Error(), "live useful work") {
		t.Fatalf("want live useful work refuse, got %v", err)
	}
}
