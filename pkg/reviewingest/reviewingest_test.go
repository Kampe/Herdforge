package reviewingest

import (
	"strings"
	"testing"
)

const realSHA = "0123456789abcdef0123456789abcdef01234567"

var coordinators = map[string]struct{}{"herdforge-orchestrator": {}, "coordinator": {}}

func exists(string) bool { return true }

func artifact(reviewer, rfam, bfam, verdict, readHead, body string) string {
	var b strings.Builder
	b.WriteString("sha: " + realSHA + "\n")
	b.WriteString("reviewer: " + reviewer + "\n")
	if rfam != "" {
		b.WriteString("reviewer-family: " + rfam + "\n")
	}
	if bfam != "" {
		b.WriteString("builder-family: " + bfam + "\n")
	}
	b.WriteString("verdict: " + verdict + "\n")
	if readHead != "" {
		b.WriteString("reviewed-head: " + readHead + "\n")
	}
	b.WriteString("---\n")
	b.WriteString(body)
	return b.String()
}

var longBody = strings.Repeat("Checked the router unpin against the waterfall and re-ran the suite. ", 6)

func TestWellFormedIndependentPassIsAccepted(t *testing.T) {
	a := Parse(artifact("review-kimi", "moonshot", "anthropic", "PASS", realSHA, longBody))
	if err := a.Validate(coordinators, exists); err != nil {
		t.Fatalf("a well-formed independent PASS must be accepted: %v", err)
	}
	if a.Verdict != "PASS" || a.Reviewer != "review-kimi" {
		t.Fatalf("parse lost fields: %+v", a)
	}
}

// The coordinator grading its own work is not review at any tier.
func TestCoordinatorSelfVerdictIsRefused(t *testing.T) {
	a := Parse(artifact("herdforge-orchestrator", "openai", "anthropic", "PASS", realSHA, longBody))
	err := a.Validate(coordinators, exists)
	if err == nil || !strings.Contains(err.Error(), "self-verification") {
		t.Fatalf("coordinator self-verdict must be refused, got %v", err)
	}
}

func TestSameFamilyIsNotAnIndependentReview(t *testing.T) {
	a := Parse(artifact("review-x", "anthropic", "anthropic", "PASS", realSHA, longBody))
	err := a.Validate(coordinators, exists)
	if err == nil || !strings.Contains(err.Error(), "not an independent review") {
		t.Fatalf("same-family review must be refused, got %v", err)
	}
}

// The failure this whole gate exists for.
func TestPassWithNoEvidenceIsRefused(t *testing.T) {
	a := Parse(artifact("review-y", "moonshot", "anthropic", "PASS", realSHA, "looks good to me"))
	err := a.Validate(coordinators, exists)
	if err == nil || !strings.Contains(err.Error(), "evidence floor") {
		t.Fatalf("a PASS with no reasoning must be refused, got %v", err)
	}
}

// A verdict produced by reading a different tree is not a verdict about this
// commit, and the ledger cannot tell the difference afterwards.
func TestStatedReadHeadMismatchIsRefused(t *testing.T) {
	other := strings.Repeat("b", 40)
	a := Parse(artifact("review-z", "moonshot", "anthropic", "PASS", other, longBody))
	err := a.Validate(coordinators, exists)
	if err == nil || !strings.Contains(err.Error(), "different tree") {
		t.Fatalf("a stated read-head mismatch must be refused, got %v", err)
	}
	// Absent read-head is tolerated: older reviewers predate the field.
	a2 := Parse(artifact("review-z", "moonshot", "anthropic", "PASS", "", longBody))
	if err := a2.Validate(coordinators, exists); err != nil {
		t.Fatalf("an absent read-head must still be accepted: %v", err)
	}
}

func TestMalformedShaAndVerdictAreRefused(t *testing.T) {
	bad := Parse("sha: nope\nreviewer: r\nverdict: PASS\n---\n" + longBody)
	if err := bad.Validate(coordinators, exists); err == nil || !strings.Contains(err.Error(), "40-hex") {
		t.Fatalf("a non-sha must be refused, got %v", err)
	}
	unknown := Parse(artifact("review-a", "moonshot", "anthropic", "LGTM", realSHA, longBody))
	if err := unknown.Validate(coordinators, exists); err == nil || !strings.Contains(err.Error(), "PASS, FAIL or BLOCKED") {
		t.Fatalf("an unknown verdict must be refused, got %v", err)
	}
	missing := Parse(artifact("", "moonshot", "anthropic", "PASS", realSHA, longBody))
	if err := missing.Validate(coordinators, exists); err == nil || !strings.Contains(err.Error(), "reviewer is missing") {
		t.Fatalf("a missing reviewer must be refused, got %v", err)
	}
}

// A SHA that does not resolve is not reviewable.
func TestUnresolvableShaIsRefused(t *testing.T) {
	a := Parse(artifact("review-z", "moonshot", "anthropic", "PASS", realSHA, longBody))
	err := a.Validate(coordinators, func(string) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("an unresolvable sha must be refused, got %v", err)
	}
}

// FAIL and BLOCKED still need evidence — a bare rejection is unactionable.
func TestFailAndBlockedAlsoNeedEvidence(t *testing.T) {
	for _, v := range []string{"FAIL", "BLOCKED"} {
		thin := Parse(artifact("review-y", "moonshot", "anthropic", v, realSHA, "nope"))
		if err := thin.Validate(coordinators, exists); err == nil {
			t.Fatalf("a bare %s must be refused", v)
		}
		full := Parse(artifact("review-y", "moonshot", "anthropic", v, realSHA, longBody))
		if err := full.Validate(coordinators, exists); err != nil {
			t.Fatalf("a reasoned %s must be accepted: %v", v, err)
		}
	}
}
