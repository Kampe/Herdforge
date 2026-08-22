package invariant

import "testing"

// TestConsolidationIsNotAViolation is the FAC-557 correction to this gate.
//
// It flagged a change that consolidated three copies of the ancestry check into
// one, because the surviving copy lived in a file the baseline had never seen.
// Failing a change that strictly REDUCES duplication is worse than not firing:
// the next person reaches for the baseline regenerator, and a gate baselined
// away once is gone for good.
func TestConsolidationIsNotAViolation(t *testing.T) {
	base := &Baseline{Inherited: map[string][]string{
		"--is-ancestor": {"pkg/a/one.go", "pkg/a/two.go", "pkg/a/three.go"},
	}}
	// Three copies became one, in a brand-new file.
	consolidated := []Occurrence{{Literal: "--is-ancestor", Files: []string{"pkg/a/single.go"}}}
	if v := NewViolations(consolidated, base); len(v) != 0 {
		t.Errorf("consolidating 3 locations into 1 must not be a violation, got %+v", v)
	}
	// Moving a copy without changing the count is neutral.
	moved := []Occurrence{{Literal: "--is-ancestor", Files: []string{"pkg/a/one.go", "pkg/a/two.go", "pkg/a/moved.go"}}}
	if v := NewViolations(moved, base); len(v) != 0 {
		t.Errorf("relocating a copy must not be a violation, got %+v", v)
	}
	// Genuinely adding a location still fails, which is the whole point.
	added := []Occurrence{{Literal: "--is-ancestor", Files: []string{
		"pkg/a/one.go", "pkg/a/two.go", "pkg/a/three.go", "pkg/a/four.go"}}}
	if v := NewViolations(added, base); len(v) != 1 {
		t.Fatalf("adding a 4th location must fail the gate, got %+v", v)
	}
	// A literal absent from the baseline entirely is still new.
	fresh := []Occurrence{{Literal: "brand/new/rule", Files: []string{"pkg/a/x.go", "pkg/a/y.go"}}}
	if v := NewViolations(fresh, base); len(v) != 1 {
		t.Errorf("an unbaselined duplicate must still fail, got %+v", v)
	}
}
