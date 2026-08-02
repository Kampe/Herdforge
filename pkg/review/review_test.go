package review

import (
	"context"
	"testing"
)

func TestClassifyRiskTier(t *testing.T) {
	tests := []struct {
		files    []string
		expected RiskTier
	}{
		{[]string{"README.md", "docs/spec.md"}, TierR0RiskMechanical},
		{[]string{"pkg/config/config.go"}, TierR1RiskStandard},
		{[]string{"pkg/auth/jwt.go"}, TierR3RiskCritical},
	}

	for _, tt := range tests {
		got := ClassifyRiskTier(tt.files)
		if got != tt.expected {
			t.Errorf("ClassifyRiskTier(%v) = %s, expected %s", tt.files, got, tt.expected)
		}
	}
}

func TestSelectCrossFamilyReviewer(t *testing.T) {
	available := []string{"anthropic/claude-3-7-sonnet", "google/gemini-2.5-flash", "openai/gpt-4o"}

	reviewer, err := SelectCrossFamilyReviewer("anthropic", available)
	if err != nil || reviewer != "google/gemini-2.5-flash" {
		t.Errorf("expected google/gemini-2.5-flash reviewer for anthropic author, got %s (err: %v)", reviewer, err)
	}
}

func TestSelectCrossFamilyReviewer_Fallback(t *testing.T) {
	reviewer, err := SelectCrossFamilyReviewer("anthropic", []string{"anthropic/claude-3-5-sonnet"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reviewer != "anthropic/claude-3-5-sonnet" {
		t.Errorf("expected fallback to first reviewer, got %s", reviewer)
	}
}

func TestSelectCrossFamilyReviewer_Empty(t *testing.T) {
	_, err := SelectCrossFamilyReviewer("anthropic", nil)
	if err == nil {
		t.Fatal("expected error for empty reviewers")
	}
}

func TestComputePatchID_GitError(t *testing.T) {
	rel := NewReviewEngine("/nonexistent-repo-xyzzy")
	_, err := rel.ComputePatchID(context.Background(), "abc123")
	if err == nil {
		t.Fatal("expected error for nonexistent repo")
	}
}

func TestRebaseMergeBranch_FetchError(t *testing.T) {
	rel := NewReviewEngine("/nonexistent-repo-xyzzy")
	_, err := rel.RebaseMergeBranch(context.Background(), "feature", "main")
	if err == nil {
		t.Fatal("expected error for nonexistent repo")
	}
}
