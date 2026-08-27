package reviewingest

import (
	"errors"
	"strings"
	"testing"
)

// FAC-704: PR 3468 carried an exact-SHA PASS that had reviewed only the
// immediate-parent repair commit. Head matched, every gate passed, and the rest
// of the range merged unexamined.
func TestNarrowerRangeIsRefusedEvenWhenTheHeadMatches(t *testing.T) {
	// claimed base is a DESCENDANT of the true base: the reviewer started
	// partway through.
	isAncestor := func(ancestor, descendant string) (bool, error) {
		return ancestor == "trueBase" && descendant == "repairCommit", nil
	}
	v, reason, err := CheckReviewedRange("repairCommit", "head", "trueBase", "head", isAncestor)
	if err != nil {
		t.Fatal(err)
	}
	if v != RangeNarrower {
		t.Fatalf("a partial review was admitted as complete: %s", v)
	}
	if !strings.Contains(reason, "UNEXAMINED") {
		t.Fatalf("refusal does not name the consequence: %s", reason)
	}
}

func TestExactRangeCovers(t *testing.T) {
	isAncestor := func(string, string) (bool, error) { return true, nil }
	if v, _, _ := CheckReviewedRange("trueBase", "head", "trueBase", "head", isAncestor); v != RangeCovers {
		t.Fatalf("an exact range was not accepted: %s", v)
	}
}

func TestWiderRangeStillCovers(t *testing.T) {
	// Reading MORE than the candidate spans is complete, not a violation.
	// trueBase is NOT an ancestor of the claimed (earlier) base.
	isAncestor := func(ancestor, descendant string) (bool, error) { return false, nil }
	if v, _, _ := CheckReviewedRange("olderBase", "head", "trueBase", "head", isAncestor); v != RangeCovers {
		t.Fatalf("a wider review was refused: %s", v)
	}
}

func TestMissingBaseIsUnverifiableNotAPass(t *testing.T) {
	// The #3468 shape before the field existed. Absence must be surfaced, never
	// silently accepted -- and it is not a FAILURE either.
	v, reason, _ := CheckReviewedRange("", "head", "trueBase", "head", nil)
	if v != RangeUnverifiable {
		t.Fatalf("a missing base was classified as %s", v)
	}
	if !strings.Contains(reason, "where it started") {
		t.Fatalf("reason does not explain what an endpoint cannot prove: %s", reason)
	}
}

func TestHeadMismatchIsNarrower(t *testing.T) {
	isAncestor := func(string, string) (bool, error) { return false, nil }
	if v, _, _ := CheckReviewedRange("trueBase", "otherHead", "trueBase", "head", isAncestor); v != RangeNarrower {
		t.Fatalf("a reviewer that stopped early was admitted: %s", v)
	}
}

func TestAncestryFailureIsUnverifiableNotACover(t *testing.T) {
	// If ancestry cannot be determined, the range is unknown. Returning
	// "covers" would fail OPEN on exactly the check that exists to catch a
	// partial review.
	isAncestor := func(string, string) (bool, error) { return false, errors.New("bad object") }
	v, reason, _ := CheckReviewedRange("someBase", "head", "trueBase", "head", isAncestor)
	if v != RangeUnverifiable {
		t.Fatalf("an undeterminable ancestry was treated as %s", v)
	}
	if !strings.Contains(reason, "bad object") {
		t.Fatalf("underlying cause swallowed: %s", reason)
	}
}
