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
