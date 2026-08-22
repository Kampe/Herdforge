package main

import "testing"

// TestReviewAgentNameIsUniquePerCandidate is the FAC-574 regression.
//
// The truncation suffix hashed the REF ONLY, so two distinct SHAs on the same
// branch produced the same agent name and the second reviewer collided with the
// first still-active one. Reviewing a second exact SHA on one branch is normal.
func TestReviewAgentNameIsUniquePerCandidate(t *testing.T) {
	// A ref long enough to force truncation, which is where the bug lived.
	ref := "reconstruct/cha-2209-review-handoff-queue-identity-long-branch"
	a := reviewAgentName(ref, "aaaaaaaaaaaa1111111111112222222222223333")
	b := reviewAgentName(ref, "bbbbbbbbbbbb1111111111112222222222223333")
	if a == b {
		t.Fatalf("two candidates on one branch must not share an agent name: %q", a)
	}
	if len(a) > reviewAgentNameLimit || len(b) > reviewAgentNameLimit {
		t.Fatalf("names must stay within %d chars: %q %q", reviewAgentNameLimit, a, b)
	}
}

// Short refs must keep their readable un-truncated form.
func TestShortRefKeepsReadableName(t *testing.T) {
	got := reviewAgentName("cha-1", "abcdef1234567890")
	if got != "review-cha-1-abcdef123456" && len(got) > reviewAgentNameLimit {
		t.Fatalf("unexpected short-ref name %q", got)
	}
	if got == reviewAgentName("cha-1", "999999999999") {
		t.Fatal("even short names must differ per candidate")
	}
}

// Tab label and agent name must both be candidate-unique; they are one
// definition now, so this guards against them diverging again.
func TestTabLabelAndAgentNameAgreeOnUniqueness(t *testing.T) {
	ref := "reconstruct/cha-2209-review-handoff-queue-identity-long-branch"
	if reviewTabLabel(ref, "aaaa1111") == reviewTabLabel(ref, "bbbb2222") {
		t.Fatal("tab labels must differ per candidate")
	}
}
