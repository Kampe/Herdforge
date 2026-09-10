package launch

import (
	"fmt"
	"strings"
)

// CanonicalMemberRequired is the fail-closed membership refusal. One spelling
// is shared with pkg/reviewledger so the copies cannot diverge.
const CanonicalMemberRequired = "launch receipt is not a canonical accepted member"

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
		return Receipt{}, fmt.Errorf("%s", CanonicalMemberRequired)
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
	return Receipt{}, fmt.Errorf("%s", CanonicalMemberRequired)
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

// AcceptedReviewLaunchForCandidate returns the canonical accepted review-role
// launch bound to reviewer and the requested candidate. A name-only lookup is
// not authorization for another SHA, task, repository, or lane. Receipts that
// omit CandidateSHA are not candidate-pinned and cannot authenticate a SHA.
func AcceptedReviewLaunchForCandidate(members []Receipt, reviewer, sha, task, repo, lane string) (Receipt, error) {
	reviewer = strings.TrimSpace(reviewer)
	sha = strings.TrimSpace(sha)
	if reviewer == "" {
		return Receipt{}, fmt.Errorf("reviewer identity is required")
	}
	if sha == "" {
		return Receipt{}, fmt.Errorf("candidate SHA is required for review launch proof")
	}
	task = strings.TrimSpace(task)
	repo = strings.TrimSpace(repo)
	lane = strings.TrimSpace(lane)
	if task == "" || repo == "" || lane == "" {
		return Receipt{}, fmt.Errorf("review launch proof requires task, repository, and lane binding")
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
		if strings.TrimSpace(m.CandidateSHA) != sha {
			continue
		}
		if strings.TrimSpace(m.TaskRef) != task {
			continue
		}
		if strings.TrimSpace(m.Repository) != repo {
			continue
		}
		if strings.TrimSpace(m.Lane) != lane {
			continue
		}
		matches = append(matches, m)
	}
	if len(matches) == 0 {
		return Receipt{}, fmt.Errorf("no canonical accepted review launch for %q on candidate %s", reviewer, sha)
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

// AcceptedNativeLaunchRouteForAgent resolves the authentic accepted launch route (provider, model, and optional account)
// for an active live agent by matching against canonical accepted launch receipts.
// Keyed by exact lane/name, session ID, pane ID, or tab ID.
// Receipts that are not accepted, or whose session/pane/incarnation conflicts, are rejected.
func AcceptedNativeLaunchRouteForAgent(members []Receipt, name, sessionID, paneID, tabID string) (provider, model, account string, err error) {
	name = strings.TrimSpace(name)
	sessionID = strings.TrimSpace(sessionID)
	paneID = strings.TrimSpace(paneID)
	tabID = strings.TrimSpace(tabID)

	var matches []Receipt
	for _, m := range members {
		if !m.Accepted {
			continue
		}
		if strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.Model) == "" {
			continue
		}

		// Check session match if receipt has session
		if sessionID != "" && strings.TrimSpace(m.HerdrSession) != "" {
			if strings.TrimSpace(m.HerdrSession) != sessionID {
				continue
			}
		}

		// Check pane match if receipt has pane
		if paneID != "" && strings.TrimSpace(m.PaneID) != "" {
			if strings.TrimSpace(m.PaneID) != paneID {
				continue
			}
		}

		// Check tab match if receipt has tab
		if tabID != "" && strings.TrimSpace(m.TabID) != "" {
			if strings.TrimSpace(m.TabID) != tabID {
				continue
			}
		}

		// Check name / lane match if name is provided
		if name != "" {
			mName := strings.TrimSpace(m.Name)
			mLane := strings.TrimSpace(m.Lane)
			nameMatches := mName == name || mLane == name ||
				strings.TrimPrefix(name, "forge-") == mLane ||
				strings.TrimPrefix(name, "forge-") == mName ||
				strings.TrimPrefix(mName, "forge-") == name
			if !nameMatches {
				continue
			}
		}

		matches = append(matches, m)
	}

	if len(matches) == 0 {
		return "", "", "", fmt.Errorf("no authentic accepted launch receipt found for lane %q (session: %q, pane: %q)", name, sessionID, paneID)
	}

	// Latest accepted receipt wins
	latest := matches[len(matches)-1]
	return strings.TrimSpace(latest.Provider), strings.TrimSpace(latest.Model), strings.TrimSpace(latest.RedactedAuthority), nil
}
