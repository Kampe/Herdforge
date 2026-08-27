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
func ReconcileBuilderFamily(a *Artifact, receiptPath, branch string) error {
	if a == nil {
		return nil
	}
	recorded, ok := launch.BuilderFamilyForBranch(receiptPath, branch)
	if !ok {
		return nil
	}
	stated := strings.TrimSpace(a.BuilderFamily)
	if stated == "" {
		a.BuilderFamily = recorded
		return nil
	}
	if !strings.EqualFold(stated, recorded) {
		return fmt.Errorf("builder-family %q contradicts recorded launch provenance %q for branch %s; "+
			"one of them is wrong about who wrote this code and admitting either would launder the disagreement",
			stated, recorded, branch)
	}
	return nil
}
