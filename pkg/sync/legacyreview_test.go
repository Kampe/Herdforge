package sync

import (
	"strings"
	"testing"
)

const legacyBlockCard = "## Outcome\nland it\n\n```herd-acceptance-v1\n" +
	`{"commands":[{"command":"pnpm test","context":"packages/adapters"}]}` + "\n```\n"

type fakeLegacy struct {
	ev  LegacyReviewEvidence
	err error
}

func (f fakeLegacy) AdmittedPass(string) (LegacyReviewEvidence, error) { return f.ev, f.err }

func goodEvidence() LegacyReviewEvidence {
	return LegacyReviewEvidence{
		CandidateSHA:   "8ef12f0dbd6b172745c3b9abde4fc294fe7b1d2f",
		MergeSHA:       "da3a0f555d178a0053e91678adbabee5b942064e",
		Artifact:       ".herd/review/outbox/8ef12f0dbd6b-review-cha-2199-google.md",
		Reviewer:       "review-cha-2199-google",
		ReviewerFamily: "google",
		BuilderFamily:  "openai",
		Verdict:        "PASS",
	}
}

// TestRouteAPreferredWhenCardHasContract: a card WITH a block is held to it.
func TestRouteAPreferredWhenCardHasContract(t *testing.T) {
	route, legacy, err := authorizeClosureEvidence(
		legacyBlockCard, "$ cd packages/adapters && pnpm test\nok 42 passed\n", nil, fakeLegacy{ev: goodEvidence()}, "CHA-1")
	if err != nil {
		t.Fatal(err)
	}
	if route != RouteAcceptanceBlock || legacy != nil {
		t.Fatalf("a card with a contract must close by acceptance block, got route=%s legacy=%v", route, legacy)
	}
}

// A card WITH a block and bad evidence must fail on the block, never fall
// through to the legacy route. Otherwise Route B becomes a way around Route A.
func TestRouteBCannotBypassAnExistingBlock(t *testing.T) {
	override := &OverrideRecord{Policy: LegacyRoutePolicy}
	_, _, err := authorizeClosureEvidence(
		legacyBlockCard, "unrelated output", override, fakeLegacy{ev: goodEvidence()}, "CHA-1")
	if err == nil {
		t.Fatal("a card with a block must fail on that block")
	}
	if !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("failure must name the acceptance contract, got %v", err)
	}
}

// TestRouteBAuthorizesLegacyCard is the consumer's requested design: a card
// that never had a fence closes on admitted cross-family review evidence,
// without inventing a retrospective contract.
func TestRouteBAuthorizesLegacyCard(t *testing.T) {
	override := &OverrideRecord{Policy: LegacyRoutePolicy}
	route, legacy, err := authorizeClosureEvidence(
		"## Outcome\nlegacy card, no fence\n", "", override, fakeLegacy{ev: goodEvidence()}, "CHA-2199")
	if err != nil {
		t.Fatalf("admitted cross-family review must authorize a legacy close: %v", err)
	}
	if route != RouteLegacyReview {
		t.Fatalf("route must be recorded as legacy, got %q", route)
	}
	if legacy == nil || legacy.CandidateSHA != goodEvidence().CandidateSHA {
		t.Fatalf("legacy evidence must be returned for the record, got %+v", legacy)
	}
}

// Route B is legacy-only: any other policy stays on Route A.
func TestRouteBRequiresTheLegacyPolicy(t *testing.T) {
	for _, policy := range []string{"duplicate-card", "", "make-it-green"} {
		_, _, err := authorizeClosureEvidence(
			"no fence here", "", &OverrideRecord{Policy: policy}, fakeLegacy{ev: goodEvidence()}, "CHA-3")
		if err == nil {
			t.Fatalf("policy %q must not reach the legacy route", policy)
		}
	}
}

// Route B refuses weak evidence. Same-family review is self-certification with
// extra steps; a missing artifact is an assertion, not a record.
func TestRouteBRefusesWeakEvidence(t *testing.T) {
	override := &OverrideRecord{Policy: LegacyRoutePolicy}
	cases := map[string]func(LegacyReviewEvidence) LegacyReviewEvidence{
		"same family":     func(e LegacyReviewEvidence) LegacyReviewEvidence { e.BuilderFamily = e.ReviewerFamily; return e },
		"no artifact":     func(e LegacyReviewEvidence) LegacyReviewEvidence { e.Artifact = ""; return e },
		"not pass":        func(e LegacyReviewEvidence) LegacyReviewEvidence { e.Verdict = "FAIL"; return e },
		"no merge":        func(e LegacyReviewEvidence) LegacyReviewEvidence { e.MergeSHA = ""; return e },
		"no candidate":    func(e LegacyReviewEvidence) LegacyReviewEvidence { e.CandidateSHA = ""; return e },
		"unknown family":  func(e LegacyReviewEvidence) LegacyReviewEvidence { e.ReviewerFamily = "acme"; return e },
	}
	for name, mutate := range cases {
		_, _, err := authorizeClosureEvidence(
			"no fence", "", override, fakeLegacy{ev: mutate(goodEvidence())}, "CHA-4")
		if err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

// With no authority configured, and no acceptance block, closure refuses.
func TestNoRouteRefuses(t *testing.T) {
	override := &OverrideRecord{Policy: LegacyRoutePolicy}
	if _, _, err := authorizeClosureEvidence("no fence", "", override, nil, "CHA-5"); err == nil {
		t.Fatal("no acceptance block and no legacy authority must refuse")
	}
}
