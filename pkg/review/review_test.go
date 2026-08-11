package review

import (
	"context"
	"fmt"
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

func TestSelectJury_ThreeDistinctFamilies(t *testing.T) {
	available := []string{
		"anthropic/claude-3-7-sonnet",
		"google/gemini-2.5-flash",
		"openai/gpt-4o",
		"grok/grok-4",
	}
	jury, err := SelectJury("anthropic", available, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(jury) != 3 {
		t.Fatalf("expected 3 jurors, got %d", len(jury))
	}
	reg := NewFamilyRegistry()
	for _, j := range jury {
		if reg.Lookup(j) == FamilyAnthropic {
			t.Fatalf("author family should be excluded from jury; got %s", j)
		}
	}
	families := map[ModelFamily]bool{}
	for _, j := range jury {
		fam := reg.Lookup(j)
		if families[fam] {
			t.Fatalf("duplicate family in jury: %s for %s", fam, j)
		}
		families[fam] = true
	}
}

func TestSelectJury_ExcludesAuthorFamily(t *testing.T) {
	available := []string{
		"anthropic/claude-3-7-sonnet",
		"anthropic/claude-opus-4",
		"google/gemini-2.5-flash",
	}
	jury, err := SelectJury("anthropic", available, 3)
	if err != nil {
		t.Fatal(err)
	}
	reg := NewFamilyRegistry()
	for _, j := range jury {
		if reg.Lookup(j) == FamilyAnthropic {
			t.Fatalf("author family must be excluded; got %s", j)
		}
	}
}

func TestSelectJury_InsufficientFamilies(t *testing.T) {
	available := []string{
		"google/gemini-2.5-flash",
		"google/gemini-2.5-pro",
	}
	jury, err := SelectJury("anthropic", available, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(jury) != 2 {
		t.Fatalf("expected 2 jurors (only 2 available), got %d", len(jury))
	}
}

func TestEvaluateJury_QuorumNotMet(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		return VerdictPass, nil
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	_, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "google/gemini-2.5-pro"}, 3, review)
	if err == nil {
		t.Fatal("expected quorum error when only 2 of 3 reviewers available")
	}
}

func TestSelectJury_NoCrossFamilyAvailable(t *testing.T) {
	available := []string{"anthropic/claude-3-7-sonnet"}
	_, err := SelectJury("anthropic", available, 3)
	if err == nil {
		t.Fatal("expected error when no cross-family reviewers available")
	}
}

func TestSelectJury_DefaultSize(t *testing.T) {
	available := []string{
		"google/gemini-2.5-flash",
		"openai/gpt-4o",
		"grok/grok-4",
		"ollama/llama-3",
	}
	jury, err := SelectJury("anthropic", available, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(jury) != JurySize {
		t.Fatalf("expected default jury size %d, got %d", JurySize, len(jury))
	}
}

func TestEvaluateJury_UnanimousPass(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		return VerdictPass, nil
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	jv, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "openai/gpt-4o", "grok/grok-4"}, 3, review)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Verdict != VerdictPass {
		t.Fatalf("unanimous pass should give PASS, got %s", jv.Verdict)
	}
	if jv.Passes != 3 {
		t.Fatalf("expected 3 passes, got %d", jv.Passes)
	}
}

func TestEvaluateJury_UnanimousFail(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		return VerdictFail, nil
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	jv, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "openai/gpt-4o", "grok/grok-4"}, 3, review)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Verdict != VerdictFail {
		t.Fatalf("unanimous fail should give FAIL, got %s", jv.Verdict)
	}
	if jv.Fails != 3 {
		t.Fatalf("expected 3 fails, got %d", jv.Fails)
	}
}

func TestEvaluateJury_MajorityPass(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		if model == "grok/grok-4" {
			return VerdictFail, nil
		}
		return VerdictPass, nil
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	jv, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "openai/gpt-4o", "grok/grok-4"}, 3, review)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Verdict != VerdictPass {
		t.Fatalf("2 pass + 1 fail should give PASS (majority), got %s", jv.Verdict)
	}
	if jv.Passes != 2 || jv.Fails != 1 {
		t.Fatalf("expected 2 pass 1 fail, got pass=%d fail=%d", jv.Passes, jv.Fails)
	}
}

func TestEvaluateJury_MajorityFail(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		if model == "grok/grok-4" {
			return VerdictPass, nil
		}
		return VerdictFail, nil
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	jv, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "openai/gpt-4o", "grok/grok-4"}, 3, review)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Verdict != VerdictFail {
		t.Fatalf("2 fail + 1 pass should give FAIL (majority), got %s", jv.Verdict)
	}
}

func TestEvaluateJury_TieFailsClosed(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		if model == "grok/grok-4" {
			return VerdictStale, nil
		}
		if model == "google/gemini-2.5-flash" {
			return VerdictPass, nil
		}
		return VerdictFail, nil
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	jv, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "openai/gpt-4o", "grok/grok-4"}, 3, review)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Verdict != VerdictFail {
		t.Fatalf("1 pass + 1 fail + 1 stale should FAIL (no majority pass), got %s", jv.Verdict)
	}
}

func TestEvaluateJury_AllStaleFailsClosed(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		return VerdictStale, nil
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	jv, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "openai/gpt-4o", "grok/grok-4"}, 3, review)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Verdict != VerdictFail {
		t.Fatalf("all stale should FAIL (fail-closed), got %s", jv.Verdict)
	}
	if jv.Stales != 3 {
		t.Fatalf("expected 3 stales, got %d", jv.Stales)
	}
}

func TestEvaluateJury_ReviewerErrorFailsClosed(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		return "", fmt.Errorf("harness unavailable")
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	jv, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "openai/gpt-4o", "grok/grok-4"}, 3, review)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Verdict != VerdictFail {
		t.Fatalf("all reviewer errors should FAIL (fail-closed), got %s", jv.Verdict)
	}
	if jv.Fails != 3 {
		t.Fatalf("expected 3 fails (errors count as fail), got %d", jv.Fails)
	}
}

func TestEvaluateJury_PassWithErrorForcesFail(t *testing.T) {
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		return VerdictPass, fmt.Errorf("harness error after verdict")
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	jv, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash", "openai/gpt-4o", "grok/grok-4"}, 3, review)
	if err != nil {
		t.Fatal(err)
	}
	if jv.Verdict != VerdictFail {
		t.Fatalf("pass-with-error must force FAIL, got %s", jv.Verdict)
	}
	for _, v := range jv.Votes {
		if v.Verdict != VerdictFail {
			t.Fatalf("all votes must be FAIL when error present, got %s for %s", v.Verdict, v.Reviewer)
		}
	}
}

func TestEvaluateJury_NilReviewFunc(t *testing.T) {
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	_, err := EvaluateJury(context.Background(), pkt, "anthropic",
		[]string{"google/gemini-2.5-flash"}, 1, nil)
	if err == nil {
		t.Fatal("expected error for nil review function")
	}
}

func TestEvaluateJury_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	review := func(ctx context.Context, model string, p Packet) (ReviewVerdict, error) {
		return VerdictPass, nil
	}
	pkt := Packet{ID: "FAC-1", Tier: TierR3RiskCritical}
	_, err := EvaluateJury(ctx, pkt, "anthropic",
		[]string{"google/gemini-2.5-flash"}, 1, review)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestShouldUseJury(t *testing.T) {
	if !ShouldUseJury(TierR3RiskCritical) {
		t.Fatal("R3 should require jury")
	}
	if ShouldUseJury(TierR0RiskMechanical) {
		t.Fatal("R0 should not require jury")
	}
	if ShouldUseJury(TierR1RiskStandard) {
		t.Fatal("R1 should not require jury")
	}
	if ShouldUseJury(TierR2RiskHigh) {
		t.Fatal("R2 should not require jury")
	}
}

func TestMajorityVerdict_EdgeCases(t *testing.T) {
	if majorityVerdict(0, 0, 0) != VerdictFail {
		t.Fatal("zero total should fail closed")
	}
	if majorityVerdict(1, 0, 0) != VerdictPass {
		t.Fatal("1 pass 0 fail should pass")
	}
	if majorityVerdict(0, 1, 0) != VerdictFail {
		t.Fatal("0 pass 1 fail should fail")
	}
	if majorityVerdict(2, 1, 0) != VerdictPass {
		t.Fatal("2 pass 1 fail should pass (majority)")
	}
	if majorityVerdict(1, 1, 1) != VerdictFail {
		t.Fatal("1-1-1 tie should fail closed")
	}
	if majorityVerdict(2, 2, 0) != VerdictFail {
		t.Fatal("2-2 tie should fail closed")
	}
}
