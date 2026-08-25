package next

import (
	"strings"
	"testing"
)

// FAC-623: zero claimable with a role filter means NOTHING MATCHED, not that the
// queue is idle. The old wording ("No blocking actions") led an operator to
// conclude the review-in-progress cap was throttling dispatch. It was not: the
// cap is advisory and never gates claiming, and the real cause was that no
// pending card carried the requested role label.
func TestClaimPreview_ZeroWithRoleSaysWhy(t *testing.T) {
	got := ClaimPreview{Role: "scout-planner"}.Description()

	if strings.Contains(got, "No blocking actions") {
		t.Fatalf("zero claimable must not read as a healthy idle queue: %q", got)
	}
	if !strings.Contains(got, "scout-planner") {
		t.Errorf("must name the role that matched nothing: %q", got)
	}
	if !strings.Contains(got, "filter result") {
		t.Errorf("must say this is a filter result, not an absence of work: %q", got)
	}
}

// Without a role filter the same distinction holds, minus the role name.
func TestClaimPreview_ZeroWithoutRoleStillSaysFilterResult(t *testing.T) {
	got := ClaimPreview{}.Description()
	if strings.Contains(got, "No blocking actions") {
		t.Fatalf("zero claimable must not read as idle: %q", got)
	}
	if !strings.Contains(got, "filter result") {
		t.Errorf("must say filter result: %q", got)
	}
}

// A real non-zero result must still report normally, so the guard cannot be
// satisfied by never saying "No blocking actions".
func TestClaimPreview_NonZeroIsStillHealthy(t *testing.T) {
	got := ClaimPreview{Claimable: 7}.Description()
	if !strings.Contains(got, "No blocking actions") || !strings.Contains(got, "7") {
		t.Fatalf("a real claimable count must report normally: %q", got)
	}
}
