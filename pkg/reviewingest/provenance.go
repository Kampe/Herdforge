package reviewingest

import (
	"fmt"
	"strings"

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
func ReconcileBuilderFamilyForSHA(a *Artifact, receiptPath, sha string, reachable reaches) error {
	if a == nil || strings.TrimSpace(sha) == "" || reachable == nil {
		return nil
	}
	recorded, ok := launch.BuilderFamilyReachingSHA(receiptPath, sha, func(branch string) bool {
		return reachable(branch, sha)
	})
	if !ok {
		return nil
	}
	stated := strings.TrimSpace(a.BuilderFamily)
	if stated == "" {
		a.BuilderFamily = recorded
		return nil
	}
	if !strings.EqualFold(stated, recorded) {
		return fmt.Errorf("builder-family %q contradicts launch provenance %q recorded for a branch reaching %s; "+
			"one of them is wrong about who wrote this code and admitting either would launder the disagreement",
			stated, recorded, shortSHA(sha))
	}
	return nil
}
