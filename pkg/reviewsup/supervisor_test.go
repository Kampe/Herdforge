package reviewsup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time {
	return time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
}

func newTestSupervisor(t *testing.T) (*ReviewSupervisor, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	cfg.MaxPendingReviews = 3
	cfg.StaleDuration = 24 * time.Hour
	cfg.RetryLimit = 3
	return New(cfg), dir
}

func svc(t *testing.T) *ReviewSupervisor {
	t.Helper()
	sv, _ := newTestSupervisor(t)
	_, _, err := sv.Ingest(CompletionCallback{
		SHA:         "aaa111",
		Branch:      "feat/foo",
		PatchID:     "patch-1",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR1,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	_, _, err = sv.Ingest(CompletionCallback{
		SHA:         "bbb222",
		Branch:      "feat/bar",
		PatchID:     "patch-2",
		AuthorModel: "gemini-2.5-flash",
		Tier:        TierR3,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return sv
}

func TestNewDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	if cfg.MaxPendingReviews != 3 {
		t.Errorf("MaxPendingReviews = %d, want 3", cfg.MaxPendingReviews)
	}
	if cfg.RetryLimit != 3 {
		t.Errorf("RetryLimit = %d, want 3", cfg.RetryLimit)
	}
	if cfg.StaleDuration != 24*time.Hour {
		t.Errorf("StaleDuration = %v, want 24h", cfg.StaleDuration)
	}
	if !strings.HasSuffix(cfg.LedgerPath, "supervisor-ledger.jsonl") {
		t.Errorf("LedgerPath = %s", cfg.LedgerPath)
	}
}

func TestIngest(t *testing.T) {
	sv := svc(t)
	if sv.PendingCount() != 2 {
		t.Errorf("PendingCount = %d, want 2", sv.PendingCount())
	}
	c := sv.Candidate("aaa111")
	if c == nil {
		t.Fatal("candidate aaa111 not found")
	}
	if c.State != StatePending {
		t.Errorf("state = %s, want pending", c.State)
	}
	if c.AuthorFamily != "anthropic" {
		t.Errorf("AuthorFamily = %s, want anthropic", c.AuthorFamily)
	}

	c2 := sv.Candidate("bbb222")
	if c2 == nil {
		t.Fatal("candidate bbb222 not found")
	}
	if c2.AuthorFamily != "google" {
		t.Errorf("AuthorFamily = %s, want google", c2.AuthorFamily)
	}
	if c2.Tier != TierR3 {
		t.Errorf("Tier = %s, want R3", c2.Tier)
	}
}

func TestIngestDuplicateSHA(t *testing.T) {
	sv := svc(t)
	accepted, stale, err := sv.Ingest(CompletionCallback{
		SHA:         "aaa111",
		Branch:      "feat/foo",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR1,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if accepted {
		t.Error("expected accepted=false for duplicate SHA")
	}
	if stale != "" {
		t.Errorf("expected empty stale, got %s", stale)
	}
}

func TestIngestSupersedesOld(t *testing.T) {
	sv := svc(t)

	// Same patchID, newer SHA.
	accepted, stale, err := sv.Ingest(CompletionCallback{
		SHA:         "aaa222",
		Branch:      "feat/foo",
		PatchID:     "patch-1",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR1,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !accepted {
		t.Error("expected accepted=true for new SHA")
	}
	if stale != "aaa111" {
		t.Errorf("stale SHA = %s, want aaa111", stale)
	}

	// Old candidate should be evicted.
	c := sv.Candidate("aaa111")
	if c == nil {
		t.Fatal("old candidate should still exist")
	}
	if c.State != StateEvicted {
		t.Errorf("old candidate state = %s, want evicted", c.State)
	}

	// New candidate should be pending.
	c2 := sv.Candidate("aaa222")
	if c2 == nil {
		t.Fatal("new candidate not found")
	}
	if c2.State != StatePending {
		t.Errorf("new candidate state = %s, want pending", c2.State)
	}
}

func TestIngestEmptySHA(t *testing.T) {
	sv := svc(t)
	_, _, err := sv.Ingest(CompletionCallback{
		SHA:         "",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR1,
	})
	if err == nil {
		t.Fatal("expected error for empty SHA")
	}
}

func TestSelectReviewerCrossFamily(t *testing.T) {
	sv := svc(t)
	// aaa111 is anthropic (claude) at R1 — needs cross-family.
	rev, err := sv.SelectReviewer("aaa111", []ReviewerEntry{
		{Name: "gemini-2.5-flash", Model: "gemini-2.5-flash"},
		{Name: "claude-3-7-sonnet", Model: "claude-3-7-sonnet"},
	})
	if err != nil {
		t.Fatalf("SelectReviewer: %v", err)
	}
	if rev == nil {
		t.Fatal("expected a reviewer")
	}
	if rev.Name != "gemini-2.5-flash" {
		t.Errorf("expected gemini-2.5-flash, got %s", rev.Name)
	}
}

func TestSelectReviewerSameFamilyR1Rejected(t *testing.T) {
	sv := svc(t)
	// aaa111 is anthropic at R1 — same family must be rejected.
	rev, err := sv.SelectReviewer("aaa111", []ReviewerEntry{
		{Name: "claude-3-7-sonnet", Model: "claude-3-7-sonnet"},
	})
	if err != nil {
		t.Fatalf("SelectReviewer: %v", err)
	}
	if rev != nil {
		t.Errorf("expected nil (no cross-family reviewer), got %s", rev.Name)
	}
}

func TestSelectReviewerR0SameFamilyOK(t *testing.T) {
	s, _ := newTestSupervisor(t)
	_, _, err := s.Ingest(CompletionCallback{
		SHA:         "r0sha",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR0,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	rev, err := s.SelectReviewer("r0sha", []ReviewerEntry{
		{Name: "claude-3-5-haiku", Model: "claude-3-5-haiku"},
	})
	if err != nil {
		t.Fatalf("SelectReviewer: %v", err)
	}
	if rev == nil {
		t.Fatal("expected reviewer for R0 even with same family")
	}
	if rev.Name != "claude-3-5-haiku" {
		t.Errorf("expected claude-3-5-haiku, got %s", rev.Name)
	}
}

func TestSelectReviewerUnknownCandidate(t *testing.T) {
	sv := svc(t)
	_, err := sv.SelectReviewer("nonexistent", []ReviewerEntry{
		{Name: "gemini", Model: "gemini"},
	})
	if err == nil {
		t.Fatal("expected error for unknown candidate")
	}
}

func TestSelectReviewerBackpressure(t *testing.T) {
	sv := svc(t)
	// No reviewers available at all.
	rev, err := sv.SelectReviewer("aaa111", nil)
	if err != nil {
		t.Fatalf("SelectReviewer: %v", err)
	}
	if rev != nil {
		t.Errorf("expected nil (backpressure), got %s", rev.Name)
	}
}

func TestLaunchReview(t *testing.T) {
	sv := svc(t)
	err := sv.LaunchReview("aaa111", "gemini-2.5-flash", "gemini-2.5-flash")
	if err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}
	c := sv.Candidate("aaa111")
	if c.State != StateReviewing {
		t.Errorf("state = %s, want reviewing", c.State)
	}
	if c.Reviewer != "gemini-2.5-flash" {
		t.Errorf("Reviewer = %s", c.Reviewer)
	}
	if c.ReviewFamily != "google" {
		t.Errorf("ReviewFamily = %s, want google", c.ReviewFamily)
	}
	if c.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", c.Attempts)
	}
}

func TestLaunchReviewNotPending(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini")
	// Second launch should fail.
	err := sv.LaunchReview("aaa111", "gemini-2", "gemini-2")
	if err == nil {
		t.Fatal("expected error for non-pending candidate")
	}
}

func TestSubmitVerdictPASS(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")

	_, err := sv.SubmitVerdict(ReviewVerdict{
		SHA:      "aaa111",
		Reviewer: "gemini",
		Verdict:  VerdictPASS,
	})
	if err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}

	c := sv.Candidate("aaa111")
	if c.State != StateHarvested {
		t.Errorf("state = %s, want harvested", c.State)
	}

	// Should be in harvest queue.
	ready, err := sv.ReadyForHarvest(10)
	if err != nil {
		t.Fatalf("ReadyForHarvest: %v", err)
	}
	found := false
	for _, h := range ready {
		if h.SHA == "aaa111" {
			found = true
			break
		}
	}
	if !found {
		t.Error("aaa111 not found in harvest queue after PASS")
	}
}

func TestSubmitVerdictFAILReturnsToPending(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")

	_, err := sv.SubmitVerdict(ReviewVerdict{
		SHA:      "aaa111",
		Reviewer: "gemini",
		Verdict:  VerdictFAIL,
		Reason:   "missing error handling",
	})
	if err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}

	c := sv.Candidate("aaa111")
	if c.State != StatePending {
		t.Errorf("state after FAIL = %s, want pending (for repair)", c.State)
	}
	if c.VerdictReason != "missing error handling" {
		t.Errorf("reason = %s", c.VerdictReason)
	}

	// Should NOT be in harvest queue.
	ready, _ := sv.ReadyForHarvest(10)
	for _, h := range ready {
		if h.SHA == "aaa111" {
			t.Error("aaa111 should not be in harvest queue after FAIL")
		}
	}
}

func TestSubmitVerdictFAILExceedsRetryLimit(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Now = fixedNow
	cfg.RetryLimit = 1
	sv2 := New(cfg)
	sv2.Ingest(CompletionCallback{
		SHA:         "aaa111",
		AuthorModel: "claude",
		Tier:        TierR1,
	})
	sv2.LaunchReview("aaa111", "gemini", "gemini")
	sv2.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictFAIL})

	c := sv2.Candidate("aaa111")
	if c.State != StateBlocked {
		t.Errorf("state = %s, want blocked (retry limit hit)", c.State)
	}
	if c.Verdict != VerdictBLOCKED {
		t.Errorf("verdict = %s, want BLOCKED", c.Verdict)
	}
}

func TestSubmitVerdictBLOCKED(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictBLOCKED, Reason: "can't reproduce"})

	c := sv.Candidate("aaa111")
	if c.State != StateBlocked {
		t.Errorf("state = %s, want blocked", c.State)
	}
}

func TestCapacityTracking(t *testing.T) {
	sv := svc(t) // 2 pending

	if sv.AtCapacity() {
		t.Error("expected not at capacity with 2/3 pending")
	}
	if sv.AvailableCapacity() != 1 {
		t.Errorf("AvailableCapacity = %d, want 1", sv.AvailableCapacity())
	}

	// Add third.
	sv.Ingest(CompletionCallback{SHA: "ccc333", AuthorModel: "gpt-4", Tier: TierR2})
	if !sv.AtCapacity() {
		t.Error("expected at capacity with 3/3")
	}
	if sv.AvailableCapacity() != 0 {
		t.Errorf("AvailableCapacity = %d, want 0", sv.AvailableCapacity())
	}
}

func TestVerdictReleasesCapacity(t *testing.T) {
	sv := svc(t)
	sv.Ingest(CompletionCallback{SHA: "ccc333", AuthorModel: "gpt-4", Tier: TierR2})
	if !sv.AtCapacity() {
		t.Fatal("expected at capacity")
	}

	// Resolve one.
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})

	if sv.PendingCount() != 2 {
		t.Errorf("PendingCount = %d, want 2", sv.PendingCount())
	}
	if sv.AtCapacity() {
		t.Error("expected NOT at capacity after resolving one")
	}
}

func TestMarkHarvested(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})

	ready, _ := sv.ReadyForHarvest(10)
	if len(ready) != 1 {
		t.Fatalf("expected 1 ready, got %d", len(ready))
	}

	sv.MarkHarvested("aaa111")

	ready2, _ := sv.ReadyForHarvest(10)
	if len(ready2) != 0 {
		t.Errorf("expected 0 after mark harvested, got %d", len(ready2))
	}
}

func TestEvictStale(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = func() time.Time {
		// Past + 48h = stale.
		return time.Date(2025, 6, 3, 12, 0, 0, 0, time.UTC)
	}
	cfg.StaleDuration = 24 * time.Hour

	// Use a supervisor with an early fixed time for ingest.
	cfg2 := DefaultConfig(dir)
	cfg2.Now = func() time.Time {
		return time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	}
	cfg2.StaleDuration = 24 * time.Hour
	sv2 := New(cfg2)
	sv2.Ingest(CompletionCallback{SHA: "oldsha", AuthorModel: "claude", Tier: TierR1})

	// Now use late-time supervisor with same ledger path to test eviction.
	cfg3 := DefaultConfig(dir)
	cfg3.Now = func() time.Time {
		return time.Date(2025, 6, 3, 12, 0, 0, 0, time.UTC)
	}
	cfg3.StaleDuration = 24 * time.Hour
	sv3 := New(cfg3)
	// Reconstruct first.
	sv3.Reconstruct()

	n, err := sv3.EvictStale()
	if err != nil {
		t.Fatalf("EvictStale: %v", err)
	}
	if n != 1 {
		t.Errorf("evicted %d, want 1", n)
	}
}

func TestReconstructFromLedger(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	sv := New(cfg)

	// Create some state.
	sv.Ingest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1, PatchID: "p1"})
	sv.Ingest(CompletionCallback{SHA: "bbb222", AuthorModel: "gemini", Tier: TierR3, PatchID: "p2"})
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})

	// Create new supervisor from same ledger.
	sv2 := New(cfg)
	n, err := sv2.Reconstruct()
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if n != 2 {
		t.Errorf("reconstructed %d candidates, want 2", n)
	}

	c := sv2.Candidate("aaa111")
	if c == nil {
		t.Fatal("aaa111 not found after reconstruct")
	}
	if c.State != StatePass {
		t.Errorf("state = %s, want pass (queue harvested flag not set until MarkHarvested)", c.State)
	}
	if c.ReviewFamily != "google" {
		t.Errorf("ReviewFamily = %s, want google", c.ReviewFamily)
	}

	c2 := sv2.Candidate("bbb222")
	if c2 == nil {
		t.Fatal("bbb222 not found after reconstruct")
	}
	if c2.State != StatePending {
		t.Errorf("bbb222 state = %s, want pending", c2.State)
	}
}

func TestReconstructSupersede(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	sv := New(cfg)

	sv.Ingest(CompletionCallback{SHA: "old", PatchID: "p1", AuthorModel: "claude", Tier: TierR1})
	sv.Ingest(CompletionCallback{SHA: "new", PatchID: "p1", AuthorModel: "claude", Tier: TierR1})

	sv2 := New(cfg)
	sv2.Reconstruct()

	old := sv2.Candidate("old")
	if old == nil {
		t.Fatal("old candidate should exist")
	}
	if old.State != StateEvicted {
		t.Errorf("old state = %s, want evicted", old.State)
	}

	ne := sv2.Candidate("new")
	if ne == nil {
		t.Fatal("new candidate should exist")
	}
	if ne.State != StatePending {
		t.Errorf("new state = %s, want pending", ne.State)
	}
}

func TestReconstructWithQueueState(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	sv := New(cfg)

	sv.Ingest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1})
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})
	sv.MarkHarvested("aaa111")

	sv2 := New(cfg)
	sv2.Reconstruct()

	c := sv2.Candidate("aaa111")
	if c == nil {
		t.Fatal("aaa111 not found")
	}
	if c.State != StateHarvested {
		t.Errorf("state = %s, want harvested (queue persisted)", c.State)
	}
}

func TestReadyForHarvestReturnsSorted(t *testing.T) {
	sv := svc(t)
	sv.Ingest(CompletionCallback{SHA: "ccc333", AuthorModel: "gpt-4", Tier: TierR2})
	sv.Ingest(CompletionCallback{SHA: "ddd444", AuthorModel: "codex", Tier: TierR1})

	// Launch and pass all.
	for _, sha := range []string{"aaa111", "bbb222", "ccc333", "ddd444"} {
		rev := "gemini"
		mod := "gemini"
		if sv.Candidate(sha).AuthorFamily == "google" {
			rev = "claude"
			mod = "claude-3"
		}
		sv.LaunchReview(sha, rev, mod)
		sv.SubmitVerdict(ReviewVerdict{SHA: sha, Reviewer: rev, Verdict: VerdictPASS})
	}

	ready, _ := sv.ReadyForHarvest(10)
	if len(ready) != 4 {
		t.Fatalf("expected 4 ready, got %d", len(ready))
	}
	// Should be sorted by SHA.
	if ready[0].SHA != "aaa111" || ready[3].SHA != "ddd444" {
		t.Errorf("unexpected order: %v", ready)
	}
}

func TestReadyForHarvestLimit(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})
	sv.LaunchReview("bbb222", "claude", "claude")
	sv.SubmitVerdict(ReviewVerdict{SHA: "bbb222", Reviewer: "claude", Verdict: VerdictPASS})

	ready, _ := sv.ReadyForHarvest(1)
	if len(ready) != 1 {
		t.Errorf("expected 1 (limited), got %d", len(ready))
	}
}

func TestR3RequiresCrossFamily(t *testing.T) {
	if !RequireCrossFamily(TierR3) {
		t.Error("R3 should require cross-family")
	}
	if !RequireCrossFamily(TierR2) {
		t.Error("R2 should require cross-family")
	}
	if !RequireCrossFamily(TierR1) {
		t.Error("R1 should require cross-family")
	}
	if RequireCrossFamily(TierR0) {
		t.Error("R0 should NOT require cross-family")
	}
}

func TestLookupFamily(t *testing.T) {
	tests := []struct {
		model string
		want  ModelFamily
	}{
		{"claude-3-7-sonnet", FamilyAnt},
		{"gemini-2.5-flash", FamilyGoogle},
		{"gpt-4o", FamilyOpenAI},
		{"grok-3", FamilyGrok},
		{"deepseek-chat", FamilyLazer},
		{"lazer/deepseek-v4", FamilyLazer},
		{"kimi-k2", FamilyKimi},
		{"codex", FamilyCodex},
		{"unknown-model", FamilyOther},
	}
	for _, tt := range tests {
		got := lookupFamily(tt.model)
		if got != tt.want {
			t.Errorf("lookupFamily(%q) = %s, want %s", tt.model, got, tt.want)
		}
	}
}

func TestCrossFamilyOK(t *testing.T) {
	if !CrossFamilyOK(FamilyAnt, FamilyGoogle) {
		t.Error("anthropic/google should be cross-family")
	}
	if CrossFamilyOK(FamilyAnt, FamilyAnt) {
		t.Error("same family should NOT be cross-family")
	}
}

func TestStatus(t *testing.T) {
	sv := svc(t)
	st := sv.Status()
	if st.CandidateCount != 2 {
		t.Errorf("CandidateCount = %d", st.CandidateCount)
	}
	if st.PendingCount != 2 {
		t.Errorf("PendingCount = %d", st.PendingCount)
	}
	if st.AtCapacity {
		t.Error("expected not at capacity")
	}
}

func TestCandidateUnknown(t *testing.T) {
	sv := svc(t)
	c := sv.Candidate("nonexistent")
	if c != nil {
		t.Error("expected nil for unknown candidate")
	}
}

func TestSubmitVerdictUnknownCandidate(t *testing.T) {
	sv := svc(t)
	_, err := sv.SubmitVerdict(ReviewVerdict{
		SHA:      "unknown",
		Verdict:  VerdictPASS,
	})
	if err == nil {
		t.Fatal("expected error for unknown candidate")
	}
}

func TestSubmitVerdictFAILReturnsFindingsToBuilder(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("bbb222", "claude", "claude-3-7-sonnet")

	_, err := sv.SubmitVerdict(ReviewVerdict{
		SHA:      "bbb222",
		Reviewer: "claude",
		Verdict:  VerdictFAIL,
		Reason:   "handle overflow in calc()",
	})
	if err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}

	c := sv.Candidate("bbb222")
	if c.State != StatePending {
		t.Errorf("state = %s, want pending for builder repair", c.State)
	}
	if c.VerdictReason != "handle overflow in calc()" {
		t.Errorf("reason = %s", c.VerdictReason)
	}

	// Verify it's available for re-review (new reviewer selection).
	rev, err := sv.SelectReviewer("bbb222", []ReviewerEntry{
		{Name: "claude-3-5-haiku", Model: "claude-3-5-haiku"},
	})
	if err != nil {
		t.Fatalf("SelectReviewer after FAIL: %v", err)
	}
	if rev != nil {
		sv.LaunchReview("bbb222", rev.Name, rev.Model)
		c2 := sv.Candidate("bbb222")
		if c2.State != StateReviewing {
			t.Errorf("state after re-launch = %s, want reviewing", c2.State)
		}
	}
}

func TestDuplicateVerdictNoDoubleHarvest(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})

	ready, _ := sv.ReadyForHarvest(10)
	if len(ready) != 1 {
		t.Fatalf("expected 1 ready, got %d", len(ready))
	}

	// Submit same verdict again.
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})
	ready2, _ := sv.ReadyForHarvest(10)
	if len(ready2) != 1 {
		t.Errorf("expected still 1 ready (no duplicate), got %d", len(ready2))
	}
}

func TestLostCallbackRecoveryViaReconstruct(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = func() time.Time {
		return time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	}
	sv := New(cfg)

	// Simulate: ingest + launch review, but verdict lost (supervisor crash).
	sv.Ingest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1, PatchID: "p1"})
	sv.LaunchReview("aaa111", "gemini", "gemini")
	// No verdict submitted — simulate crash.

	// Reconstruct — should show candidate in reviewing state.
	sv2 := New(cfg)
	n, err := sv2.Reconstruct()
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconstructed %d, want 1", n)
	}
	c := sv2.Candidate("aaa111")
	if c.State != StateReviewing {
		t.Errorf("state after reconstruct = %s, want reviewing (lost callback preserved)", c.State)
	}
}

func TestZeroValueSafety(t *testing.T) {
	sv := svc(t)

	// Launch review for empty SHA should fail.
	err := sv.LaunchReview("", "gemini", "gemini")
	if err == nil {
		t.Error("expected error for empty SHA")
	}

	// Submit empty verdict should fail.
	_, err = sv.SubmitVerdict(ReviewVerdict{SHA: "", Verdict: VerdictPASS})
	if err == nil {
		t.Error("expected error for empty verdict SHA")
	}

	// Ingest with empty SHA should fail.
	_, _, err = sv.Ingest(CompletionCallback{SHA: "", AuthorModel: "claude", Tier: TierR1})
	if err == nil {
		t.Error("expected error for empty ingest SHA")
	}
}

func TestAvailableCapacityFloor(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.MaxPendingReviews = 1
	sv := New(cfg)
	if sv.AvailableCapacity() != 1 {
		t.Errorf("expected 1, got %d", sv.AvailableCapacity())
	}
	sv.Ingest(CompletionCallback{SHA: "s1", AuthorModel: "claude", Tier: TierR1})
	if sv.AvailableCapacity() != 0 {
		t.Errorf("expected 0, got %d", sv.AvailableCapacity())
	}
	sv.Ingest(CompletionCallback{SHA: "s2", AuthorModel: "gemini", Tier: TierR1})
	if sv.AvailableCapacity() != 0 {
		t.Errorf("expected 0 (clamped), got %d", sv.AvailableCapacity())
	}
}

func TestLedgerFileCreation(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	sv := New(cfg)

	sv.Ingest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1})
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})

	// Ledger file should exist and have rows.
	rows, err := readRows(cfg.LedgerPath)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) == 0 {
		t.Error("expected ledger rows")
	}

	qrows, err := readRows(cfg.QueuePath)
	if err != nil {
		t.Fatalf("readRows queue: %v", err)
	}
	if len(qrows) == 0 {
		t.Error("expected queue rows")
	}
}

func TestMarkHarvestedUpdatesState(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictPASS})

	sv.MarkHarvested("aaa111")
	c := sv.Candidate("aaa111")
	if c.State != StateHarvested {
		t.Errorf("state = %s, want harvested", c.State)
	}
}

func TestSelectReviewerAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	sv := New(cfg)

	sv.Ingest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1})
	// Crash before launching review.

	sv2 := New(cfg)
	sv2.Reconstruct()

	// Can still select and launch reviewer.
	rev, err := sv2.SelectReviewer("aaa111", []ReviewerEntry{
		{Name: "gemini", Model: "gemini-2.5-flash"},
	})
	if err != nil {
		t.Fatalf("SelectReviewer after reconstruct: %v", err)
	}
	if rev == nil {
		t.Fatal("expected reviewer after reconstruct")
	}
}

func TestReadRowsMissingFile(t *testing.T) {
	rows, err := readRows("/nonexistent/path.jsonl")
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

func TestReadRowsMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.jsonl")
	os.WriteFile(p, []byte("{badjson}\n{\"event\":\"completion\",\"sha\":\"abc\"}\n"), 0644)

	rows, err := readRows(p)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 valid row, got %d", len(rows))
	}
}
