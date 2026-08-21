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
	"github.com/Kampe/Herdforge/pkg/refname"
	"fmt"
	"regexp"
	"strings"
)

// Verdict gates the merge.
type Verdict string

// CandidatePin is the immutable review candidate selected for harvest. The
// branch is only its provenance; it is never permission to substitute the
// branch tip for SHA.
type CandidatePin struct {
	SHA    string
	Branch string
}

// CandidateRange is the exact reviewed history slice to harvest. Base and SHA
// are kept as user-supplied git revisions so the caller can resolve them in
// the repository where the harvest is running.
type CandidateRange struct {
	Base string
	SHA  string
}

// ParseCandidateRange parses the two-dot range accepted by harvest-merge.
// Three-dot ranges and empty revisions are rejected because they do not name
// the exact linear history slice that the review covered.
func ParseCandidateRange(value string) (CandidateRange, error) {
	value = strings.TrimSpace(value)
	if strings.Count(value, "..") != 1 || strings.Contains(value, "...") {
		return CandidateRange{}, fmt.Errorf("harvest-merge: --candidate-range must be <base>..<sha>")
	}
	parts := strings.SplitN(value, "..", 2)
	if strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" ||
		strings.ContainsAny(parts[0], " \t\r\n") || strings.ContainsAny(parts[1], " \t\r\n") {
		return CandidateRange{}, fmt.Errorf("harvest-merge: --candidate-range must contain non-empty revisions")
	}
	return CandidateRange{Base: parts[0], SHA: parts[1]}, nil
}

// Valid reports whether the pin names both an exact candidate and its source
// branch.
func (p CandidatePin) Valid() bool {
	return strings.TrimSpace(p.SHA) != "" && strings.TrimSpace(p.Branch) != ""
}

const (
	PASS    Verdict = "PASS"
	FAIL    Verdict = "FAIL"
	BLOCKED Verdict = "BLOCKED"
	RETIRED Verdict = "RETIRED"
)

// MergeAllowed reports whether a verdict permits merging. Anything that is not
// an explicit PASS refuses: an unknown or absent verdict is not consent.
func MergeAllowed(v Verdict) bool { return v == PASS }

// Terminal reports whether the ledger state settles a branch. RETIRED is a
// terminal audit/drain state, but deliberately does not grant merge authority.
func Terminal(v Verdict) bool {
	return v == PASS || v == FAIL || v == BLOCKED || v == RETIRED
}

// conflictMarkerRe matches git conflict markers at line start.
//
// All four markers accept 7-or-more so `.gitattributes conflict-marker-size=8`
// is covered. The earlier exact-7 separator missed `========` — the very
// configuration the angle markers had just been widened for — so the
// half-resolved shape it was restored to catch was still getting through.
//
// DECIDED TRADE-OFF, stated at full cost: `={7,}$` matches essentially ANY
// setext-underlined markdown heading, not an edge case — round two only caught
// exactly-seven, this catches seven-or-more. That is accepted deliberately. This is a merge gate: a false
// positive is recoverable in seconds and names the offending line, while a
// false negative puts a structurally broken diff into a PR body that nobody
// re-reads. Prefer the recoverable failure. Use an ATX `#` heading or a `---`
// underline in docs if you hit it.
//
// (?m) is set so the pattern stays correct if a future caller passes a
// multi-line string; today ConflictMarkers feeds it one line at a time.
var conflictMarkerRe = regexp.MustCompile(`(?m)^(<{7,}|>{7,}|\|{7,})( |$)|^={7,}$`)

// NOTE: pkg/conflict has a SECOND, independent marker detector used by the
// resolver (prefix-based, unanchored, no width rules). The two deliberately
// disagree — this one gates a merge and errs toward refusing, that one parses a
// conflict it already knows is present. Change one and check the other.
//
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
		// TrimRIGHT only. CRLF is the actual problem — the added line is
		// "=======\r" and `$` does not match before the \r — but TrimSpace also
		// stripped LEADING whitespace, which the anchor was relying on. That
		// widened the gate to indented markers, so a runbook showing an operator
		// what a conflict looks like became unharvestable.
		body := strings.TrimRight(strings.TrimPrefix(line, "+"), "\r\n \t")
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
	// Diffstat is `git diff --shortstat origin/main...<branch>` for the lane.
	// Empty means the branch changes no bytes. Required: see Validate.
	Diffstat string
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
	// A commit COUNT is not a content check. PR #151 merged 0 additions, 0
	// deletions, 0 files: the branch carried its anchor commit, so len(Commits)
	// was 1 and `git cherry` marked it '+' because no patch-equivalent existed
	// upstream. The adversarial reviewer returned PASS -- an empty diff has
	// nothing wrong with it -- and the card was nearly closed as done.
	//
	// A merge that changes no bytes is not a completed ticket. An ABSENT
	// diffstat is refused on the same principle as an absent verdict: the caller
	// presents content evidence, it is not inferred from silence.
	if strings.TrimSpace(p.Diffstat) == "" {
		return fmt.Errorf("harvest-merge: %s changes no bytes against origin/main; "+
			"an empty diff is not a completed ticket (pass the `git diff --shortstat origin/main...%s` output)",
			p.Lane, p.Branch)
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
	// FAC-574: this is the generator a consumer actually invoked, and it
	// produced harvest/reconstruct_cha-2197-current-main-17ccbb16ecc0 -- which a
	// publication guard matching "main" refused. FAC-571 had fixed the OTHER
	// generator (pkg/resetsafe), so the defect survived its own fix. Both now
	// share one definition in pkg/refname.
	safe := refname.PublishSafeSegment(lane)
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return fmt.Sprintf("harvest/%s-%s", safe, sha)
}
