package lifecycle

import (
	"errors"
	"strings"
	"testing"
)

// A role may be owned by several lanes. The registry that previously rejected
// a second lane with the same role must now accept it — standing improvement
// fleets (ci-warden, mender) carry the same control role.
func TestRegistryAcceptsMultipleLanesPerRole(t *testing.T) {
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{
		{Name: "ci-warden", Role: "assayer", Standing: true},
		{Name: "mender", Role: "assayer", Standing: true},
	})
	if err != nil {
		t.Fatalf("duplicate role must be accepted: %v", err)
	}
	names := registry.LaneNames()
	if len(names) != 2 || names[0] != "ci-warden" || names[1] != "mender" {
		t.Fatalf("lane names=%v", names)
	}
}

// A duplicate lane NAME is still a config error — names are the primary key.
func TestRegistryRejectsDuplicateLaneName(t *testing.T) {
	if _, err := NewCanonicalLaneRegistry([]CanonicalLane{
		{Name: "smith", Role: "worker"},
		{Name: "smith", Role: "reviewer"},
	}); err == nil {
		t.Fatal("duplicate lane name must be rejected")
	}
}

// ResolveRole must refuse to pick one lane when a role is shared, naming every
// candidate so the caller can disambiguate by naming a lane explicitly.
func TestResolveRoleAmbiguousNamesAllCandidates(t *testing.T) {
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{
		{Name: "ci-warden", Role: "assayer"},
		{Name: "mender", Role: "assayer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.ResolveRole("assayer")
	if !errors.Is(err, ErrAmbiguousRole) {
		t.Fatalf("ambiguous role must return ErrAmbiguousRole, got %v", err)
	}
	msg := err.Error()
	// Deliberately non-overlapping names: asserting both was one assertion
	// wearing two hats, since one could be a substring of the other.
	for _, want := range []string{"ci-warden", "mender"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must name candidate %q: %v", want, err)
		}
	}
}

// A unique role still resolves to its single lane.
func TestResolveRoleUniqueReturnsSingleLane(t *testing.T) {
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{
		{Name: "smith", Role: "worker"},
		{Name: "scout", Role: "forge-smith"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lane, err := registry.ResolveRole("worker")
	if err != nil || lane.Name != "smith" {
		t.Fatalf("resolve worker: lane=%+v err=%v", lane, err)
	}
}

// Resolve must prefer an exact lane name over an ambiguous role match — the
// caller used a name, so they already disambiguated.
func TestResolvePrefersLaneNameOverAmbiguousRole(t *testing.T) {
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{
		{Name: "ci-warden", Role: "assayer"},
		{Name: "mender", Role: "assayer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lane, err := registry.Resolve("ci-warden")
	if err != nil || lane.Name != "ci-warden" {
		t.Fatalf("name must win over ambiguous role: lane=%+v err=%v", lane, err)
	}
}

// Resolve must propagate the ambiguity error (with candidate names) when the
// value matches no lane name but the role is shared.
func TestResolvePropagatesAmbiguityWhenNameMisses(t *testing.T) {
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{
		{Name: "ci-warden", Role: "assayer"},
		{Name: "mender", Role: "assayer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Resolve("assayer")
	if !errors.Is(err, ErrAmbiguousRole) {
		t.Fatalf("ambiguous role via Resolve must return ErrAmbiguousRole, got %v", err)
	}
	if !strings.Contains(err.Error(), "ci-warden") || !strings.Contains(err.Error(), "mender") {
		t.Fatalf("ambiguity error must name candidates: %v", err)
	}
}

// ResolveLaneName must still resolve a lane by name even when its role is
// shared with another lane.
func TestResolveLaneNameWorksWithSharedRole(t *testing.T) {
	registry, err := NewCanonicalLaneRegistry([]CanonicalLane{
		{Name: "ci-warden", Role: "assayer"},
		{Name: "mender", Role: "assayer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lane, err := registry.ResolveLaneName("mender")
	if err != nil || lane.Name != "mender" || lane.Role != "assayer" {
		t.Fatalf("resolve mender: lane=%+v err=%v", lane, err)
	}
}
