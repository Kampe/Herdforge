// Package refname builds git ref names that are safe to publish.
//
// FAC-574: this exists because the same rule was implemented twice and I fixed
// the wrong copy. A generated harvest branch inherited "current-main" from its
// source, and a publication guard matching "main" anywhere refused it. FAC-571
// fixed pkg/resetsafe -- but the path a consumer actually invoked was
// pkg/harvestmerge.TempBranchName, which sanitized differently (underscores)
// and never stripped "main" at all. So the defect survived its own fix.
//
// That was the sixth instance in one session of one rule implemented twice and
// diverging (tip sets, ledger paths, closure gates, fence message, agy argv,
// and this). There is now exactly ONE definition, and both callers use it.
package refname

import (
	"regexp"
	"strings"
)

// unsafeChars are everything git-unfriendly for a single ref segment.
// StandingBranchPrefix is the branch namespace a standing lane's HOME lives in.
//
// FAC-711: this literal was written in cmd/herd/worktreereap.go and
// pkg/review/queue.go, and one of those uses is protective -- isResidentHome
// refuses to reap a standing lane's home, and a reaper that stops recognising
// this prefix classifies that home as a removable task worktree. FAC-672 caught
// exactly that in a dry run, with the coordinator's own home listed for removal.
// A convention rename landing on only one copy is how that becomes a live loss.
//
// NOT to be confused with kick.StandingNamePrefix ("standing-"), which is an
// AGENT NAME prefix. Similar spelling, different namespace: one names a git
// branch, the other names a live pane. Conflating them would make a reaper
// protect the wrong thing.
const StandingBranchPrefix = "standing/"

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// mainToken matches the trunk name in any case. A publication guard that greps
// for "main" cannot tell "current-main" from a push to main, so a generated name
// must not contain it at all.
var mainToken = regexp.MustCompile(`(?i)main`)

// PublishSafeSegment turns an arbitrary source branch name into one ref segment
// that no "main"-matching publication guard will refuse.
//
// Identity is NOT carried here: callers append the exact SHA, which is what
// actually identifies the harvest. That is why rewriting the descriptive portion
// is safe -- and why a consumer renaming only this segment, keeping the SHA
// suffix, was a correct workaround rather than a loss of provenance.
func PublishSafeSegment(branch string) string {
	seg := unsafeChars.ReplaceAllString(strings.TrimSpace(branch), "-")
	seg = mainToken.ReplaceAllString(seg, "trunk")
	// Collapse runs introduced by sanitizing, and trim separators so the
	// segment never starts or ends with one.
	for strings.Contains(seg, "--") {
		seg = strings.ReplaceAll(seg, "--", "-")
	}
	return strings.Trim(seg, "-._")
}
