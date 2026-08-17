package finish

import "testing"

func validEvidence() Evidence {
	return Evidence{Ref: "FAC-306", LandedSHA: "0123456789abcdef0123456789abcdef01234567", ReceiptRef: "FAC-306", ReceiptValid: true, ReceiptCandidateSHA: "abcdef0123456789abcdef0123456789abcdef01", ReceiptMergeSHA: "0123456789abcdef0123456789abcdef01234567", ReceiptVerdict: "PASS", AuthorFamily: "anthropic", ReviewerFamily: "openai", ReceiptIntegration: "merged", ReviewPass: true, ChecksPass: true, LandedOnMain: true, Clean: true, BranchRemoved: true, WorktreeRemoved: true}
}

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Evidence)
		want   string
	}{
		{"all proofs pass", func(*Evidence) {}, ""},
		{"dirty tree", func(e *Evidence) { e.Clean = false }, "dirty"},
		{"unique work", func(e *Evidence) { e.UniqueWork = true }, "unique"},
		{"wrong landed sha", func(e *Evidence) { e.ReceiptMergeSHA = "abcdef0123456789abcdef0123456789abcdef01" }, "merge SHA"},
		{"missing receipt", func(e *Evidence) { e.ReceiptValid = false }, "receipt"},
		{"stale review", func(e *Evidence) { e.ReviewPass = false }, "review PASS"},
		{"checks failed", func(e *Evidence) { e.ChecksPass = false }, "checks"},
		{"branch remains", func(e *Evidence) { e.BranchRemoved = false }, "branch"},
		{"worktree remains", func(e *Evidence) { e.WorktreeRemoved = false }, "worktree"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvidence()
			tc.mutate(&e)
			r := Evaluate(e)
			if tc.want == "" {
				if !r.Ready {
					t.Fatalf("unexpected refusal: %v", r.Reasons)
				}
				return
			}
			if r.Ready {
				t.Fatal("invalid evidence was accepted")
			}
			found := false
			for _, reason := range r.Reasons {
				if contains(reason, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("reasons=%v, want %q", r.Reasons, tc.want)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
