package main

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/pulse"
)

func lane() pulse.AgentObservation {
	return pulse.AgentObservation{
		Name: "forge-api-crusader", TabID: "wB:t3EW",
		Workspace: "wB", Worktree: "/wt/api-crusader",
	}
}

// TestPacketNamesEveryUnlandedCandidate is the FAC-566 regression.
//
// The handoff carried only lane, tab and workspace. A receiver could not confirm
// it was still valid, and a lane holding many unlanded commits collapsed into one
// ambiguous assertion -- an observed lane had 29 unlanded against 8 already
// patch-equivalent, which a tab-level handoff cannot express.
func TestPacketNamesEveryUnlandedCandidate(t *testing.T) {
	commits := []harvest.UnlandedCommit{
		{SHA: "aaaa111122223333444455556666777788889999", Subject: "feat: one"},
		{SHA: "bbbb111122223333444455556666777788889999", Subject: "feat: two"},
	}
	got := pulseReviewPacket(lane(), commits, nil)

	for _, want := range []string{
		"forge-api-crusader", "wB:t3EW", "/wt/api-crusader",
		"aaaa111122223333444455556666777788889999", "feat: one",
		"bbbb111122223333444455556666777788889999", "feat: two",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("packet must name %q; got:\n%s", want, got)
		}
	}
	// Multiple candidates must not read as one harvestable tip.
	if !strings.Contains(got, "own receipt") {
		t.Fatalf("multi-candidate packet must say each needs its own receipt:\n%s", got)
	}
	// The receiver must be told not to trust a stale pane transcript.
	if !strings.Contains(got, "stale transcript") {
		t.Fatalf("packet must warn about stale pane transcripts:\n%s", got)
	}
}

// A lane with nothing unlanded must be stated as such, never implied to hold a
// candidate. This is the CHA-2206 case that was re-emitted after merging.
func TestPacketStatesNoCandidates(t *testing.T) {
	got := pulseReviewPacket(lane(), nil, nil)
	if !strings.Contains(got, "CANDIDATES: NONE") {
		t.Fatalf("an empty lane must say so explicitly:\n%s", got)
	}
	if !strings.Contains(got, "do not open a review") {
		t.Fatalf("packet must tell the receiver not to open a review:\n%s", got)
	}
}

// An unresolvable lane must be labelled unresolved rather than looking verified.
func TestPacketFailsClosedOnResolveError(t *testing.T) {
	got := pulseReviewPacket(lane(), nil, errResolve)
	if !strings.Contains(got, "UNRESOLVED") {
		t.Fatalf("a resolve failure must be labelled:\n%s", got)
	}
	if strings.Contains(got, "CANDIDATES: NONE") {
		t.Fatal("a resolve failure must not be reported as an empty lane")
	}
	if !strings.Contains(got, "Do not treat this as a candidate assertion") {
		t.Fatalf("packet must disclaim being a candidate assertion:\n%s", got)
	}
}

var errResolve = &resolveError{}

type resolveError struct{}

func (*resolveError) Error() string { return "worktree unreadable" }
