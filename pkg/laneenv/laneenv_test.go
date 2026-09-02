package laneenv

import (
	"os"
	"path/filepath"
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
	// Managed verification must not let inherited routing policy change the
	// behavior of an in-process package test.
	t.Setenv("HERD_MODE", "production")
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("HERDR_PANE", "wK:p1")
	t.Setenv("HERDR_WORKSPACE", "wK")

	Strip()

	if leaked := Leaked(); len(leaked) != 0 {
		t.Fatalf("launch metadata survived Strip: %v", leaked)
	}
	for _, v := range []string{"HERD_MODE", "HERD_USE_PI"} {
		if _, ok := os.LookupEnv(v); ok {
			t.Fatalf("routing metadata survived Strip: %s", v)
		}
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
func TestIsolateDefaultSlotDirDoesNotRestoreFleetMetadata(t *testing.T) {
	Strip()
	restore, err := IsolateDefaultSlotDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
	if leaked := Leaked(); len(leaked) != 0 {
		t.Fatalf("slot-dir isolation restored launch metadata: %v", leaked)
	}
	dir := os.Getenv(nestedSlotDirVar)
	if dir == "" {
		t.Fatal("IsolateDefaultSlotDir left HERD_HEAVY_PHASE_SLOT_DIR empty")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("isolated slot dir missing: %v", err)
	}
	shared := filepath.Join(os.TempDir(), "herd-heavy-phase-slots")
	if filepath.Clean(dir) == filepath.Clean(shared) {
		t.Fatal("package isolation reused the shared host slot directory")
	}
}

func TestIsolateDefaultSlotDirCreatesUniqueDirectories(t *testing.T) {
	first, err := IsolateDefaultSlotDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first)
	dirA := os.Getenv(nestedSlotDirVar)
	second, err := IsolateDefaultSlotDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second)
	dirB := os.Getenv(nestedSlotDirVar)
	if dirA == "" || dirB == "" || dirA == dirB {
		t.Fatalf("package slot dirs collided: %q %q", dirA, dirB)
	}
}

func TestIsolateDefaultSlotDirRemovesDirectoryAndRestoresEnv(t *testing.T) {
	prev := filepath.Join(t.TempDir(), "previous-slots")
	t.Setenv(nestedSlotDirVar, prev)

	restore, err := IsolateDefaultSlotDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv(nestedSlotDirVar)
	if dir == "" || dir == prev {
		t.Fatalf("isolation did not replace prior slot dir: got %q", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("isolated slot dir missing before cleanup: %v", err)
	}

	restore()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("watched RED cleanup: isolated dir retained after restore: %v", err)
	}
	if got := os.Getenv(nestedSlotDirVar); got != prev {
		t.Fatalf("cleanup restored %s=%q, want prior %q", nestedSlotDirVar, got, prev)
	}

	restore()
	if got := os.Getenv(nestedSlotDirVar); got != prev {
		t.Fatalf("second restore changed %s to %q", nestedSlotDirVar, got)
	}
}

func TestIsolateDefaultSlotDirRestoresAbsentEnv(t *testing.T) {
	if err := os.Unsetenv(nestedSlotDirVar); err != nil {
		t.Fatal(err)
	}
	restore, err := IsolateDefaultSlotDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv(nestedSlotDirVar)
	if dir == "" {
		t.Fatal("isolation left HERD_HEAVY_PHASE_SLOT_DIR empty")
	}
	restore()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("watched RED cleanup: isolated dir retained after restore: %v", err)
	}
	if _, ok := os.LookupEnv(nestedSlotDirVar); ok {
		t.Fatal("cleanup invented HERD_HEAVY_PHASE_SLOT_DIR after an absent prior value")
	}
}

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
