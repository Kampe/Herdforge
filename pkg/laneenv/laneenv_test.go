package laneenv

import (
	"os"
	"strings"
	"testing"
)

// FAC-610: Strip must actually remove the metadata. A suite that calls it and
// assumes it worked is trusting a reading without checking what produced it --
// which is how the phantom failures went unexplained for hours.
func TestStripRemovesEveryLaunchVariable(t *testing.T) {
	for _, v := range Vars {
		t.Setenv(v, "set-by-launch")
	}
	t.Setenv("HERDR_PANE", "wK:p1")
	t.Setenv("HERDR_WORKSPACE", "wK")

	Strip()

	if leaked := Leaked(); len(leaked) != 0 {
		t.Fatalf("launch metadata survived Strip: %v", leaked)
	}
}

// The prefix sweep must be a sweep, not an enumeration: herdr can add a pane
// variable tomorrow and a hardcoded list would silently stop covering it.
func TestStripSweepsUnknownHerdrVariables(t *testing.T) {
	t.Setenv("HERDR_SOMETHING_INVENTED_LATER", "x")

	Strip()

	if _, ok := os.LookupEnv("HERDR_SOMETHING_INVENTED_LATER"); ok {
		t.Fatal("an unenumerated HERDR_ variable survived; the sweep is really an allowlist")
	}
}

// Stripping must not touch unrelated environment. A test suite that clears the
// wrong thing trades one invisible failure for another.
func TestStripLeavesUnrelatedEnvironmentAlone(t *testing.T) {
	t.Setenv("HOME_AWAY_FROM_HOME", "keep-me")
	t.Setenv("HERDFORGE_NOT_A_LANE_VAR", "keep-me-too")

	Strip()

	for _, k := range []string{"HOME_AWAY_FROM_HOME", "HERDFORGE_NOT_A_LANE_VAR"} {
		if v := os.Getenv(k); v == "" || !strings.HasPrefix(v, "keep-me") {
			t.Fatalf("Strip cleared unrelated variable %s", k)
		}
	}
}
