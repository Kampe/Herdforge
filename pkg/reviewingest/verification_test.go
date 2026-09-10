package reviewingest

import (
	"strings"
	"testing"
)

const withTests = `Verdict: PASS

Tests run:
- ` + "`go test ./pkg/review`" + ` — ok, 12 tests
- ` + "`zsh -n bin/thing`" + ` — syntax OK

Rubric (0-2 each):
  correctness: 2 — irrelevant to the digest
`

// FAC-658: the verification digest is the one binding that must NOT be
// synthesised. Task, lease and patch id are identities computable from the
// system, so recording them adds no claim. A verification digest asserts that
// verification HAPPENED; hashing the artifact text or the SHA would satisfy the
// gate's shape while proving nothing -- worse than absence, because absence is
// visible and a false digest is not.
func TestVerificationDigestCoversOnlyTheReviewersOwnEvidence(t *testing.T) {
	a := Parse("sha: abc\n---\n" + withTests)
	ev := a.VerificationEvidence()
	if !strings.Contains(ev, "go test ./pkg/review") {
		t.Fatalf("the recorded commands must be captured: %q", ev)
	}
	if strings.Contains(ev, "correctness") {
		t.Errorf("the rubric is not verification evidence and must not be digested: %q", ev)
	}
	if a.VerificationDigest() == "" {
		t.Error("an artifact that records real verification must produce a digest")
	}
}

// A reviewer that records NOTHING gets no digest, and its verdict stays
// inadmissible under the existing gate. That is the correct outcome, not a bug:
// the alternative is certifying verification that never happened.
func TestVerificationDigestIsEmptyWhenNothingWasRecorded(t *testing.T) {
	a := Parse("sha: abc\n---\nVerdict: PASS\n\nLooks fine to me.\n")
	if got := a.VerificationDigest(); got != "" {
		t.Fatalf("an artifact with no verification section must produce NO digest, got %q", got)
	}
}

// Reflowing whitespace must not change the digest, or a cosmetic edit would
// invalidate an admitted verdict.
func TestVerificationDigestIsStableAcrossWhitespace(t *testing.T) {
	a := Parse("sha: abc\n---\n" + withTests)
	spaced := strings.ReplaceAll(withTests, "- `go test", "-   `go test")
	b := Parse("sha: abc\n---\n" + spaced)
	if a.VerificationDigest() != b.VerificationDigest() {
		t.Error("whitespace reflow must not change the digest")
	}
}

// Changing a COMMAND or a RESULT must change the digest, or it certifies nothing.
func TestVerificationDigestChangesWhenTheEvidenceChanges(t *testing.T) {
	a := Parse("sha: abc\n---\n" + withTests)
	altered := strings.Replace(withTests, "ok, 12 tests", "FAIL, 3 tests", 1)
	b := Parse("sha: abc\n---\n" + altered)
	if a.VerificationDigest() == b.VerificationDigest() {
		t.Fatal("a changed test RESULT must change the digest, or it proves nothing")
	}
	cmd := strings.Replace(withTests, "go test ./pkg/review", "go test ./pkg/other", 1)
	c := Parse("sha: abc\n---\n" + cmd)
	if a.VerificationDigest() == c.VerificationDigest() {
		t.Fatal("a changed COMMAND must change the digest")
	}
}

// A passing mention of the words in prose must not open a section, or unrelated
// text would be digested as evidence.
func TestVerificationDigestIgnoresProseMentions(t *testing.T) {
	a := Parse("sha: abc\n---\nVerdict: PASS\n\nI could not see which tests run in CI.\n")
	if got := a.VerificationDigest(); got != "" {
		t.Fatalf("a prose mention must not open a verification section, got %q", got)
	}
}

// FAC-658: a heading may carry its evidence on the SAME line. 254 of 726 live
// artifacts write `Tests run: <command> — <result>` inline, and a heading-only
// matcher silently skipped every one of them: it reported 259 artifacts as
// recording no verification when they had recorded it all along, which would
// have left them permanently inadmissible for a FORMATTING choice rather than a
// missing check. Fixing it took real coverage from 467/726 to 721/726.
func TestVerificationDigestReadsEvidenceWrittenInlineOnTheHeading(t *testing.T) {
	inline := Parse("sha: abc\n---\nVerdict: PASS\n\nTests run: `go test ./pkg/review` — ok, 12 tests\n")
	ev := inline.VerificationEvidence()
	if !strings.Contains(ev, "go test ./pkg/review") {
		t.Fatalf("inline evidence must be captured: %q", ev)
	}
	if inline.VerificationDigest() == "" {
		t.Fatal("an artifact recording evidence inline must produce a digest")
	}
	// A changed inline result must still change the digest.
	other := Parse("sha: abc\n---\nVerdict: PASS\n\nTests run: `go test ./pkg/review` — FAIL, 3 tests\n")
	if inline.VerificationDigest() == other.VerificationDigest() {
		t.Error("a changed inline result must change the digest")
	}
}

// A label that merely ends in a colon must not open the section, or arbitrary
// prose would be digested as verification evidence.
func TestVerificationDigestDoesNotTreatAnyColonLineAsEvidence(t *testing.T) {
	a := Parse("sha: abc\n---\nVerdict: PASS\n\nResidual risk: none that I can see.\n")
	if got := a.VerificationDigest(); got != "" {
		t.Fatalf("an unrelated labelled line must not open a verification section, got %q", got)
	}
}

// FAC-795: actual retained FAC-792 review artifacts contain child Markdown
// headings under `## Tests run` (e.g. `### Non-Vacuity Guard Regression`).
// The parser must include nested subsections and terminate only at a sibling
// or ancestor heading.
func TestVerificationDigestExtractsNestedHeadingsAndSubsections(t *testing.T) {
	artifact := `sha: abc
---
Verdict: PASS

## Findings and risk

1. Independent review confirmed exact candidate identity.

## Tests run

### Non-Vacuity Guard Regression
- ` + "`go test ./pkg/harness/opencode -run TestConsumption`" + ` — FAIL (RED 1)
- ` + "`go test ./pkg/harness/opencode -run TestConsumption`" + ` — PASS (GREEN 0)

### Package Tests
- ` + "`go test ./pkg/harness/...`" + ` — ok

## Author instructions

Replace every placeholder with your own evidence.
`
	a := Parse(artifact)
	ev := a.VerificationEvidence()
	if !strings.Contains(ev, "Non-Vacuity Guard Regression") {
		t.Fatalf("nested subsection heading must be included in evidence, got: %q", ev)
	}
	if !strings.Contains(ev, "go test ./pkg/harness/opencode") {
		t.Fatalf("nested command evidence must be included, got: %q", ev)
	}
	if !strings.Contains(ev, "Package Tests") {
		t.Fatalf("second subsection heading must be included, got: %q", ev)
	}
	if !strings.Contains(ev, "go test ./pkg/harness/...") {
		t.Fatalf("second subsection command evidence must be included, got: %q", ev)
	}
	if strings.Contains(ev, "Author instructions") || strings.Contains(ev, "Replace every placeholder") {
		t.Fatalf("sibling heading and following content must not be included in evidence, got: %q", ev)
	}
	if a.VerificationDigest() == "" {
		t.Fatal("nested verification evidence must produce a nonempty digest")
	}
}

func TestVerificationDigestTerminatesAtSiblingAndAncestorHeadings(t *testing.T) {
	// Level 3 verification section: children (level 4) included; sibling (level 3) and ancestor (level 2, 1) terminate.
	l3 := `sha: abc
---
Verdict: PASS

### Verification Evidence

#### Unit Suite
- ` + "`go test ./pkg/reviewingest`" + ` — PASS

### Rubric
correctness: 2
`
	a := Parse(l3)
	ev := a.VerificationEvidence()
	if !strings.Contains(ev, "Unit Suite") || !strings.Contains(ev, "go test ./pkg/reviewingest") {
		t.Fatalf("level 4 child heading must be included in level 3 section: %q", ev)
	}
	if strings.Contains(ev, "Rubric") || strings.Contains(ev, "correctness") {
		t.Fatalf("level 3 sibling heading must terminate section: %q", ev)
	}

	// Level 1 verification section: children (level 2) included; sibling (level 1) terminates.
	l1 := `sha: abc
---
Verdict: PASS

# Verification

## Unit Suite
- ` + "`go test ./pkg/reviewingest`" + ` — PASS

# Residual risk
none
`
	b := Parse(l1)
	ev1 := b.VerificationEvidence()
	if !strings.Contains(ev1, "Unit Suite") {
		t.Fatalf("level 2 child heading must be included in level 1 section: %q", ev1)
	}
	if strings.Contains(ev1, "Residual risk") {
		t.Fatalf("level 1 sibling heading must terminate section: %q", ev1)
	}
}

func TestVerificationDigestPreservesCodeFencesWithHeadingLikeLines(t *testing.T) {
	artifact := `sha: abc
---
Verdict: PASS

## Tests run

` + "```bash" + `
# Comment inside code fence
go test ./pkg/reviewingest
### Another comment
PASS
` + "```" + `

## Author instructions
Instructions here.
`
	a := Parse(artifact)
	ev := a.VerificationEvidence()
	if !strings.Contains(ev, "# Comment inside code fence") {
		t.Fatalf("code fence comment must not be treated as a section terminator: %q", ev)
	}
	if !strings.Contains(ev, "go test ./pkg/reviewingest") {
		t.Fatalf("command inside code fence must be captured: %q", ev)
	}
	if !strings.Contains(ev, "### Another comment") {
		t.Fatalf("subheading-like line inside code fence must be captured: %q", ev)
	}
	if strings.Contains(ev, "Author instructions") {
		t.Fatalf("sibling heading after code fence must terminate section: %q", ev)
	}
	if a.VerificationDigest() == "" {
		t.Fatal("code fence evidence must produce a nonempty digest")
	}
}

func TestVerificationDigestEmptySubsectionsFailClosed(t *testing.T) {
	artifact := `sha: abc
---
Verdict: PASS

## Tests run

### Non-Vacuity Guard Regression

### Full Unit Suite

## Author instructions
Replace placeholders.
`
	a := Parse(artifact)
	if got := a.VerificationEvidence(); got != "" {
		t.Fatalf("empty subsections must produce no evidence, got %q", got)
	}
	if got := a.VerificationDigest(); got != "" {
		t.Fatalf("empty subsections must produce no digest, got %q", got)
	}
}

