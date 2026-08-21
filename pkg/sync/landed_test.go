package sync

import (
	"os"
	"strings"
	"testing"
)

func validDisposition() LandedDisposition {
	return LandedDisposition{
		Ref:          "CHA-2183",
		CandidateSHA: "fc3bd108309667d67493a09efc9725f47b15452f",
		MergeSHA:     "14ca5bae82606e825fbd78d2abcd8463d9a07175",
		Branch:       "reconstruct/cha-2183-current-main-refresh",
		Method:       LandedByRebaseEmptyDiff,
	}
}

// TestLandedDispositionRoundTrip is the FAC-565 case: a verdict admitted before
// the merge carries no merge_sha, so an already-merged candidate had no way to
// prove its merge disposition without inventing pre-merge provenance.
func TestLandedDispositionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	written, err := WriteLandedDisposition(dir, validDisposition())
	if err != nil {
		t.Fatal(err)
	}
	if written.Actor == "" || written.ObservedAt == "" {
		t.Fatalf("observation must be attributed and timestamped: %+v", written)
	}
	// It must state plainly that it is not a receipt.
	if !strings.Contains(written.Note, "NOT a completion receipt") {
		t.Fatalf("disposition must disclaim being a receipt, got %q", written.Note)
	}

	back, err := ReadLandedDisposition(dir, "CHA-2183")
	if err != nil || back == nil {
		t.Fatalf("round trip failed: %v %v", back, err)
	}
	if back.MergeSHA != validDisposition().MergeSHA {
		t.Fatalf("merge sha lost: %+v", back)
	}
}

// An incomplete disposition is refused: it could otherwise be read as evidence
// about a different object.
func TestLandedDispositionRefusesIncomplete(t *testing.T) {
	dir := t.TempDir()
	for name, mutate := range map[string]func(LandedDisposition) LandedDisposition{
		"no ref":       func(d LandedDisposition) LandedDisposition { d.Ref = ""; return d },
		"no candidate": func(d LandedDisposition) LandedDisposition { d.CandidateSHA = ""; return d },
		"no merge":     func(d LandedDisposition) LandedDisposition { d.MergeSHA = ""; return d },
		"no method":    func(d LandedDisposition) LandedDisposition { d.Method = ""; return d },
	} {
		if _, err := WriteLandedDisposition(dir, mutate(validDisposition())); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

// A disposition must only satisfy the candidate it is about.
func TestBindsCandidateRejectsOtherObjects(t *testing.T) {
	d := validDisposition()
	if !d.BindsCandidate(d.CandidateSHA) {
		t.Fatal("must bind its own candidate")
	}
	if !d.BindsCandidate("fc3bd108") {
		t.Fatal("an abbreviated candidate prefix must bind")
	}
	if d.BindsCandidate("14ca5bae82606e825fbd78d2abcd8463d9a07175") {
		t.Fatal("the MERGE sha must not be accepted as the candidate")
	}
	if d.BindsCandidate("deadbeefdeadbeef") || d.BindsCandidate("") || d.BindsCandidate("fc3") {
		t.Fatal("unrelated, empty, or too-short values must not bind")
	}
}

// Missing is not an error; corrupt fails closed so no weaker path proceeds as
// if no landing had been claimed.
func TestReadLandedMissingVsCorrupt(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadLandedDisposition(dir, "CHA-1")
	if err != nil || got != nil {
		t.Fatalf("missing must be (nil, nil), got %v %v", got, err)
	}
	if _, err := WriteLandedDisposition(dir, validDisposition()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LandedPath(dir, "CHA-2183"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLandedDisposition(dir, "CHA-2183"); err == nil {
		t.Fatal("a corrupt disposition must fail closed")
	}
}
