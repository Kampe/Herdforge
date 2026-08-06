// Package harvestmerge ports bin/herd-harvest-merge: coordinator harvest and
// merge of a lane's reviewed local commits.
//
// A lane never pushes its own branch — it commits locally and reports
// NEEDS_REVIEW with branch and HEAD. The coordinator then ran a five-step
// dance by hand, roughly thirty times in one chainseer session, and every trap
// it hit is a mechanical guarantee here rather than a thing to remember.
//
// The strategy that makes it safe: cherry-pick the lane's unique commits onto
// a FRESH worktree off origin/main, never onto a pin at the lane's older HEAD.
// That surfaces conflicts locally and keeps the merge-base at origin/main, so
// the downstream stale-base gate cannot trip on work that is actually current.
package harvestmerge

import (
	"fmt"
	"regexp"
	"strings"
)

// Verdict gates the merge.
type Verdict string

const (
	PASS    Verdict = "PASS"
	FAIL    Verdict = "FAIL"
	BLOCKED Verdict = "BLOCKED"
)

// MergeAllowed reports whether a verdict permits merging. Anything that is not
// an explicit PASS refuses: an unknown or absent verdict is not consent.
func MergeAllowed(v Verdict) bool { return v == PASS }

// conflictMarkerRe matches git conflict markers at line start.
//
// All four markers accept 7-or-more so `.gitattributes conflict-marker-size=8`
// is covered. The earlier exact-7 separator missed `========` — the very
// configuration the angle markers had just been widened for — so the
// half-resolved shape it was restored to catch was still getting through.
//
// DECIDED TRADE-OFF: `={7,}$` also matches a markdown setext underline, so a
// heading of seven-or-more characters in an added doc line will abort the
// harvest. That is accepted deliberately. This is a merge gate: a false
// positive is recoverable in seconds and names the offending line, while a
// false negative puts a structurally broken diff into a PR body that nobody
// re-reads. Prefer the recoverable failure. Use an ATX `#` heading or a `---`
// underline in docs if you hit it.
//
// (?m) is set so the pattern stays correct if a future caller passes a
// multi-line string; today ConflictMarkers feeds it one line at a time.
var conflictMarkerRe = regexp.MustCompile(`(?m)^(<{7,}|>{7,}|\|{7,})( |$)|^={7,}$`)

// ConflictMarkers returns the conflict markers found in staged ADDED lines.
//
// This is a HARD stage gate. A cherry-pick that leaves markers behind produces
// a diff that compiles-looking but is structurally broken, and once it is in a
// PR body nobody re-reads it. Scanning only ADDED lines means an unrelated
// pre-existing marker in the file cannot block an honest harvest.
func ConflictMarkers(stagedDiff string) []string {
	var found []string
	for _, line := range strings.Split(stagedDiff, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		// Match the TRIMMED body: with CRLF the added line is "=======\r" and
		// `$` does not match before the \r, so the separator check was
		// defeated by line endings alone. Reporting already trimmed, so
		// matching on the same value keeps the two consistent.
		body := strings.TrimSpace(strings.TrimPrefix(line, "+"))
		if conflictMarkerRe.MatchString(body) {
			found = append(found, body)
		}
	}
	return found
}

// Plan is a validated harvest.
type Plan struct {
	Lane        string
	Branch      string
	SHA         string
	Title       string
	Body        string
	Verdict     Verdict
	Commits     []string
	WorktreeDir string
	TempBranch  string
}

// Validate refuses a harvest that must not proceed. Every branch here is a
// trap that cost a real merge.
func (p Plan) Validate() error {
	if strings.TrimSpace(p.Lane) == "" {
		return fmt.Errorf("harvest-merge: lane is required")
	}
	if strings.TrimSpace(p.Title) == "" {
		return fmt.Errorf("harvest-merge: --title is required; an unlabelled PR is unreviewable")
	}
	// Absent is not consent. The first version only rejected a NON-EMPTY
	// non-PASS verdict, so omitting --verdict entirely sailed through the gate
	// and a full harvest ran to "gates passed" with no review at all.
	if !MergeAllowed(p.Verdict) {
		if strings.TrimSpace(string(p.Verdict)) == "" {
			return fmt.Errorf("harvest-merge: --verdict is required; an absent verdict is not consent to merge")
		}
		return fmt.Errorf("harvest-merge: verdict %s refuses the merge", p.Verdict)
	}
	if len(p.Commits) == 0 {
		return fmt.Errorf("harvest-merge: %s has no unique commits to harvest", p.Lane)
	}
	return nil
}

// UniqueCommits filters `git cherry origin/main <branch>` output down to the
// commits not already upstream.
//
// git cherry is PATCH-based, so a commit whose content already landed under a
// different SHA is marked '-' and skipped. Re-picking it would either conflict
// or silently duplicate the change.
func UniqueCommits(cherryOutput string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(cherryOutput), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "+" {
			continue
		}
		out = append(out, fields[1])
	}
	return out
}

// TempBranchName is the throwaway branch a harvest builds on. Deterministic
// per lane and SHA so a retry reuses it instead of littering the ref namespace.
func TempBranchName(lane, sha string) string {
	safe := regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(lane, "_")
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return fmt.Sprintf("harvest/%s-%s", safe, sha)
}
