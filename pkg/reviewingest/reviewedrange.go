package reviewingest

import "fmt"

// RangeVerdict classifies a reviewer's claimed review range against the range
// the candidate actually spans.
type RangeVerdict string

const (
	// RangeCovers means the reviewer read at least the whole candidate.
	RangeCovers RangeVerdict = "covers"
	// RangeNarrower means the reviewer read LESS than the candidate spans.
	// Commits merged without being examined.
	RangeNarrower RangeVerdict = "narrower"
	// RangeUnverifiable means the artifact does not state a base, so no range
	// claim exists to check. NOT a pass and NOT a failure: an absence.
	RangeUnverifiable RangeVerdict = "unverifiable"
)

// AncestryFn reports whether ancestor is an ancestor of descendant. Injected so
// the decision is testable without a repository.
type AncestryFn func(ancestor, descendant string) (bool, error)

// CheckReviewedRange decides whether a verdict examined the whole candidate.
//
// FAC-704: PR 3468 carried an exact-SHA PASS that had reviewed only the
// immediate-parent repair commit. The head matched, every existing gate passed,
// and the rest of the range was never read. An artifact that records only an
// ENDPOINT cannot detect that -- a reviewer who read one commit and one who
// read fifty produce identical provenance. Only a base can.
//
// The test is ancestry, not equality. A reviewer may legitimately read MORE
// than the candidate spans (a wider base is still complete), so the refusal
// fires only when the claimed base is a strict DESCENDANT of the true merge
// base, which is precisely "started reading partway through".
func CheckReviewedRange(claimedBase, claimedHead, trueBase, trueHead string, isAncestor AncestryFn) (RangeVerdict, string, error) {
	if claimedBase == "" {
		return RangeUnverifiable, "artifact states no reviewed-base, so the review RANGE is unknown; " +
			"an exact head match proves only where the reviewer stopped, never where it started", nil
	}
	if trueBase == "" || trueHead == "" {
		return RangeUnverifiable, "candidate merge-base/head not resolved, so no range comparison is possible", nil
	}
	if claimedHead != "" && claimedHead != trueHead {
		return RangeNarrower, fmt.Sprintf("reviewer read head %s but the candidate head is %s", short(claimedHead), short(trueHead)), nil
	}

	// Claimed base strictly after the true base means commits between them were
	// never examined.
	after, err := isAncestor(trueBase, claimedBase)
	if err != nil {
		return RangeUnverifiable, "ancestry between the claimed base and the candidate base could not be determined: " + err.Error(), nil
	}
	if after && claimedBase != trueBase {
		return RangeNarrower, fmt.Sprintf(
			"reviewer read from %s but the candidate starts at %s: every commit between them merged UNEXAMINED",
			short(claimedBase), short(trueBase)), nil
	}
	return RangeCovers, "", nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
