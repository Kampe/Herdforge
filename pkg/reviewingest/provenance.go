package reviewingest

import (
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/launch"
)

// ReconcileBuilderFamily joins an artifact to recorded launch provenance.
//
// FAC-620 P1: builder-family arrived as a HEADER a reviewer typed. That is an
// assertion, not provenance -- and after FAC-615's provider fallthrough it is
// an assertion about a route the reviewer never observed. A lane configured for
// codex that actually ran claude gets attested as whatever the reviewer
// believed, and cross-family independence is then computed against a family
// that may never have written the code.
//
// Two outcomes matter and they are NOT the same:
//
//   - The artifact states no family. Provenance FILLS it. That is strictly
//     better than the reviewer guessing, and better than refusing a review for
//     a field the reviewer had no way to know.
//
//   - The artifact states a family that CONTRADICTS provenance. That is
//     refused. One of the two is wrong about who wrote the code, and admitting
//     either reading would launder the disagreement into the ledger. Compare
//     the ReadHead rule immediately above in Validate: absent is tolerated, a
//     STATED mismatch never is.
//
// No recorded provenance leaves the artifact untouched. Absence of a receipt is
// not evidence of a different builder -- it is the pre-FAC-620 state, and
// refusing on it would reject every historical artifact.
// reaches reports whether a receipt's branch actually contains the reviewed
// commit. Injected so the join is testable without a live repository.
type reaches func(branch, sha string) bool

// ReconcileBuilderFamilyForSHA joins an artifact to provenance that is PROVEN
// to reach the reviewed commit.
//
// FAC-620: an earlier version matched branch TEXT only. A branch name is not
// evidence: branches are reused, relaunched and rebased, so a receipt naming
// "wt/defi-crusader" says nothing about whether that branch still contains the
// SHA under review. Attributing a family on name alone produces confident WRONG
// provenance, which is the exact harm this card exists to prevent -- worse than
// none, because independence would be computed against it and it would look
// authoritative.
//
// So a receipt qualifies only when its branch git-reaches the exact SHA. A
// receipt whose branch has moved on is ignored, not guessed at.
//
// And reachability is not enough on its own: a standing lane relaunched on the
// same branch AFTER the commit was written also reaches it, and would steal the
// attribution from the launch that actually produced it. commitTime closes
// that: only receipts predating the commit qualify, latest one wins. An
// unknown commit time yields no provenance rather than a guess.
//
// The fourth review found the hole that pairs with all of the above: every
// rejection path here lands on "no qualifying receipt", and that outcome LEFT
// THE ARTIFACT ALONE -- so a reviewer-asserted family sailed through unchecked.
// Tightening the join therefore widened the hole it was closing. So when no
// receipt qualifies, a BARE asserted family must be corroborated by an
// exact-SHA ledger record or refused.
//
// corroborate reports the family the ledger can prove for this exact SHA. It
// returns an error for an unreadable ledger, which is not proof of anything and
// must not be reported as absence.
//
// candid reports that the artifact declined to claim a family at all -- an
// honest "unknown", or a hedged claim the reviewer flagged as inferred. Those
// are the pre-FAC-620 state and stay admissible; the ingest gate routes them to
// provenance-unrecorded. Only an UNHEDGED assertion with nothing behind it is
// refused, because that is the one shape that looks like proof and is not.
// The returned bool reports whether the family is now PROVENANCE-BACKED by a
// receipt reaching this exact SHA. FAC-625: RequireCorroboratedFamily used to
// run unconditionally after this join, re-checking against the ledger a family
// that was already proven from a launch receipt. The ledger record that would
// corroborate a SHA is created BY INGESTING A VERDICT FOR THAT SHA, so every
// first review of every candidate was downgraded to unrecorded regardless of
// how solid its receipt-based proof was. Callers must skip the corroboration
// gate when this returns true.
func ReconcileBuilderFamilyForSHA(a *Artifact, receiptPath, sha string, commitTime time.Time, reachable reaches) (bool, error) {
	if a == nil || strings.TrimSpace(sha) == "" || reachable == nil {
		return false, nil
	}
	recorded, ok := launch.BuilderFamilyReachingSHA(receiptPath, sha, commitTime, func(branch string) bool {
		return reachable(branch, sha)
	})
	if !ok {
		return false, nil
	}
	stated := strings.TrimSpace(a.BuilderFamily)
	if stated == "" {
		a.BuilderFamily = recorded
		return true, nil
	}
	if !strings.EqualFold(stated, recorded) {
		return false, fmt.Errorf("builder-family %q contradicts launch provenance %q recorded for a branch reaching %s; "+
			"one of them is wrong about who wrote this code and admitting either would launder the disagreement",
			stated, recorded, shortSHA(sha))
	}
	return true, nil
}

// RequireCorroboratedFamily stops an unproven builder-family being TRUSTED.
//
// It downgrades rather than refuses, and that distinction was measured, not
// assumed. Refusing was the obvious reading, and it is wrong: the ledger record
// that could corroborate a SHA is created BY ingesting a verdict for that SHA,
// so no first review of any commit can ever be corroborated. A dry-run of the
// refusing version against the live inbox refused the only artifact in it -- a
// legitimate independent FAIL. A gate whose "absence" branch is the normal case
// does not harden a pipeline, it halts one.
//
// So an uncorroborated assertion is rewritten to the unrecorded sentinel. The
// review survives, and the field can no longer launder a guess into an
// independence computation -- which was the whole harm. It lands in exactly the
// bucket FAC-627/FAC-628 built for honestly-unprovable provenance.
//
// Refusal is kept for the two cases where something is actually WRONG rather
// than merely unproven: the ledger contradicts the claim, or the ledger cannot
// be read (unreadable is not proof of absence).
//
// unrecorded is the sentinel the caller's gate recognises. Passing "" disables
// the downgrade and leaves the artifact untouched.
func RequireCorroboratedFamily(a *Artifact, sha, unrecorded string, corroborate func(string) (string, error), candid func(string) bool) error {
	if a == nil || strings.TrimSpace(sha) == "" {
		return nil
	}
	return requireCorroboration(a, sha, unrecorded, corroborate, candid)
}

// requireCorroboration handles the no-qualifying-receipt case.
//
// Untouched: no claim at all, or a candid/hedged one. Untouched: a claim the
// ledger independently proves for this exact SHA. Downgraded: a bare assertion
// with neither -- it is indistinguishable from a guess. Refused: a claim the
// ledger contradicts, or one that could not be checked at all.
func requireCorroboration(a *Artifact, sha, unrecorded string, corroborate func(string) (string, error), candid func(string) bool) error {
	stated := strings.TrimSpace(a.BuilderFamily)
	if stated == "" {
		return nil
	}
	if candid != nil && candid(stated) {
		return nil
	}
	if corroborate == nil {
		// No way to check is not the same as checked-and-absent. Refusing here
		// would punish a caller that simply has no ledger wired; admitting is
		// the pre-FAC-620 behaviour and the caller keeps its own gate.
		return nil
	}
	proven, err := corroborate(sha)
	if err != nil {
		return fmt.Errorf("builder-family %q could not be checked for %s: %w; an unreadable ledger is not proof of provenance",
			stated, shortSHA(sha), err)
	}
	if proven == "" {
		// Not refused: downgraded. The review is real work and survives; the
		// unproven family simply stops being usable as provenance.
		if unrecorded != "" {
			a.BuilderFamily = unrecorded
		}
		return nil
	}
	if !strings.EqualFold(stated, proven) {
		return fmt.Errorf("builder-family %q contradicts ledger-recorded %q for %s", stated, proven, shortSHA(sha))
	}
	return nil
}
