package reviewingest

import (
	"strings"
	"testing"
)

const advSHA = "7718fb5a9924aaaabbbbccccddddeeeeffff0000"

func advArtifact(extra string) string {
	return "sha: " + advSHA + "\nbranch: b\ntask: CHA-2701\n" +
		"reviewer: review-cha-2701-claude\nreviewer-family: anthropic\n" +
		"builder-family: openai\nverdict: FAIL\nreviewed-head: " + advSHA + "\n" +
		extra + "---\n" + strings.Repeat("evidence ", 40) + "\n"
}

// FAC-590: reviewers volunteer advisory keys. The unknown-key gate exists to
// catch a MISSPELLED gate key — a typo'd reviewed-head silently disables the
// wandering-reviewer check — not to discard a finished verdict over one extra
// informational line. Real FAIL verdicts for CHA-2701 and CHA-2703 died at
// ingest for exactly that.
func TestAdvisoryKeysDoNotRefuseAVerdict(t *testing.T) {
	for _, key := range []string{
		"merge-recommendation", "recommendation", "confidence",
		"skills-used", "model-family", "provider", "model",
	} {
		a := Parse(advArtifact(key + ": something\n"))
		if len(a.UnknownHeaders) != 0 {
			t.Errorf("%q must be accepted as advisory, got unknown %v", key, a.UnknownHeaders)
		}
		if err := a.Validate(nil, func(string) bool { return true }); err != nil {
			t.Errorf("%q must not refuse the verdict: %v", key, err)
		}
		if a.Verdict != "FAIL" {
			t.Errorf("%q: verdict lost, got %q", key, a.Verdict)
		}
	}
}

// The security property. A typo of a GATE key must still be refused, or
// tolerating advisory keys would silently disable the gate the typo belongs to.
func TestTypoOfAGateKeyIsStillRefused(t *testing.T) {
	typos := []string{
		"reviewed-hea",    // dropped char
		"reviewed-heads",  // extra char
		"reviewer-familx", // substitution
		"verdic",
		"sh",
		"bilder-family",
	}
	for _, key := range typos {
		a := Parse(advArtifact(key + ": x\n"))
		if len(a.UnknownHeaders) == 0 {
			t.Errorf("%q is a near-miss of a gate key and must be refused, not ignored", key)
		}
	}
}

func TestNearMissDetectsSingleEdits(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"verdict", "verdict", true},
		{"verdic", "verdict", true},   // deletion
		{"verdictt", "verdict", true}, // insertion
		{"verdicx", "verdict", true},  // substitution
		{"merge-recommendation", "verdict", false},
		{"confidence", "reviewed-head", false},
		{"", "sha", false},
	}
	for _, c := range cases {
		if got := nearMiss(c.a, c.b); got != c.want {
			t.Errorf("nearMiss(%q,%q) = %v want %v", c.a, c.b, got, c.want)
		}
	}
}

// A genuinely unknown key that is not advisory and not a near-miss must still
// be refused: the allowlist is explicit, not permissive-by-default.
func TestUnlistedUnknownKeyStillRefused(t *testing.T) {
	a := Parse(advArtifact("totally-made-up-key: x\n"))
	if len(a.UnknownHeaders) == 0 {
		t.Error("an unlisted key must still be surfaced and refused")
	}
}
