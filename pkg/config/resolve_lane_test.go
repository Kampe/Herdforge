package config

import "testing"

func laneCfg(lanes ...LaneDef) *Config { return &Config{Lanes: lanes} }

// The bug: the dispatch default is "worker", a ROLE, and no lane is named that.
func TestResolveLaneFindsALaneByItsRoleWhenNoLaneCarriesThatName(t *testing.T) {
	cfg := laneCfg(
		LaneDef{Name: "scout", Role: "forge-smith"},
		LaneDef{Name: "smith", Role: "worker"},
	)
	lane, err := ResolveLane(cfg, "worker")
	if err != nil {
		t.Fatalf("bare dispatch default must resolve: %v", err)
	}
	if lane.Name != "smith" {
		t.Fatalf("resolved %q, want smith", lane.Name)
	}
}

// An explicitly named lane must never be overridden by a role match elsewhere.
func TestResolveLanePrefersAnExactNameOverAnyRoleMatch(t *testing.T) {
	cfg := laneCfg(
		LaneDef{Name: "smith", Role: "recovery"},
		LaneDef{Name: "recovery", Role: "worker"},
	)
	lane, err := ResolveLane(cfg, "recovery")
	if err != nil {
		t.Fatal(err)
	}
	if lane.Name != "recovery" {
		t.Fatalf("name match lost to role match: got %q", lane.Name)
	}
}

// Validate only rejects duplicate STANDING role owners, so two ephemeral lanes
// may legitimately share a role. Binding the caller to whichever came first in
// the YAML would be a silent wrong-lane launch — a reviewer caught exactly that
// in the first version of this change.
func TestResolveLaneRefusesAnAmbiguousRoleInsteadOfPickingTheFirst(t *testing.T) {
	cfg := laneCfg(
		LaneDef{Name: "smith", Role: "worker"},
		LaneDef{Name: "smith-claude", Role: "worker"},
	)
	lane, err := ResolveLane(cfg, "worker")
	if err == nil {
		t.Fatalf("ambiguous role silently resolved to %q", lane.Name)
	}
	// Deliberately non-overlapping names: asserting both "smith" and
	// "smith-claude" was one assertion wearing two hats, since the first is a
	// substring of the second and could never fail independently.
	cfg = laneCfg(
		LaneDef{Name: "anvil", Role: "worker"},
		LaneDef{Name: "bellows", Role: "worker"},
	)
	if _, err = ResolveLane(cfg, "worker"); err == nil {
		t.Fatal("ambiguous role silently resolved")
	}
	for _, want := range []string{"ambiguous", "anvil", "bellows"} {
		if !contains(err.Error(), want) {
			t.Errorf("error must name the collision (%q missing): %v", want, err)
		}
	}
}

func TestResolveLaneReportsAnUnknownLaneAsBothLookupsFailing(t *testing.T) {
	if _, err := ResolveLane(laneCfg(LaneDef{Name: "smith", Role: "worker"}), "nope"); err == nil {
		t.Fatal("unknown lane must fail closed")
	}
	if _, err := ResolveLane(nil, "worker"); err == nil {
		t.Fatal("nil config must fail closed")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
