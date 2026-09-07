package main

import (
	"strings"
	"testing"
)

func TestParseReviewHostIngestRefusesFlagOnlyTrust(t *testing.T) {
	_, err := parseReviewHostIngestArgs([]string{
		"--candidate", strings.Repeat("a", 40),
		"--reviewer", "review-fac-652-2a3a20d57ba7",
		"--receipt", "receipt.json",
		"--host", "w4",
	})
	if err == nil || !strings.Contains(err.Error(), "not authentication") {
		t.Fatalf("--host error = %v", err)
	}
	_, err = parseReviewHostIngestArgs([]string{
		"--candidate", strings.Repeat("a", 40),
		"--reviewer", "review-fac-652-2a3a20d57ba7",
		"--receipt", "receipt.json",
		"--family", "openai",
	})
	if err == nil || !strings.Contains(err.Error(), "not authentication") {
		t.Fatalf("--family error = %v", err)
	}
}
