package reviewingest

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// VerificationDigest returns a stable digest of the verification a reviewer
// states it actually ran, or "" when the artifact records none.
//
// FAC-658: Ledger.Admit binds a verification digest, and across 2210 live ledger
// rows the key never appeared once. It is the last of the four bindings that
// made harvest admission structurally unsatisfiable.
//
// It is also the one binding that must NOT be synthesised. Task, lease and patch
// id are identities that can be computed from the system itself, so recording
// them adds no claim. A verification digest asserts that verification HAPPENED,
// and hashing the artifact text or the SHA would produce a value that satisfies
// the gate's shape while proving nothing -- the same failure this tree already
// refuses for an empty lease. A digest that certifies nothing is worse than an
// honest absence, because absence is visible and a false digest is not.
//
// So it digests only the reviewer's own verification section: the commands run
// and their outcomes. Reviewers already write this -- the live corpus contains
// real entries such as `go test ./pkg/...` results, `zsh -n` checks, and
// non-vacuous test swaps with observed failures. A reviewer that records nothing
// gets no digest, and its verdict remains inadmissible under the existing gate,
// which is the correct outcome rather than a bug.
//
// The digest is over NORMALISED content, so reflowing whitespace does not change
// it, while changing a command or a result does.
func (a Artifact) VerificationDigest() string {
	section := a.VerificationEvidence()
	if section == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(section))
	return fmt.Sprintf("%x", sum)[:32]
}

// verificationHeadings are the section labels reviewers actually use for the
// record of what they executed. Matching is case-insensitive and anchored to a
// line start so a passing mention inside prose cannot open a section.
var verificationHeadings = []string{
	"tests run",
	"verification",
	"verification evidence",
	"commands run",
}

// VerificationEvidence extracts and normalises the reviewer's record of what it
// executed. Returns "" when the artifact records none.
func (a Artifact) VerificationEvidence() string {
	lines := strings.Split(a.Body, "\n")
	var out []string
	collecting := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(strings.TrimLeft(line, "#*- "))
		lower := strings.ToLower(trimmed)
		if inline, ok := verificationHeadingContent(trimmed); ok {
			collecting = true
			// A heading may carry its evidence on the SAME line. 254 of 726 live
			// artifacts write `Tests run: <command> — <result>` inline, which a
			// heading-only matcher silently skipped: it reported 259 artifacts as
			// recording no verification when they had recorded it all along, and
			// would have left them permanently inadmissible for a formatting
			// choice rather than a missing check.
			if inline != "" {
				out = append(out, strings.Join(strings.Fields(inline), " "))
			}
			continue
		}
		_ = lower
		if !collecting {
			continue
		}
		// A new heading ends the section. Indented or bulleted content belongs
		// to it; a fresh unindented heading-looking line does not.
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		if strings.HasPrefix(line, "#") || (isSectionHeading(lower) && !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "\t") && !strings.HasPrefix(raw, "-")) {
			collecting = false
			continue
		}
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	// Drop blank padding so reflowed whitespace cannot change the digest.
	var kept []string
	for _, l := range out {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// verificationHeadingContent reports whether a line opens the verification
// section, and returns any evidence written on that same line.
func verificationHeadingContent(trimmed string) (string, bool) {
	idx := strings.Index(trimmed, ":")
	if idx < 0 {
		// A bare heading with no colon, e.g. a markdown "## Tests run".
		for _, h := range verificationHeadings {
			if strings.EqualFold(strings.TrimSpace(trimmed), h) {
				return "", true
			}
		}
		return "", false
	}
	label := strings.ToLower(strings.TrimSpace(trimmed[:idx]))
	for _, h := range verificationHeadings {
		if label == h {
			return strings.TrimSpace(trimmed[idx+1:]), true
		}
	}
	return "", false
}

// isSectionHeading recognises the other labelled sections a review artifact uses,
// so the verification section ends where the next one begins.
func isSectionHeading(lower string) bool {
	lower = strings.TrimSuffix(strings.TrimSpace(lower), ":")
	for _, h := range []string{
		"verdict", "rubric", "required findings", "optional findings",
		"acceptance criteria", "merge recommendation", "residual risk",
		"invariant and adr result", "skills used", "task id", "model family",
	} {
		if strings.HasPrefix(lower, h) {
			return true
		}
	}
	return false
}
