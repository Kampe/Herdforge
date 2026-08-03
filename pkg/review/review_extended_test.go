package review

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestClassifyRiskTier_AllTiers(t *testing.T) {
	tests := []struct {
		files []string
		tier  RiskTier
	}{
		{[]string{"README.md", "docs/spec.md"}, TierR0RiskMechanical},
		{[]string{"pkg/config/config.go", "api/handler.go"}, TierR1RiskStandard},
		{[]string{"pkg/auth/jwt.go"}, TierR3RiskCritical},
		{[]string{"internal/secret/manager.go"}, TierR3RiskCritical},
		{[]string{"pkg/payment/stripe.go"}, TierR3RiskCritical},
		{[]string{"internal/money/handler.go"}, TierR3RiskCritical},
		{[]string{"Makefile", ".github/workflows/ci.yml"}, TierR0RiskMechanical},
		{[]string{"pkg/api/handler.ts", "pkg/core/engine.rs"}, TierR1RiskStandard},
	}

	for _, tt := range tests {
		got := ClassifyRiskTier(tt.files)
		if got != tt.tier {
			t.Errorf("ClassifyRiskTier(%v) = %s, expected %s", tt.files, got, tt.tier)
		}
	}
}

func TestSelectCrossFamilyReviewer_NoMatch(t *testing.T) {
	available := []string{"anthropic/claude-3-7-sonnet"}
	reviewer, err := SelectCrossFamilyReviewer("anthropic", available)
	if err != nil {
		t.Fatalf("expected fallback to only available reviewer, got: %v", err)
	}
	if reviewer != "anthropic/claude-3-7-sonnet" {
		t.Errorf("expected fallback to the only reviewer, got %s", reviewer)
	}
}

func TestSelectCrossFamilyReviewer_GoogleSkipsGemini(t *testing.T) {
	reviewer, err := SelectCrossFamilyReviewer("google", []string{"gemini-2.5-flash", "claude-3-5-sonnet"})
	if err != nil || reviewer != "claude-3-5-sonnet" {
		t.Fatalf("expected claude-3-5-sonnet (cross-family), got %s (err: %v)", reviewer, err)
	}
}

func TestSelectCrossFamilyReviewer_XAISkipsGrok(t *testing.T) {
	reviewer, err := SelectCrossFamilyReviewer("xai", []string{"grok-2", "claude-3-5-sonnet"})
	if err != nil || reviewer != "claude-3-5-sonnet" {
		t.Fatalf("expected claude-3-5-sonnet (cross-family), got %s (err: %v)", reviewer, err)
	}
}

func TestSelectCrossFamilyReviewer_NoReviewers(t *testing.T) {
	_, err := SelectCrossFamilyReviewer("anthropic", nil)
	if err == nil {
		t.Fatal("expected error for empty reviewer list")
	}
}

// patchIDFixture builds a repo whose HEAD is a real file commit — the live
// repo's HEAD is usually a PR merge commit, and `git show` on a merge emits
// no diff, so testing against "." was red whenever main's tip was a merge.
func patchIDFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@h.local"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(dir+"/f.txt", []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "f.txt"},
		{"commit", "-q", "-m", "feat: real diff"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestComputePatchID(t *testing.T) {
	r := NewReviewEngine(patchIDFixture(t))
	id, err := r.ComputePatchID(context.Background(), "HEAD")
	if err != nil {
		t.Fatalf("expected valid patch ID for HEAD, got: %v", err)
	}
	if len(id) == 0 {
		t.Error("expected non-empty patch ID")
	}
}

func TestComputePatchID_ShowFailure(t *testing.T) {
	r := NewReviewEngine(".")
	_, err := r.ComputePatchID(context.Background(), "nonexistent-sha-12345")
	if err == nil {
		t.Fatal("expected error for nonexistent commit")
	}
}

func TestComputePatchID_NoOutput(t *testing.T) {
	// A commit with no diff (empty commit, like a merge) must fail closed,
	// never return an empty id that a caller could mistake for a match.
	dir := patchIDFixture(t)
	c := exec.Command("git", "commit", "--allow-empty", "-q", "-m", "empty: no diff")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	r := NewReviewEngine(dir)
	if _, err := r.ComputePatchID(context.Background(), "HEAD"); err == nil {
		t.Error("empty-diff commit must error, not return an empty patch id")
	}
}

func TestComputePatchID_PipeStartFailure(t *testing.T) {
	r := NewReviewEngine("/nonexistent")
	_, err := r.ComputePatchID(context.Background(), "HEAD")
	if err == nil {
		t.Fatal("expected error for nonexistent repo")
	}
}

func TestRebaseMergeBranch_FailedFetch(t *testing.T) {
	r := NewReviewEngine("/nonexistent")
	res, err := r.RebaseMergeBranch(context.Background(), "feature-x", "main")
	if err == nil {
		t.Fatal("expected error for nonexistent repo")
	}
	if res.Merged {
		t.Errorf("expected Merged=false on failure")
	}
}

func TestRebaseMergeBranch_FailedCheckout(t *testing.T) {
	r := NewReviewEngine(".")
	res, err := r.RebaseMergeBranch(context.Background(), "nonexistent-branch-xyz", "nonexistent-target-xyz")
	if err == nil {
		t.Fatal("expected error for nonexistent branch")
	}
	if res.Merged {
		t.Errorf("expected Merged=false on failure")
	}
}

func TestRebaseMergeBranch_FailedMerge(t *testing.T) {
	r := NewReviewEngine(".")
	res, err := r.RebaseMergeBranch(context.Background(), "nonexistent-branch-xyz", "HEAD")
	if err == nil {
		t.Fatal("expected error for nonexistent branch merge")
	}
	if res.Merged {
		t.Errorf("expected Merged=false on failure")
	}
}

func TestRebaseMergeBranch_FailedRevParse(t *testing.T) {
	r := NewReviewEngine(".")
	res, err := r.RebaseMergeBranch(context.Background(), "HEAD", "HEAD")
	if err == nil {
		t.Fatal("expected error: git merge --rebase is not a valid git command")
	}
	if res.Merged {
		t.Errorf("expected Merged=false on failure")
	}
}
