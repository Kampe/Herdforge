package sync

import (
	"strings"
	"testing"
)

// TestFencedPathUsesTheSameTwoRoutes is the FAC-569 regression.
//
// CHA-2184's exact shape: a legacy card with NO acceptance block, a current
// admitted cross-family PASS, an exact candidate, and a same-candidate landed
// disposition. It still failed with "card has no herd-acceptance-v1 block".
//
// Cause: BoardDoneFenced validated acceptance BEFORE authorizing the override
// and never consulted the legacy route. So when FAC-566 correctly sent overrides
// down the fenced path, Route B stopped being reachable. Two implementations of
// one gate, and only one knew about the second route.
//
// This test exercises the shared authority the fenced path now calls, with the
// override already authorized -- the ordering that was wrong.
func TestFencedPathUsesTheSameTwoRoutes(t *testing.T) {
	legacyCardNoFence := "## Outcome\nlegacy card groomed before the fence existed\n"
	override := &OverrideRecord{Policy: LegacyRoutePolicy, Actor: "coordinator"}

	route, legacy, err := authorizeClosureEvidence(
		legacyCardNoFence, "", override,
		fakeLegacy{ev: LegacyReviewEvidence{
			CandidateSHA:   "bb3fa1ae8355c5a086fdee716742104e7d9215ca",
			MergeSHA:       "8b5d266e00000000000000000000000000000000",
			Artifact:       ".herd/review/inbox/bb3fa1ae-review-cha-2184-google.md",
			Reviewer:       "review-cha-2184-google",
			ReviewerFamily: "google",
			BuilderFamily:  "openai",
			Verdict:        "PASS",
		}},
		"CHA-2184")
	if err != nil {
		t.Fatalf("CHA-2184's shape must authorize via the legacy route: %v", err)
	}
	if route != RouteLegacyReview {
		t.Fatalf("route = %q, want the legacy review route", route)
	}
	if legacy == nil || legacy.CandidateSHA != "bb3fa1ae8355c5a086fdee716742104e7d9215ca" {
		t.Fatalf("legacy evidence must be recorded for attribution, got %+v", legacy)
	}
}

// The ordering itself is the defect: acceptance must not be judged before the
// closing authority is known, or an override can never be considered.
func TestAcceptanceIsNotJudgedBeforeAuthority(t *testing.T) {
	legacyCardNoFence := "## Outcome\nno fence\n"

	// With NO override, a card lacking a block must still fail on acceptance.
	if _, _, err := authorizeClosureEvidence(legacyCardNoFence, "", nil, nil, "CHA-1"); err == nil {
		t.Fatal("no override and no block must still refuse")
	}
	// With a legacy override AND evidence, it must succeed -- proving the
	// override is consulted rather than pre-empted.
	route, _, err := authorizeClosureEvidence(
		legacyCardNoFence, "", &OverrideRecord{Policy: LegacyRoutePolicy},
		fakeLegacy{ev: goodEvidence()}, "CHA-1")
	if err != nil || route != RouteLegacyReview {
		t.Fatalf("the override must be consulted, got route=%q err=%v", route, err)
	}
}

// The flag named in operator-facing text must be the flag that exists.
func TestErrorTextNamesTheRealFlag(t *testing.T) {
	// Guard against reintroducing the invented name in this package's guidance.
	for _, blob := range []string{landedNote, baselineNote} {
		if strings.Contains(blob, "--candidate-sha") {
			t.Fatalf("operator text must name --candidate, not the nonexistent --candidate-sha: %q", blob)
		}
	}
}
