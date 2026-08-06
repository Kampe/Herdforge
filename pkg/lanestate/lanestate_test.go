package lanestate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func at(t *testing.T) time.Time { t.Helper(); return time.Unix(1_800_000_000, 0).UTC() }

func outcome(results []Result, a Artifact) Outcome {
	for _, r := range results {
		if r.Artifact == a {
			return r.Outcome
		}
	}
	return ""
}

// A real ledger must never be overwritten by a reseed.
func TestExistingFilesAreNeverTouched(t *testing.T) {
	wt := t.TempDir()
	real := []byte(`{"lane":"smith","features":[{"id":"real"}]}`)
	if err := os.WriteFile(filepath.Join(wt, string(WorkState)), real, 0o644); err != nil {
		t.Fatal(err)
	}
	results := Seed(wt, "smith", t.TempDir(), at(t))
	if got := outcome(results, WorkState); got != Present {
		t.Fatalf("existing ledger = %s, want PRESENT", got)
	}
	body, _ := os.ReadFile(filepath.Join(wt, string(WorkState)))
	if string(body) != string(real) {
		t.Fatalf("ledger was overwritten: %s", body)
	}
}

// A verified snapshot always beats a blank template.
func TestSnapshotIsPreferredOverBlankTemplate(t *testing.T) {
	wt, stateRoot := t.TempDir(), t.TempDir()
	src := SnapshotDir(stateRoot, "smith")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	saved := []byte(`{"lane":"smith","features":[{"id":"carried-over"}]}`)
	if err := os.WriteFile(filepath.Join(src, string(WorkState)), saved, 0o644); err != nil {
		t.Fatal(err)
	}
	results := Seed(wt, "smith", stateRoot, at(t))
	if got := outcome(results, WorkState); got != Restored {
		t.Fatalf("work state = %s, want RESTORED", got)
	}
	body, _ := os.ReadFile(filepath.Join(wt, string(WorkState)))
	if string(body) != string(saved) {
		t.Fatalf("restored content = %s", body)
	}
	// No snapshot for progress, so that one falls back to a template.
	if got := outcome(results, LaneProgress); got != Seeded {
		t.Fatalf("lane progress = %s, want SEEDED", got)
	}
}

// The case that must never be silent.
func TestContinuityLostOnlyWhenBlankSeededDespitePriorRuns(t *testing.T) {
	blank := []Result{{Artifact: WorkState, Outcome: Seeded}}
	restored := []Result{{Artifact: WorkState, Outcome: Restored}}
	mixed := []Result{{Artifact: WorkState, Outcome: Restored}, {Artifact: LaneProgress, Outcome: Seeded}}
	present := []Result{{Artifact: WorkState, Outcome: Present}}

	if !ContinuityLost(blank, true) {
		t.Fatal("blank seed with prior traffic is lost continuity and must warn")
	}
	if ContinuityLost(blank, false) {
		t.Fatal("a genuine first run must not warn")
	}
	if ContinuityLost(restored, true) {
		t.Fatal("a restore preserved continuity; no warning")
	}
	// A partial restore proves the snapshot mechanism worked, so the blank
	// half is a genuinely absent artifact, not lost continuity.
	if ContinuityLost(mixed, true) {
		t.Fatal("partial restore must not report lost continuity")
	}
	if ContinuityLost(present, true) {
		t.Fatal("nothing seeded means nothing lost")
	}
}

// A freshly seeded ledger must read as incomplete, never as a clean clock-out.
func TestSeededLedgerHasNoFabricatedProgress(t *testing.T) {
	wt := t.TempDir()
	Seed(wt, "smith", t.TempDir(), at(t))
	raw, err := os.ReadFile(filepath.Join(wt, string(WorkState)))
	if err != nil {
		t.Fatal(err)
	}
	var ws struct {
		Lane     string `json:"lane"`
		Features []any  `json:"features"`
	}
	if err := json.Unmarshal(raw, &ws); err != nil {
		t.Fatalf("seeded ledger must be valid JSON: %v", err)
	}
	if ws.Lane != "smith" {
		t.Fatalf("lane = %q", ws.Lane)
	}
	if len(ws.Features) != 0 {
		t.Fatalf("seeded ledger must claim no completed work, got %v", ws.Features)
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	wt, stateRoot := t.TempDir(), t.TempDir()
	first := Seed(wt, "smith", stateRoot, at(t))
	if outcome(first, WorkState) != Seeded || outcome(first, LaneProgress) != Seeded {
		t.Fatalf("first run must seed both: %+v", first)
	}
	second := Seed(wt, "smith", stateRoot, at(t))
	for _, r := range second {
		if r.Outcome != Present {
			t.Fatalf("second run must touch nothing: %+v", r)
		}
	}
}
