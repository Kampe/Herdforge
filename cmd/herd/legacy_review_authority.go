package main

import (
	"fmt"
	"strings"

	"github.com/Kampe/Herdforge/pkg/review"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// ledgerLegacyReview resolves admitted cross-family review evidence from the
// append-only review ledger.
//
// FAC-564: this is Route B for cards groomed before herd-acceptance-v1 existed.
// Every fact it returns is READ FROM THE LEDGER -- verdict, reviewer, families,
// artifact, merge SHA -- so the closer cannot assert its own authorization. That
// is the whole point: the evidence was recorded before the outcome was known,
// which is exactly what a retrospective acceptance fence is not.
type ledgerLegacyReview struct {
	ledgerPath string
	// refMatches decides whether a ledger row belongs to a ref. Branch naming
	// is repository convention, so it is injectable rather than assumed.
	refMatches func(row review.LedgerRow, ref string) bool
}

func newLedgerLegacyReview(ledgerPath string) *ledgerLegacyReview {
	return &ledgerLegacyReview{ledgerPath: ledgerPath, refMatches: rowNamesRef}
}

// rowNamesRef is true when a ledger row's branch or artifact names the ref.
// Matching is case-insensitive because branch conventions vary in case.
func rowNamesRef(row review.LedgerRow, ref string) bool {
	needle := strings.ToLower(strings.TrimSpace(ref))
	if needle == "" {
		return false
	}
	for _, field := range []string{row.Branch, row.Artifact, row.Lane} {
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

// AdmittedPass returns the admitted cross-family PASS bound to ref.
//
// It refuses ambiguity rather than picking one: two distinct admitted candidates
// for the same ref means the operator must name which one shipped, and guessing
// would authorize a closure against evidence for a different object.
func (l *ledgerLegacyReview) AdmittedPass(ref string) (hsync.LegacyReviewEvidence, error) {
	snap, err := review.OpenLedger(l.ledgerPath).Snapshot()
	if err != nil {
		return hsync.LegacyReviewEvidence{}, fmt.Errorf("read review ledger: %w", err)
	}

	revoked := map[string]bool{}
	for _, row := range snap.Rows {
		if row.Event == string(reviewledger.EventRevoked) || row.Event == string(reviewledger.EventSupersession) {
			revoked[row.SHA] = true
		}
	}

	found := map[string]hsync.LegacyReviewEvidence{}
	for _, row := range snap.Rows {
		if row.Verdict != string(reviewledger.VerdictPASS) {
			continue
		}
		if row.SHA == "" || revoked[row.SHA] {
			// A revoked or superseded PASS is not current evidence.
			continue
		}
		if !l.refMatches(row, ref) {
			continue
		}
		ev := hsync.LegacyReviewEvidence{
			CandidateSHA:   row.SHA,
			MergeSHA:       row.MergeSHA,
			Artifact:       row.Artifact,
			Reviewer:       row.Reviewer,
			ReviewerFamily: row.ReviewerFamily,
			BuilderFamily:  row.BuilderFamily,
			Verdict:        row.Verdict,
		}
		// Later rows enrich earlier ones (merge SHA is often recorded after the
		// verdict), so merge rather than overwrite with blanks.
		if prev, ok := found[row.SHA]; ok {
			if ev.MergeSHA == "" {
				ev.MergeSHA = prev.MergeSHA
			}
			if ev.BuilderFamily == "" {
				ev.BuilderFamily = prev.BuilderFamily
			}
			if ev.Artifact == "" {
				ev.Artifact = prev.Artifact
			}
		}
		found[row.SHA] = ev
	}

	switch len(found) {
	case 0:
		return hsync.LegacyReviewEvidence{}, fmt.Errorf("no current admitted PASS names %s", ref)
	case 1:
		for _, ev := range found {
			return l.withLandedDisposition(ref, ev), nil
		}
	}
	shas := make([]string, 0, len(found))
	for sha := range found {
		shas = append(shas, sha)
	}
	return hsync.LegacyReviewEvidence{}, fmt.Errorf(
		"%d distinct admitted candidates name %s (%s); name the one that shipped rather than letting the tool guess",
		len(found), ref, strings.Join(shas, ", "))
}

// withLandedDisposition supplies the merge disposition for a verdict that was
// admitted BEFORE the merge and therefore carries no merge_sha.
//
// FAC-565: that is the normal shape for the legacy backlog -- the candidate was
// reviewed, then merged by the repository-authorized path -- and it left Route B
// refusing work that provably landed. The disposition is a post-merge git
// observation recorded by `harvest-merge --verify-landed --ref`, and it is only
// accepted when it BINDS THE SAME CANDIDATE the verdict is about. A disposition
// for another object must never satisfy this check.
func (l *ledgerLegacyReview) withLandedDisposition(ref string, ev hsync.LegacyReviewEvidence) hsync.LegacyReviewEvidence {
	if strings.TrimSpace(ev.MergeSHA) != "" {
		return ev
	}
	disposition, err := hsync.ReadLandedDisposition(".", ref)
	if err != nil || disposition == nil {
		// Absent or unreadable: leave MergeSHA empty so validation refuses with
		// "no verified merge disposition" rather than closing on nothing.
		return ev
	}
	if !disposition.BindsCandidate(ev.CandidateSHA) {
		return ev
	}
	ev.MergeSHA = disposition.MergeSHA
	return ev
}
