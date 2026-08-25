package main

import "testing"

// FAC-628: reviewers on this fleet cannot prove authorship -- commits are
// authored under a shared human identity with no trailers -- so they write
// "unknown", often with a paragraph explaining why. Those reviews are real and
// were being destroyed for being candid: 69 refused in one sweep.
func TestHonestlyUnrecordedFamily_AcceptsCandidUnknowns(t *testing.T) {
	for _, in := range []string{
		"unknown",
		"UNKNOWN",
		"unrecorded",
		"unspecified (native assignment; not stated in packet)",
		"unproven",
		`unknown (no lane/model attribution found in repo, ledger, or worktree)`,
		"unknown, commits authored as the shared human identity",
		"unknown: no launch receipt",
	} {
		got, ok := honestlyUnrecordedFamily(in)
		if !ok {
			t.Errorf("candid unknown %q must be accepted", in)
			continue
		}
		if got != "unrecorded" {
			t.Errorf("%q normalised to %q, want unrecorded", in, got)
		}
	}
}

// A near-miss of a real family is an ASSERTION of authorship the reviewer did
// not verify. FAC-590 refuses those and this must not become a bypass.
func TestHonestlyUnrecordedFamily_RejectsTyposAndRealFamilies(t *testing.T) {
	for _, in := range []string{
		"anthropc",   // typo of anthropic
		"anthropic",  // a real family: must go through the normal gate
		"openai",
		"xai",
		"codex",      // a harness name, wrong but still an assertion
		"",
		"Kampe",      // the human git identity, seen in the wild
		"unknownish", // not a sentinel, must not prefix-match loosely
	} {
		if _, ok := honestlyUnrecordedFamily(in); ok {
			t.Errorf("%q must NOT be treated as an honest unknown", in)
		}
	}
}
