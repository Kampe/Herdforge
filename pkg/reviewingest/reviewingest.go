// Package reviewingest ports bin/herd-review-ingest: parse a reviewer's
// verdict artifact and validate it before it may touch the ledger.
//
// Every rule here exists because a bad verdict is indistinguishable from a
// good one once it lands. The ledger cannot tell that a PASS was written by
// the coordinator about its own work, or produced by reading a live worktree
// instead of the reviewed commit — so those have to be refused at the door.
package reviewingest

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

// MinBodyChars is the evidence floor. A PASS with no reasoning is not a
// review; chainseer accepted one and it merged unexamined work.
const MinBodyChars = 200

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// unknownHeaderRe matches things that LOOK like front-matter keys. An
// unrecognised key is surfaced rather than ignored: a misspelled
// `reviewed-head` silently disables the wandering-reviewer gate, which is
// exactly how that gate came to be dead code in the first place.
var unknownHeaderRe = regexp.MustCompile(`^[a-z][a-z0-9._-]*$`)

// Artifact is a parsed reviewer verdict.
type Artifact struct {
	SHA    string
	Branch string
	// TaskRef is the board card this verdict belongs to. Without it a ledger
	// row carries only sha+verdict, so no verdict can be tied back to a card
	// and a corrupted board cannot be rebuilt from review history (FAC-578).
	TaskRef        string
	Reviewer       string
	Authority      string
	ReviewerFamily string
	BuilderFamily  string
	Verdict        string
	// ReadHead is the HEAD the reviewer states it actually read. Provenance,
	// not proof — but a truthful reviewer working from a pinned disposable
	// worktree reports the pin without effort, and a wandering one has to
	// state a mismatch or lie outright.
	ReadHead string
	RetryOf  string
	Body     string
	// UnknownHeaders are front-matter keys we did not recognise. Validate
	// REFUSES on these — a write-only field would surface nothing, which is how
	// a misspelled gate key stayed silent in the first place.
	UnknownHeaders []string
	// MalformedHeaderRegion records that a non-header line appeared before the
	// `---` separator.
	MalformedHeaderRegion bool
	// ConflictingHeaders are keys that appeared more than once with DIFFERENT
	// values. Ambiguous provenance is refused, never resolved by position.
	ConflictingHeaders []string
}

// Parse reads a front-matter artifact: `key: value` lines, then `---`, then a
// free-form body.
func Parse(text string) Artifact {
	var a Artifact
	// A BOM makes the first key parse as "\ufeffsha", which ends the header
	// region and reports a missing sha instead of the real cause.
	text = strings.TrimPrefix(text, "\ufeff")
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inBody := false
	seen := map[string]string{}
	var body strings.Builder

	for sc.Scan() {
		line := sc.Text()
		if !inBody {
			if strings.TrimSpace(line) == "---" {
				inBody = true
				continue
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			key, value, ok := strings.Cut(line, ":")
			// The header region is the LEADING block only. Any prose line
			// before the real headers used to claim a slot permanently under
			// first-wins, letting an attacker shadow `reviewer`,
			// `reviewer-family` or `reviewed-head` with a sentence and have
			// the honest header below it discarded. A non-header line ends the
			// region; everything after it is body.
			if !ok || !unknownHeaderRe.MatchString(strings.ToLower(strings.TrimSpace(key))) {
				a.MalformedHeaderRegion = true
				inBody = true
				body.WriteString(line)
				body.WriteString("\n")
				continue
			}
			value = strings.TrimSpace(value)
			name := strings.ToLower(strings.TrimSpace(key))
			// Neither first-wins nor last-wins is safe. Last-wins admits
			// `verdict: FAIL` followed by `verdict: PASS`. First-wins is worse:
			// a prose line like "Reviewer: see the lane assignment below"
			// lowercases to a REAL key and permanently shadows the honest
			// `reviewer:` beneath it, defeating the coordinator, reviewed-head
			// and family gates at once. A key that appears twice with different
			// values is ambiguous, and an ambiguous correctness gate must
			// refuse rather than pick.
			if prior, dup := seen[name]; dup {
				if prior != value {
					a.ConflictingHeaders = append(a.ConflictingHeaders, name)
				}
				continue
			}
			seen[name] = value
			switch name {
			case "sha":
				a.SHA = value
			case "branch":
				a.Branch = value
			case "task", "task-id", "card", "ticket":
				a.TaskRef = NormalizeTaskRef(value)
			case "reviewer":
				a.Reviewer = value
			case "authority", "asserting-authority":
				a.Authority = value
			case "reviewer-family":
				a.ReviewerFamily = value
			case "builder-family":
				a.BuilderFamily = value
			case "verdict":
				a.Verdict = strings.ToUpper(value)
			case "reviewed-head":
				// The reference key, written by every reviewer via
				// `reviewed-head: $(git rev-parse HEAD)`. Spelling this
				// anything else makes the wandering-reviewer gate dead code
				// that fails OPEN with no warning.
				a.ReadHead = value
			case "retry-of":
				a.RetryOf = value
			default:
				if unknownHeaderRe.MatchString(name) {
					a.UnknownHeaders = append(a.UnknownHeaders, name)
				}
			}
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	a.Body = body.String()
	if a.TaskRef == "" {
		a.TaskRef = inferTaskRef(a.Body, a.Reviewer)
	}
	return a
}

// BodyChars counts non-whitespace evidence.
func (a Artifact) BodyChars() int {
	return len(strings.Join(strings.Fields(a.Body), ""))
}

// Validate returns nil when the artifact may be admitted to the ledger.
// commitExists resolves a SHA against the repository; pass nil to skip that
// check (fixtures).
func (a Artifact) Validate(coordinators map[string]struct{}, commitExists func(string) bool) error {
	// STRUCTURAL checks run FIRST so the error names the real defect. A markdown
	// title above the front matter ends the header region, which meant the
	// operator was told their sha was missing rather than that their title broke
	// the artifact.
	if a.MalformedHeaderRegion {
		return fmt.Errorf("front matter must be the leading block ending in ---; " +
			"a prose line before the headers can shadow a real one")
	}
	if len(a.ConflictingHeaders) > 0 {
		return fmt.Errorf("front-matter key(s) %s appear more than once with different values; "+
			"ambiguous provenance is refused rather than resolved by position",
			strings.Join(a.ConflictingHeaders, ", "))
	}
	// An unrecognised header is refused, not logged. A misspelled reviewed-head
	// silently disables the wandering-reviewer gate, and a field nothing reads
	// surfaces nothing at all. The accepted key set is published in
	// .herd/prompts/review-verdict.template.md — a blanket refusal against an
	// unwritten contract would make every reviewer's first artifact a guess.
	if len(a.UnknownHeaders) > 0 {
		return fmt.Errorf("unrecognised front-matter key(s): %s; accepted keys are "+
			"sha, branch, task, task-id, card, ticket, reviewer, authority, asserting-authority, reviewer-family, builder-family, verdict, reviewed-head, retry-of "+
			"(see .herd/prompts/review-verdict.template.md); a misspelled gate key silently "+
			"disables its gate, so this is refused rather than ignored",
			strings.Join(a.UnknownHeaders, ", "))
	}

	if !shaRe.MatchString(a.SHA) {
		return fmt.Errorf("sha is not a 40-hex commit id: %q", orMissing(a.SHA))
	}
	if commitExists != nil && !commitExists(a.SHA) {
		return fmt.Errorf("sha %s does not resolve to a commit in this repo", a.SHA)
	}
	if a.Verdict == "RETIRED" {
		if strings.TrimSpace(a.Authority) == "" {
			return fmt.Errorf("retirement authority is missing")
		}
		for c := range coordinators {
			if strings.EqualFold(strings.TrimSpace(a.Authority), strings.TrimSpace(c)) {
				if n := a.BodyChars(); n < MinBodyChars {
					return fmt.Errorf("retirement rationale is %d chars, below the %d-char evidence floor", n, MinBodyChars)
				}
				return nil
			}
		}
		return fmt.Errorf("retirement authority %q is not a coordinator", a.Authority)
	}
	if strings.TrimSpace(a.Reviewer) == "" {
		return fmt.Errorf("reviewer is missing")
	}
	switch a.Verdict {
	case "PASS", "FAIL", "BLOCKED":
	default:
		return fmt.Errorf("verdict must be PASS, FAIL or BLOCKED, got %q", orMissing(a.Verdict))
	}

	// The coordinator grading its own work is not review at any tier.
	// Case-insensitive: "Herdforge-Orchestrator" is the same identity as
	// "herdforge-orchestrator", and nothing downstream re-checks this.
	for c := range coordinators {
		if strings.EqualFold(strings.TrimSpace(a.Reviewer), strings.TrimSpace(c)) {
			return fmt.Errorf("reviewer %q is a coordinator; self-verification never qualifies", a.Reviewer)
		}
	}

	// Same family is not an independent read.
	if a.ReviewerFamily != "" && a.BuilderFamily != "" &&
		strings.EqualFold(strings.TrimSpace(a.ReviewerFamily), strings.TrimSpace(a.BuilderFamily)) {
		return fmt.Errorf("reviewer-family %q equals builder-family; not an independent review", a.ReviewerFamily)
	}

	// A verdict produced by reading a different tree is not a verdict about
	// the reviewed commit, and the ledger cannot tell the difference. Absent
	// is tolerated for older reviewers; a STATED mismatch never is.
	if a.ReadHead != "" && a.ReadHead != a.SHA {
		return fmt.Errorf("reviewer states it read %s but the verdict claims to be about %s; "+
			"a verdict from a different tree is not a verdict about this commit",
			shortSHA(a.ReadHead), shortSHA(a.SHA))
	}

	// A PASS with no evidence is the failure mode this whole gate exists for.
	if n := a.BodyChars(); n < MinBodyChars {
		return fmt.Errorf("verdict body is %d chars, below the %d-char evidence floor: "+
			"a verdict with no reasoning is not a review", n, MinBodyChars)
	}
	return nil
}

// ValidatePassDiff refuses a PASS verdict whose candidate has no diff against
// the integration base. PR #151 (FAC-212) merged with 0 additions, 0
// deletions, 0 files because the branch held only its anchor commit; the
// adversarial reviewer returned PASS because an empty diff has nothing wrong
// with it. A merge that changes no bytes is not a completed ticket.
//
// diffEmpty reports whether `git diff origin/main...sha` is empty. A nil
// callback skips the check (fixtures that don't exercise this path). A
// non-PASS verdict is never refused — a FAIL or BLOCKED for an empty diff is
// correct.
func (a Artifact) ValidatePassDiff(diffEmpty func(sha string) (bool, error)) error {
	if a.Verdict != "PASS" {
		return nil
	}
	if diffEmpty == nil {
		return nil
	}
	empty, err := diffEmpty(a.SHA)
	if err != nil {
		return fmt.Errorf("cannot verify candidate diff for sha %s: %w", shortSHA(a.SHA), err)
	}
	if empty {
		return fmt.Errorf("verdict PASS for sha %s but the candidate diff against origin/main is empty; "+
			"a merge that changes no bytes is not a completed ticket", shortSHA(a.SHA))
	}
	return nil
}

func orMissing(s string) string {
	if strings.TrimSpace(s) == "" {
		return "<missing>"
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// taskRefRe matches a board card reference such as CHA-2345.
var taskRefRe = regexp.MustCompile(`(?i)\b([a-z]{2,6})-([0-9]{1,6})\b`)

// bodyTaskRefRe matches only an explicit, self-declared card line in the body,
// e.g. "Task ID: CHA-2345". A bare ref anywhere in the prose is NOT accepted:
// reviews routinely cite sibling cards, and attributing a verdict to a merely
// mentioned card silently credits the wrong work.
var bodyTaskRefRe = regexp.MustCompile(`(?im)^\s*(?:task|task[ -]?id|card|ticket)\s*:\s*([a-z]{2,6}-[0-9]{1,6})\b`)

// reviewerSlugRe matches the card baked into a reviewer name, e.g.
// "review-cha-2345-claude". The reviewer name is minted from the card under
// review, so it is authoritative in a way that body prose is not.
var reviewerSlugRe = regexp.MustCompile(`(?i)review[-_]([a-z]{2,6}[-_][0-9]{1,6})`)

// NormalizeTaskRef canonicalises a card reference to upper-case PREFIX-NUMBER.
// It returns "" for anything that is not shaped like a card ref, so a malformed
// value is recorded as absent rather than as a plausible-looking wrong card.
func NormalizeTaskRef(v string) string {
	m := taskRefRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1]) + "-" + m[2]
}

// inferTaskRef recovers the card from an artifact that did not declare a
// `task:` header, using only authoritative sources: an explicit body
// declaration, then the reviewer name slug. Returns "" when neither is
// present — an unattributed verdict is strictly better than a misattributed
// one.
func inferTaskRef(body, reviewer string) string {
	if m := bodyTaskRefRe.FindStringSubmatch(body); m != nil {
		return NormalizeTaskRef(m[1])
	}
	if m := reviewerSlugRe.FindStringSubmatch(reviewer); m != nil {
		return NormalizeTaskRef(strings.ReplaceAll(m[1], "_", "-"))
	}
	return ""
}
