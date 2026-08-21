package sync

import (
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-564: closing a legacy card previously required inventing a
// herd-acceptance-v1 block AFTER implementation, review and merge.
//
// That was wrong, and the consumer's argument is the reason: it makes the closer
// author the acceptance contract having already seen the result, which is
// weaker evidence dressed as stricter process. A fence written to match a known
// outcome cannot falsify anything. The gate looked stricter while degrading what
// it proved.
//
// So there are two authorization routes, and which one was used is recorded:
//
//	Route A (preferred, prospective): a PRE-EXISTING herd-acceptance-v1 block
//	plus literal command output. This is the only route for newly groomed cards.
//
//	Route B (legacy only): an exact admitted cross-family review artifact bound
//	to the candidate, plus a verified merge disposition. Cards groomed before
//	the fence existed already carry this evidence, and it was produced BEFORE
//	the outcome was known -- which is precisely what a post-hoc fence is not.
//
// If neither route holds, closure is refused. Route B is deliberately not a
// weaker Route A: it demands independently recorded evidence that a
// self-authored fence cannot supply.

// LegacyReviewEvidence is an admitted cross-family PASS bound to one candidate.
// Every field is read from the review ledger, never asserted by the closer.
type LegacyReviewEvidence struct {
	CandidateSHA   string
	MergeSHA       string
	Artifact       string
	Reviewer       string
	ReviewerFamily string
	BuilderFamily  string
	Verdict        string
}

// LegacyReviewAuthority resolves admitted review evidence for a ref. cmd/herd
// implements it over the real review ledger; tests inject a fake.
type LegacyReviewAuthority interface {
	AdmittedPass(ref string) (LegacyReviewEvidence, error)
}

// ErrLegacyReview is returned when legacy review evidence cannot authorize a
// closure.
var ErrLegacyReview = fmt.Errorf("legacy review evidence is insufficient")

// LegacyRoutePolicy is the only override policy Route B may authorize. The
// route exists for work that provably landed outside the fleet integration
// path; it must not become a general escape from acceptance.
const LegacyRoutePolicy = "operator-external-merge"

// Closure route names, recorded so an audit can tell which authority applied.
const (
	RouteAcceptanceBlock = "acceptance-block"
	RouteLegacyReview    = "legacy-admitted-review"
)

// Validate checks that the evidence is a genuine cross-family admitted PASS
// with a verified merge disposition.
func (e LegacyReviewEvidence) Validate() error {
	if strings.TrimSpace(e.CandidateSHA) == "" {
		return fmt.Errorf("%w: no candidate SHA", ErrLegacyReview)
	}
	if strings.TrimSpace(e.Artifact) == "" {
		// Without the artifact identity this is an assertion, not a record.
		return fmt.Errorf("%w: no admitted review artifact for %s", ErrLegacyReview, e.CandidateSHA)
	}
	if e.Verdict != string(reviewledger.VerdictPASS) {
		return fmt.Errorf("%w: verdict for %s is %q, not PASS", ErrLegacyReview, e.CandidateSHA, e.Verdict)
	}
	reviewer := strings.ToLower(strings.TrimSpace(e.ReviewerFamily))
	builder := strings.ToLower(strings.TrimSpace(e.BuilderFamily))
	if !reviewledger.FamilyAllowlist[reviewer] {
		return fmt.Errorf("%w: reviewer family %q is not an allowed family", ErrLegacyReview, e.ReviewerFamily)
	}
	if !reviewledger.FamilyAllowlist[builder] {
		return fmt.Errorf("%w: builder family %q is not an allowed family", ErrLegacyReview, e.BuilderFamily)
	}
	if reviewer == builder {
		// Same-family review is self-certification with extra steps.
		return fmt.Errorf("%w: reviewer and builder are both %q; cross-family review is required", ErrLegacyReview, reviewer)
	}
	if strings.TrimSpace(e.MergeSHA) == "" {
		return fmt.Errorf("%w: no verified merge disposition for %s", ErrLegacyReview, e.CandidateSHA)
	}
	return nil
}

// authorizeClosureEvidence resolves which route authorizes this closure.
//
// Route A is always tried first, so a card that HAS a contract is held to it.
// Route B is reachable only for a legacy-policy override, and only when the
// card genuinely has no acceptance block -- never as a way around a block the
// card does carry.
func authorizeClosureEvidence(
	description, evidence string,
	override *OverrideRecord,
	legacy LegacyReviewAuthority,
	ref string,
) (route string, legacyEvidence *LegacyReviewEvidence, err error) {
	acceptErr := ValidateAcceptanceEvidence(description, evidence)
	if acceptErr == nil {
		return RouteAcceptanceBlock, nil, nil
	}
	// Only a missing contract may fall through to Route B. A card WITH a block
	// whose evidence is wrong or absent must fail on that block.
	if _, parseErr := ParseAcceptanceBlock(description); parseErr == nil {
		return "", nil, acceptErr
	}
	if override == nil || override.Policy != LegacyRoutePolicy {
		return "", nil, acceptErr
	}
	if legacy == nil {
		return "", nil, fmt.Errorf("%w; and no legacy review authority is configured to authorize %s", acceptErr, ref)
	}
	found, lookupErr := legacy.AdmittedPass(ref)
	if lookupErr != nil {
		return "", nil, fmt.Errorf("%w; and no admitted review evidence for %s: %v", acceptErr, ref, lookupErr)
	}
	if validateErr := found.Validate(); validateErr != nil {
		return "", nil, fmt.Errorf("%w; and %v", acceptErr, validateErr)
	}
	return RouteLegacyReview, &found, nil
}
