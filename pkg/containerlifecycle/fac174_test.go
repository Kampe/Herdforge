package containerlifecycle

import "testing"

func TestLabelFAC174BaselineSplitsKnownFromNew(t *testing.T) {
	if len(FAC174LegacyBaseline) != 18 {
		t.Fatalf("FAC174LegacyBaseline has %d entries, want 18 (see docs/fac-200-fac174-reconciliation-plan.md)", len(FAC174LegacyBaseline))
	}
	seen := map[string]bool{}
	for _, id := range FAC174LegacyBaseline {
		if len(id) != 64 {
			t.Errorf("baseline id %q is %d chars, want a full 64-char docker id", id, len(id))
		}
		if seen[id] {
			t.Errorf("duplicate baseline id %q", id)
		}
		seen[id] = true
	}

	unowned := []LiveContainer{
		{ID: FAC174LegacyBaseline[3], Image: "fac174-hermetic:x"},
		{ID: "brand-new-straggler-not-in-the-plan", Image: "some-other-image"},
		{ID: FAC174LegacyBaseline[0], Image: "fac174-hermetic:y"},
	}
	baseline, other := LabelFAC174Baseline(unowned)
	if len(baseline) != 2 {
		t.Fatalf("baseline = %+v, want 2", baseline)
	}
	if baseline[0].ID > baseline[1].ID {
		t.Fatalf("baseline not sorted by id: %+v", baseline)
	}
	if len(other) != 1 || other[0].ID != "brand-new-straggler-not-in-the-plan" {
		t.Fatalf("other = %+v, want exactly the new straggler", other)
	}
}

func TestLabelFAC174BaselineNeverMutatesInput(t *testing.T) {
	unowned := []LiveContainer{{ID: FAC174LegacyBaseline[5]}}
	before := unowned[0]
	_, _ = LabelFAC174Baseline(unowned)
	if unowned[0] != before {
		t.Fatalf("LabelFAC174Baseline mutated its input slice")
	}
}
