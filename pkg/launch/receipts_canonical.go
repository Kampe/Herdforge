package launch

import (
	"fmt"
	"strings"
)

// AcceptedCanonicalMember returns the canonical accepted log copy that the
// locator identifies. Membership is exact sameReceipt identity, including
// StartToken. StartToken is the post-start process proof recorded at
// Accept; a reservation has none, and resume (HasStarted) requires the
// accepted token. A copied DecisionDigest is mismatch diagnosis only, never
// authentication of a changed launch. Host, Session, BuilderFamily, and
// Accepted come from the log member. A hand-written JSON file that is not
// in the log is not authentication.
func AcceptedCanonicalMember(members []Receipt, locator Receipt) (Receipt, error) {
	var exact []Receipt
	var digestBound []Receipt
	locDigest := strings.TrimSpace(locator.DecisionDigest)
	for _, m := range members {
		if !m.Accepted {
			continue
		}
		if sameReceipt(m, locator) {
			exact = append(exact, m)
			continue
		}
		if locDigest != "" && strings.TrimSpace(m.DecisionDigest) == locDigest &&
			m.TaskRef == locator.TaskRef && m.Name == locator.Name && m.Role == locator.Role {
			digestBound = append(digestBound, m)
		}
	}
	if len(exact) > 0 {
		if receiptsConflict(exact) {
			return Receipt{}, fmt.Errorf("conflicting canonical launch receipts")
		}
		return exact[len(exact)-1], nil
	}
	if len(digestBound) == 0 {
		return Receipt{}, fmt.Errorf("launch receipt is not a canonical accepted member")
	}
	if receiptsConflict(digestBound) {
		return Receipt{}, fmt.Errorf("conflicting canonical launch receipts")
	}
	canonical := digestBound[len(digestBound)-1]
	if hostFieldMismatch(locator.PaneID, canonical.PaneID) || hostFieldMismatch(locator.ProcessIdentity, canonical.ProcessIdentity) {
		return Receipt{}, fmt.Errorf("mismatched host: locator is not the canonical review host")
	}
	if hostFieldMismatch(locator.HerdrSession, canonical.HerdrSession) {
		return Receipt{}, fmt.Errorf("mismatched session: locator is not the canonical review session")
	}
	if strings.TrimSpace(locator.StartToken) != strings.TrimSpace(canonical.StartToken) {
		return Receipt{}, fmt.Errorf("tampered start token is not the canonical accepted launch")
	}
	return Receipt{}, fmt.Errorf("launch receipt is not a canonical accepted member")
}

// AcceptedReviewLaunchFor returns the canonical accepted review-role launch
// bound to reviewer. A worker/builder ProcessIdentity, PaneID, or CWD is not
// reviewer host proof. Conflicting review hosts for the same name are refused
// rather than guessed.
func AcceptedReviewLaunchFor(members []Receipt, reviewer string) (Receipt, error) {
	reviewer = strings.TrimSpace(reviewer)
	if reviewer == "" {
		return Receipt{}, fmt.Errorf("reviewer identity is required")
	}
	var matches []Receipt
	for _, m := range members {
		if !m.Accepted {
			continue
		}
		if strings.TrimSpace(m.Role) != ReviewerRole {
			continue
		}
		if strings.TrimSpace(m.Name) != reviewer {
			continue
		}
		matches = append(matches, m)
	}
	if len(matches) == 0 {
		return Receipt{}, fmt.Errorf("no canonical accepted review launch for %q", reviewer)
	}
	if reviewHostsConflict(matches) {
		return Receipt{}, fmt.Errorf("conflicting canonical review launches for %q", reviewer)
	}
	return matches[len(matches)-1], nil
}

func hostFieldMismatch(locator, canonical string) bool {
	a, b := strings.TrimSpace(locator), strings.TrimSpace(canonical)
	if a == "" || b == "" {
		return a != b
	}
	return a != b
}

func receiptsConflict(members []Receipt) bool {
	if len(members) < 2 {
		return false
	}
	first := members[0]
	for _, m := range members[1:] {
		if strings.TrimSpace(m.BuilderFamily) != strings.TrimSpace(first.BuilderFamily) ||
			strings.TrimSpace(m.Branch) != strings.TrimSpace(first.Branch) ||
			strings.TrimSpace(m.CandidateSHA) != strings.TrimSpace(first.CandidateSHA) ||
			m.Accepted != first.Accepted {
			return true
		}
	}
	return false
}

func reviewHostsConflict(members []Receipt) bool {
	if len(members) < 2 {
		return false
	}
	host := reviewHost(members[0])
	session := reviewSession(members[0])
	for _, m := range members[1:] {
		if reviewHost(m) != host || reviewSession(m) != session {
			return true
		}
	}
	return false
}

func reviewHost(r Receipt) string {
	for _, v := range []string{r.ProcessIdentity, r.HerdrSession, r.PaneID, r.CWD, r.Worktree} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func reviewSession(r Receipt) string {
	for _, v := range []string{r.ProcessIdentity, r.HerdrSession, r.PaneID} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
