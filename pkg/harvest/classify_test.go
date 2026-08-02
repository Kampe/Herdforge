package harvest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassifyNeedsReview(t *testing.T) {
	cases := []string{
		"Status: NEEDS_REVIEW",
		"NEEDS_REVIEW",
		"Task: X\nStatus: NEEDS_REVIEW\nSome detail",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got != ClassificationNeedsReview {
			t.Errorf("ClassifyText(%q) = %q, want %q", text, got, ClassificationNeedsReview)
		}
	}
}

func TestClassifyPass(t *testing.T) {
	cases := []string{
		"Verdict: PASS\nMerge recommendation: YES",
		"Merge recommendation: YES",
		"Verdict: PASS",
		"Before: unknown\nVerdict: PASS\nAfter: merged",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got != ClassificationPass {
			t.Errorf("ClassifyText(%q) = %q, want %q", text, got, ClassificationPass)
		}
	}
}

func TestClassifyFail(t *testing.T) {
	cases := []string{
		"Verdict: FAIL\nMerge recommendation: NO",
		"Verdict: FAIL",
		"Merge recommendation: NO",
		"Findings: bug\nVerdict: FAIL",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got != ClassificationFail {
			t.Errorf("ClassifyText(%q) = %q, want %q", text, got, ClassificationFail)
		}
	}
}

func TestClassifyComplete(t *testing.T) {
	cases := []string{
		"Status: COMPLETE",
		"Task done. COMPLETE",
		"Status: COMPLETE with warning",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got != ClassificationComplete {
			t.Errorf("ClassifyText(%q) = %q, want %q", text, got, ClassificationComplete)
		}
	}
}

func TestClassifyBlocked(t *testing.T) {
	cases := []string{
		"Status: BLOCKED",
		"BLOCKED: waiting for API key",
		"Status: BLOCKED by dependency",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got != ClassificationBlocked {
			t.Errorf("ClassifyText(%q) = %q, want %q", text, got, ClassificationBlocked)
		}
	}
}

func TestClassifyQuota(t *testing.T) {
	cases := []string{
		"Error: weekly quota exceeded",
		"too many requests, try again later",
		"429 too many requests",
		"usage limit reached; resets in 3h",
		"out of credits",
		"exceeded your quota",
		"monthly limit reached",
		"api quota exceeded for today",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got != ClassificationQuota {
			t.Errorf("ClassifyText(%q) = %q, want %q", text, got, ClassificationQuota)
		}
	}
}

func TestClassifyQuotaExcludesReviewContent(t *testing.T) {
	cases := []string{
		"CONFIRMED: the rate limit exceeded path returns 429; quota bucket exceeded branch is covered",
		"The endpoint enforces a rate limit of 100/s and returns 429 on quota bucket overflow",
		"Verdict: PASS\nNote: quota handling looks correct",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got == ClassificationQuota {
			t.Errorf("ClassifyText(%q) = %q, should NOT be QUOTA", text, got)
		}
	}
}

func TestClassifyUnconsumed(t *testing.T) {
	cases := []string{
		"❯ Some command\noutput here",
		"❯ ls -la\nline1\nline2",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got != ClassificationUnconsumed {
			t.Errorf("ClassifyText(%q) = %q, want %q", text, got, ClassificationUnconsumed)
		}
	}
}

func TestClassifyUnconsumedExcludesWorking(t *testing.T) {
	cases := []string{
		"❯\nWorked for 5s",
		"❯\nStatus: complete",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got == ClassificationUnconsumed {
			t.Errorf("ClassifyText(%q) = %q, should NOT be UNCONSUMED", text, got)
		}
	}
}

func TestClassifyUnknown(t *testing.T) {
	cases := []string{
		"",
		"just some random text",
		"debug output\nline 2\nline 3",
	}
	for _, text := range cases {
		if got := ClassifyText(text); got != ClassificationUnknown {
			t.Errorf("ClassifyText(%q) = %q, want %q", text, got, ClassificationUnknown)
		}
	}
}

func TestActionForClass(t *testing.T) {
	tests := []struct {
		class  Classification
		action string
	}{
		{ClassificationNeedsReview, "dispatch_review_or_merge_gate"},
		{ClassificationPass, "merge_if_tier_ok"},
		{ClassificationFail, "return_to_builder"},
		{ClassificationComplete, "close_or_activate"},
		{ClassificationBlocked, "unblock_or_reassign"},
		{ClassificationQuota, "mark_unavailable_and_reroute"},
		{ClassificationUnconsumed, "read_pane"},
		{ClassificationUnknown, "read_pane"},
	}
	for _, tt := range tests {
		if got := ActionForClass(tt.class); got != tt.action {
			t.Errorf("ActionForClass(%q) = %q, want %q", tt.class, got, tt.action)
		}
	}
}

func TestTail(t *testing.T) {
	text := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10"
	tail := Tail(text, 100)
	if tail == "" {
		t.Fatal("expected non-empty tail")
	}
	if len(tail) > 100 {
		t.Fatalf("tail length %d exceeds max 100", len(tail))
	}
}

func TestProcessingItemJSON(t *testing.T) {
	item := ProcessingItem{
		PaneID: "pane-1",
		Name:   "agent-1",
		Status: "running",
		Class:  ClassificationNeedsReview,
		Action: "dispatch_review_or_merge_gate",
		Tail:   "Status: NEEDS_REVIEW",
	}
	out, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "pane_id") {
		t.Errorf("expected pane_id in JSON, got %s", out)
	}
}
