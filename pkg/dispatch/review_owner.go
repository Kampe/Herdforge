package dispatch

import (
	"fmt"
	"strings"
)

// FAC-522: a lane that OWNS a queue must be authorized to act on it, and a
// read-only lane must never be the declared owner of one.
//
// The reported failure was a supervisor receiving the exact NEEDS_REVIEW
// handoff it exists to service, refusing it because its standing goal declares
// it a read-only monitor, and then resuming empty polling. The handoff was
// correctly delivered and correctly addressed, then dropped, while the lane
// still looked busy. Detecting the polling is not enough: kicking the lane just
// produces another refusal, because the lane is doing exactly what its goal
// says. The contradiction has to be caught where the owner is chosen.
//
// readOnlyOwnerRoles are roles whose contract forbids acting on a queue. A
// reviewer is adversarial and read-only by its own prompt: it reports a verdict
// and explicitly may not respawn lanes or own retries. Resolving a review-queue
// owner to one of these is a configuration error, not a routing preference.
var readOnlyOwnerRoles = map[string]string{
	"reviewer":           "reviewer lanes are adversarial and read-only; they report verdicts and do not own the queue, retries, or pane lifecycle",
	"assayer":            "assayer lanes perform read-only review of an exact SHA and cannot dispatch",
	"scout-planner":      "scout-planner lanes propose eligible work and do not act on the review queue",
	"recovery-sentinel":  "recovery-sentinel lanes detect stranded work and report it; they do not service queues",
	"verification-gate":  "verification-gate lanes record deterministic evidence for one SHA and do not route handoffs",
}

// reviewOwnerRoleOrder is the resolution order for the review-queue owner.
// Read-only roles are deliberately absent: the previous order included
// "reviewer", so on a board without an explicit supervisor lane the review
// packet was addressed to a lane structurally forbidden to act on it -- the
// exact contradiction FAC-522 describes, reachable from Herdforge's own config.
var reviewOwnerRoleOrder = []string{
	"review-supervisor",
	"review_harvest_supervisor",
	"harvest-supervisor",
	"harvest",
}

// UnauthorizedOwnerError is the loud configuration error raised when a handoff
// would be addressed to a lane that cannot act on it. It names the lane, the
// handoff, and the conflicting authority, so the contradiction is reported
// rather than swallowed into a silent refusal.
type UnauthorizedOwnerError struct {
	Lane     string
	Role     string
	Handoff  string
	Conflict string
}

func (e *UnauthorizedOwnerError) Error() string {
	return fmt.Sprintf(
		"configuration error: lane %q (role %q) is the declared owner of %s but is not authorized to act on it: %s",
		e.Lane, e.Role, e.Handoff, e.Conflict)
}

// authorizeQueueOwner reports whether a lane may own a queue. A lane whose role
// is read-only fails closed with an error naming the conflict.
func authorizeQueueOwner(laneName, role, handoff string) error {
	role = strings.ToLower(strings.TrimSpace(role))
	if conflict, readOnly := readOnlyOwnerRoles[role]; readOnly {
		return &UnauthorizedOwnerError{
			Lane:     strings.TrimSpace(laneName),
			Role:     role,
			Handoff:  strings.TrimSpace(handoff),
			Conflict: conflict,
		}
	}
	return nil
}
