package router

import (
	"strings"
	"testing"
)

// familyRouter is hermetic: no CLI is present, so Pick never reaches a live
// provider probe. The exclusion guard must fire before any of that anyway --
// that is the point of validating up front.
func familyRouter(t *testing.T) *SurfaceRouter {
	t.Helper()
	clearRouteEnv(t)
	return testRouter(nil)
}

// FAC-595: an unrecognized excluded family was DROPPED, not refused.
//
//	if excludedFamily != "" && family == excludedFamily { continue }
//
// A name that is not a family matches nothing, so the filter silently does
// nothing and routing proceeds onto the very family the caller meant to
// exclude. Measured on the sibling surface: `--exclude-family grok` and
// `--exclude-family totally-bogus-family` both routed to grok, because the
// family key is "xai".
//
// This is not a typo nuisance. --exclude-family is what makes an R1-R3 review
// family-DISJOINT from the author. A silently ignored exclusion can route a
// review onto the author's own family and produce a verdict that looks valid
// and is inadmissible. The router already refuses an unknown SHAPE
// ("herd-route: unknown task shape"); a family deserves the same treatment.
func TestUnknownExcludedFamilyIsRefusedNotDropped(t *testing.T) {
	r := familyRouter(t)
	for _, bogus := range []string{"grok", "totally-bogus-family", "Anthropic "} {
		_, err := r.Pick("qa", "", bogus)
		if err == nil {
			t.Fatalf("unknown excluded family %q was accepted and silently dropped", bogus)
		}
		if !strings.Contains(err.Error(), "unknown model family") {
			t.Fatalf("refusal for %q does not name the fault: %v", bogus, err)
		}
		// The remedy is unusable without the valid names.
		if !strings.Contains(err.Error(), "xai") {
			t.Fatalf("refusal for %q does not name the valid families: %v", bogus, err)
		}
	}
}

// A real family must still be accepted, or the guard has just broken routing.
func TestKnownExcludedFamilyIsStillAccepted(t *testing.T) {
	r := familyRouter(t)
	for _, family := range KnownFamilies() {
		if _, err := r.Pick("qa", "", family); err != nil &&
			strings.Contains(err.Error(), "unknown model family") {
			t.Fatalf("valid family %q was refused: %v", family, err)
		}
	}
	// The empty string means "no exclusion" and must never be refused.
	if _, err := r.Pick("qa", "", ""); err != nil &&
		strings.Contains(err.Error(), "unknown model family") {
		t.Fatalf("empty exclusion was treated as an unknown family: %v", err)
	}
}

// Anti-drift: KnownFamilies must cover every family FamilyFor can actually
// return for a routed surface. Without this, adding a provider whose family is
// new would make that family unusable as an exclusion -- the guard would refuse
// a legitimate name. AllShapes/Waterfall are kept adjacent for the same reason.
func TestKnownFamiliesCoversEveryFamilyFamilyForCanReturn(t *testing.T) {
	known := map[string]bool{}
	for _, f := range KnownFamilies() {
		known[f] = true
	}
	for _, surface := range surfaceCapabilities {
		for _, shape := range AllShapes() {
			model := ModelFor(surface.Provider, shape)
			family := FamilyFor(surface.Provider, model)
			if family == "" {
				continue
			}
			if !known[family] {
				t.Fatalf("FamilyFor(%q, %q) returns %q, which KnownFamilies() omits; "+
					"excluding that family would be refused as unknown",
					surface.Provider, model, family)
			}
		}
	}
}

// The severe instance. For a reviewer, Decide sets excluded = req.AuthorFamily
// and separately refuses any candidate whose family == req.AuthorFamily. BOTH
// comparisons are against that string, so an author family the router does not
// recognise disables reviewer independence twice over, silently: the reviewer
// can be routed onto the author's actual family while the system believes it
// enforced disjointness.
//
// That does not produce a visible failure. It produces a verdict that looks
// valid and is inadmissible -- which is strictly worse than a refusal, and is
// the one thing the review contract says must never happen.
func TestReviewerDecisionRefusesAnUnrecognisedAuthorFamily(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil)
	_, err := r.Decide(LaunchRequest{
		Role:         RoleReviewer,
		Shape:        "qa",
		AuthorFamily: "grok", // the router calls this family "xai"
	})
	if err == nil {
		t.Fatal("an unrecognised author family was accepted; reviewer independence is now unenforceable")
	}
	if !strings.Contains(err.Error(), "unknown model family") {
		t.Fatalf("refusal does not name the fault: %v", err)
	}
}

// And the explicit exclusion on the same path.
func TestDecideRefusesAnUnrecognisedExcludedFamily(t *testing.T) {
	clearRouteEnv(t)
	r := testRouter(nil)
	_, err := r.Decide(LaunchRequest{
		Role:           RoleWorker,
		Shape:          "implementation",
		ExcludedFamily: "totally-bogus-family",
	})
	if err == nil {
		t.Fatal("an unrecognised excluded family was accepted and silently dropped")
	}
	if !strings.Contains(err.Error(), "unknown model family") {
		t.Fatalf("refusal does not name the fault: %v", err)
	}
}
