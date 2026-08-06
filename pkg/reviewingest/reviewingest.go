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

// Artifact is a parsed reviewer verdict.
type Artifact struct {
	SHA            string
	Branch         string
	Reviewer       string
	ReviewerFamily string
	BuilderFamily  string
	Verdict        string
	// ReadHead is the HEAD the reviewer states it actually read. Provenance,
	// not proof — but a truthful reviewer working from a pinned disposable
	// worktree reports the pin without effort, and a wandering one has to
	// state a mismatch or lie outright.
	ReadHead string
	Body     string
}

// Parse reads a front-matter artifact: `key: value` lines, then `---`, then a
// free-form body.
func Parse(text string) Artifact {
	var a Artifact
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inBody := false
	var body strings.Builder

	for sc.Scan() {
		line := sc.Text()
		if !inBody {
			if strings.TrimSpace(line) == "---" {
				inBody = true
				continue
			}
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "sha":
				a.SHA = value
			case "branch":
				a.Branch = value
			case "reviewer":
				a.Reviewer = value
			case "reviewer-family":
				a.ReviewerFamily = value
			case "builder-family":
				a.BuilderFamily = value
			case "verdict":
				a.Verdict = strings.ToUpper(value)
			case "read-head", "head-read", "read_head":
				a.ReadHead = value
			}
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	a.Body = body.String()
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
	if !shaRe.MatchString(a.SHA) {
		return fmt.Errorf("sha is not a 40-hex commit id: %q", orMissing(a.SHA))
	}
	if commitExists != nil && !commitExists(a.SHA) {
		return fmt.Errorf("sha %s does not resolve to a commit in this repo", a.SHA)
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
	if coordinators != nil {
		if _, isCoord := coordinators[a.Reviewer]; isCoord {
			return fmt.Errorf("reviewer %q is a coordinator; self-verification never qualifies", a.Reviewer)
		}
	}

	// Same family is not an independent read.
	if a.ReviewerFamily != "" && a.BuilderFamily != "" && a.ReviewerFamily == a.BuilderFamily {
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
