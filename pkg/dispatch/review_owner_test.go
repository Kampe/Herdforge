package dispatch

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

// TestReviewOwnerNeverResolvesToReadOnlyLane is the FAC-522 regression.
//
// A supervisor received the exact NEEDS_REVIEW handoff it exists to service,
// refused it because it was read-only, and resumed empty polling. The same
// contradiction was reachable from Herdforge's own resolver: the owner
// resolution order included the "reviewer" role, which is adversarial and
// read-only by its own prompt. A board without an explicit supervisor lane
// therefore addressed the review packet to a lane forbidden to act on it.
func TestReviewOwnerNeverResolvesToReadOnlyLane(t *testing.T) {
	for _, role := range []string{"reviewer", "assayer", "scout-planner", "recovery-sentinel", "verification-gate"} {
		if _, listed := readOnlyOwnerRoles[role]; !listed {
			t.Fatalf("read-only role %q must be refused as a queue owner", role)
		}
		for _, ordered := range reviewOwnerRoleOrder {
			if ordered == role {
				t.Fatalf("role %q is read-only and must not appear in the owner resolution order", role)
			}
		}
	}
}

// TestReviewSupervisorSkipsUnauthorizedOwner proves the resolver does not
// address a read-only lane even when it is the only review-ish lane present.
func TestReviewSupervisorSkipsUnauthorizedOwner(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{
		{Name: "assayer", Role: "reviewer"},
	}}
	d := &Dispatcher{Config: cfg}
	got := d.reviewSupervisorName()
	if strings.Contains(got, "assayer") {
		t.Fatalf("review packet addressed to a read-only lane: %q", got)
	}
	if !strings.Contains(got, "review-supervisor") {
		t.Fatalf("want the default supervisor identity when no authorized lane exists, got %q", got)
	}
}

// TestReviewSupervisorPrefersAuthorizedOwner keeps the happy path intact.
func TestReviewSupervisorPrefersAuthorizedOwner(t *testing.T) {
	cfg := &config.Config{Lanes: []config.LaneDef{
		{Name: "assayer", Role: "reviewer"},
		{Name: "review-supervisor", Role: "review-supervisor"},
	}}
	d := &Dispatcher{Config: cfg}
	if got := d.reviewSupervisorName(); !strings.Contains(got, "review-supervisor") {
		t.Fatalf("want the authorized supervisor lane, got %q", got)
	}
}

// TestUnauthorizedOwnerErrorNamesLaneHandoffAndConflict is acceptance
// criterion 2: the failure must name all three, not be swallowed.
func TestUnauthorizedOwnerErrorNamesLaneHandoffAndConflict(t *testing.T) {
	err := authorizeQueueOwner("assayer", "reviewer", "the review queue")
	if err == nil {
		t.Fatal("a read-only owner must fail closed")
	}
	msg := err.Error()
	for _, want := range []string{"assayer", "reviewer", "the review queue", "configuration error"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must name %q; got %q", want, msg)
		}
	}
}

// TestAuthorizedRolesPassAuthorization guards against over-refusal.
func TestAuthorizedRolesPassAuthorization(t *testing.T) {
	for _, role := range reviewOwnerRoleOrder {
		if err := authorizeQueueOwner("lane", role, "the review queue"); err != nil {
			t.Fatalf("authorized role %q was refused: %v", role, err)
		}
	}
}
