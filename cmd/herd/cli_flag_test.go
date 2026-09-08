package main

import (
	"strings"
	"testing"
)

func TestTakeCLIFlagAcceptsSpaceAndAttachedForms(t *testing.T) {
	sha := strings.Repeat("a", 40)
	value, next, matched, err := takeCLIFlag([]string{"--candidate", sha, "--receipt", "x"}, 0, "--candidate")
	if err != nil || !matched || value != sha || next != 1 {
		t.Fatalf("space form: value=%q next=%d matched=%v err=%v", value, next, matched, err)
	}
	value, next, matched, err = takeCLIFlag([]string{"--candidate=" + sha}, 0, "--candidate")
	if err != nil || !matched || value != sha || next != 0 {
		t.Fatalf("attached form: value=%q next=%d matched=%v err=%v", value, next, matched, err)
	}
	_, _, matched, err = takeCLIFlag([]string{"--candidate"}, 0, "--candidate")
	if !matched || err == nil {
		t.Fatal("missing value must match and error")
	}
	_, _, matched, err = takeCLIFlag([]string{"--reviewer", "r"}, 0, "--candidate")
	if matched || err != nil {
		t.Fatalf("other flag matched=%v err=%v", matched, err)
	}
}

func TestParseReviewHostIngestArgsAcceptsAttachedCandidate(t *testing.T) {
	sha := strings.Repeat("b", 40)
	got, err := parseReviewHostIngestArgs([]string{
		"--candidate=" + sha,
		"--reviewer", "review-fac-999",
		"--receipt", "receipt.json",
	})
	if err != nil {
		t.Fatalf("attached candidate: %v", err)
	}
	if got.Candidate != sha {
		t.Fatalf("candidate=%q", got.Candidate)
	}
}

func TestParseReviewBindEvidenceArgsAcceptsAttachedCandidate(t *testing.T) {
	sha := strings.Repeat("c", 40)
	got, err := parseReviewBindEvidenceArgs([]string{
		"FAC-765",
		"--candidate=" + sha,
		"--receipt", "sha256:deadbeef",
	})
	if err != nil {
		t.Fatalf("attached candidate: %v", err)
	}
	if got.Candidate != sha || got.Task != "FAC-765" {
		t.Fatalf("parsed %+v", got)
	}
}
