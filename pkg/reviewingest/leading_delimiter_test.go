package reviewingest

import (
	"strings"
	"testing"
)

const fmSHA = "005578799fdbd4686d2cf93398836f1e31fba4b8"

func headers() string {
	return "sha: " + fmSHA + "\n" +
		"branch: rescue/docs-custodian\ntask: PR-3114\n" +
		"reviewer: review-pr-3114-pool03\nreviewer-family: anthropic\n" +
		"builder-family: openai\nverdict: PASS\nreviewed-head: " + fmSHA + "\n"
}

// FAC-584: the parser required headers BEFORE the first `---`, but standard YAML
// front matter OPENS with `---`. A reviewer following the universal convention
// had its whole header block swallowed as body and was refused for a missing
// sha that was sitting right there. A finished cross-family PASS on PR-3114 died
// this way.
func TestParseAcceptsLeadingYAMLDelimiter(t *testing.T) {
	body := strings.Repeat("evidence ", 40)
	both := map[string]string{
		"bare (legacy)":    headers() + "---\n" + body,
		"yaml (leading)":   "---\n" + headers() + "---\n" + body,
		"yaml with blanks": "\n\n---\n" + headers() + "---\n" + body,
	}
	for name, text := range both {
		a := Parse(text)
		if a.SHA != fmSHA {
			t.Errorf("%s: sha = %q want %q", name, a.SHA, fmSHA)
		}
		if a.Verdict != "PASS" {
			t.Errorf("%s: verdict = %q want PASS", name, a.Verdict)
		}
		if a.TaskRef != "PR-3114" {
			t.Errorf("%s: task = %q want PR-3114", name, a.TaskRef)
		}
		if a.MalformedHeaderRegion {
			t.Errorf("%s: must not be flagged malformed", name)
		}
		if err := a.Validate(nil, func(string) bool { return true }); err != nil {
			t.Errorf("%s: Validate: %v", name, err)
		}
	}
}

// The header-shadowing guard is a security property and must survive this
// change: prose above the real headers still ends the region, whether or not a
// leading delimiter was consumed first.
func TestLeadingDelimiterDoesNotWeakenShadowingGuard(t *testing.T) {
	// Two shadowing shapes. The mechanism differs — a key-shaped line becomes a
	// conflicting duplicate, a non-key line poisons the region — but the security
	// outcome must be identical: REFUSED. Asserting the outcome rather than the
	// mechanism is the point; my first version of this test asserted
	// MalformedHeaderRegion and failed while the artifact was still correctly
	// rejected as a conflict.
	cases := map[string]string{
		"key-shaped shadow": "---\nReviewer: see the lane assignment below\n" + headers() + "---\n" + strings.Repeat("evidence ", 40),
		"prose shadow":      "---\nthis is just a sentence\n" + headers() + "---\n" + strings.Repeat("evidence ", 40),
	}
	for name, text := range cases {
		a := Parse(text)
		if err := a.Validate(nil, func(string) bool { return true }); err == nil {
			t.Errorf("%s: a shadowed artifact must be refused, got nil error "+
				"(malformed=%v conflicting=%v reviewer=%q)",
				name, a.MalformedHeaderRegion, a.ConflictingHeaders, a.Reviewer)
		}
	}
}

// Only ONE delimiter is consumed, so a document that is genuinely body-first
// still fails closed instead of having its second fence treated as headers.
func TestOnlyOneLeadingDelimiterIsConsumed(t *testing.T) {
	a := Parse("---\n---\nsha: " + fmSHA + "\nbody text here")
	if a.SHA != "" {
		t.Errorf("second delimiter must open the body, got sha %q", a.SHA)
	}
}

func TestCutLeadingDelimiterLeavesNonDelimiterTextAlone(t *testing.T) {
	for _, in := range []string{
		"sha: abc\n---\nbody",
		"---not-a-delimiter\nsha: abc",
		"",
	} {
		if got := cutLeadingDelimiter(in); got != in {
			t.Errorf("cutLeadingDelimiter(%q) = %q, want unchanged", in, got)
		}
	}
}
