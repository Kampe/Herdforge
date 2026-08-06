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
// Mirrors the reference `^\+(<{7}|>{7}|={7}$)`. Two earlier mistakes are fixed
// here: `={7}$` was dropped on the theory that flagging it would catch markdown
// underlines — but the reference already solved that by anchoring the separator
// to end-of-line, so a `=======` heading rule with trailing content is fine
// while a bare separator is caught. And the old exact-7 quantifier missed
// longer markers, which git emits under `.gitattributes conflict-marker-size=8`.
// Asymmetric on purpose: <<< >>> ||| never occur in prose so they match 7-or-
// more (covering conflict-marker-size=8), while the separator is EXACTLY seven
// '=' anchored to end-of-line, matching the reference. A markdown setext
// underline is sized to its heading and is rarely exactly seven, so this
// catches the real separator without flagging documentation.
var conflictMarkerRe = regexp.MustCompile(`^(<{7,}|>{7,}|\|{7,})( |$)|^={7}$`)

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
		body := strings.TrimPrefix(line, "+")
		if conflictMarkerRe.MatchString(body) {
			found = append(found, strings.TrimSpace(body))
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
