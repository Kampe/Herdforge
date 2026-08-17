package reviewsup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time {
	return time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
}

// testReceiptDigest is a syntactically valid FAC-122 digest used by unit
// fixtures that are not exercising the verifier itself.
const testReceiptDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testAdmitReceipt(_ context.Context, _, digest string) error {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len(testReceiptDigest) {
		return errors.New("test admit: invalid digest")
	}
	return nil
}

func newTestSupervisor(t *testing.T) (*ReviewSupervisor, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	cfg.MaxPendingReviews = 3
	cfg.StaleDuration = 24 * time.Hour
	cfg.RetryLimit = 3
	cfg.AdmitReceipt = testAdmitReceipt
	return New(cfg), dir
}

func withDigest(cb CompletionCallback) CompletionCallback {
	if cb.ReceiptDigest == "" {
		cb.ReceiptDigest = testReceiptDigest
	}
	return cb
}

func svc(t *testing.T) *ReviewSupervisor {
	t.Helper()
	sv, _ := newTestSupervisor(t)
	_, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "aaa111",
		Branch:      "feat/foo",
		PatchID:     "patch-1",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR1,
	}))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	_, _, err = sv.Ingest(withDigest(CompletionCallback{
		SHA:         "bbb222",
		Branch:      "feat/bar",
		PatchID:     "patch-2",
		AuthorModel: "gemini-2.5-flash",
		Tier:        TierR3,
	}))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return sv
}

// ephemeralPASSArtifact writes a chainseer-style temp-path verdict body. FAC-373
// requires the supervisor to retain it under .herd/review/inbox before any
// cleanup-candidate transition; the temp file may vanish after pane exit.
func ephemeralPASSArtifact(t *testing.T, sha string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "chainseer-herd-review")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "verdict-"+sha+".md")
	body := "sha: " + sha + "\nverdict: PASS\n---\n" + strings.Repeat("independent review evidence ", 20)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func passVerdict(sha, reviewer string, t *testing.T) ReviewVerdict {
	t.Helper()
	return ReviewVerdict{
		SHA:      sha,
		Reviewer: reviewer,
		Verdict:  VerdictPASS,
		Artifact: ephemeralPASSArtifact(t, sha),
	}
}

func TestNewDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.AdmitReceipt = testAdmitReceipt
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

func TestWatchdogReportsStaleReviewWithoutLiveReviewer(t *testing.T) {
	now := fixedNow()
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = func() time.Time { return now }
	cfg.AdmitReceipt = testAdmitReceipt
	sv := New(cfg)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA: "watchdog-sha", Branch: "feat/watchdog", PatchID: "watchdog",
		AuthorModel: "claude-3-7-sonnet", Tier: TierR1,
	})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := sv.LaunchReview("watchdog-sha", "reviewer-1", "gemini-2.5-flash"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}
	now = now.Add(11 * time.Minute)
	alerts := sv.Watchdog(10*time.Minute, func(string) bool { return false })
	if len(alerts) != 1 || alerts[0].SHA != "watchdog-sha" || alerts[0].Reason != "reviewer not live" {
		t.Fatalf("alerts = %+v, want one dead-reviewer alert", alerts)
	}
	if got := sv.Watchdog(10*time.Minute, func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("live reviewer still alerted: %+v", got)
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
	accepted, stale, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "aaa111",
		Branch:      "feat/foo",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR1,
	}))
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

	accepted, stale, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "aaa222",
		Branch:      "feat/foo",
		PatchID:     "patch-1",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR1,
	}))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if !accepted {
		t.Error("expected accepted=true for new SHA")
	}
	if stale != "aaa111" {
		t.Errorf("stale SHA = %s, want aaa111", stale)
	}

	c := sv.Candidate("aaa111")
	if c == nil {
		t.Fatal("old candidate should still exist")
	}
	if c.State != StateEvicted {
		t.Errorf("old candidate state = %s, want evicted", c.State)
	}

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
	_, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR1,
	}))
	if err == nil {
		t.Fatal("expected error for empty SHA")
	}
}

func TestSelectReviewerCrossFamily(t *testing.T) {
	sv := svc(t)
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

func TestSelectReviewerFallsBackFromRefusedPreferredRoute(t *testing.T) {
	sv := svc(t)
	rev, err := sv.SelectReviewer("aaa111", []ReviewerEntry{
		{Name: "anthropic-preferred", Model: "claude-sonnet-5", Provider: "claude", Preferred: true, Refused: true, RefusalReason: "risk tier refused before pane creation"},
		{Name: "grok-fallback", Model: "grok-3", Provider: "grok"},
	})
	if err != nil {
		t.Fatalf("SelectReviewer: %v", err)
	}
	if rev == nil || rev.Name != "grok-fallback" || rev.Harness != "grok" {
		t.Fatalf("fallback = %+v, want deterministic grok vendor route", rev)
	}
	if got := sv.Candidate("aaa111").State; got != StatePending {
		t.Fatalf("fallback selection changed state to %s", got)
	}
}

func TestSelectReviewerPersistsBlockedWhenNoRouteIsAdmissible(t *testing.T) {
	sv := svc(t)
	rev, err := sv.SelectReviewer("aaa111", []ReviewerEntry{
		{Name: "anthropic-preferred", Model: "claude-sonnet-5", Provider: "claude", Preferred: true, Refused: true, RefusalReason: "risk tier refused"},
		{Name: "unknown-fallback", Model: "mystery-model", Provider: ""},
	})
	if err != nil || rev != nil {
		t.Fatalf("selection = %+v, err=%v; want durable blocked outcome", rev, err)
	}
	c := sv.Candidate("aaa111")
	if c.State != StateBlocked || c.Verdict != VerdictBLOCKED || c.BlockedReason == "" {
		t.Fatalf("candidate = %+v, want visible blocked route", c)
	}
	if _, err := sv.SelectReviewer("aaa111", []ReviewerEntry{{Name: "retry", Model: "grok-3", Provider: "grok"}}); err == nil {
		t.Fatal("blocked candidate was eligible for a retry")
	}
	restarted := New(sv.cfg)
	if _, err := restarted.Reconstruct(); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if got := restarted.Candidate("aaa111"); got == nil || got.State != StateBlocked || got.BlockedReason == "" {
		t.Fatalf("reconstructed candidate = %+v, want durable blocked state", got)
	}
}

func TestSelectReviewerSameFamilyR1Rejected(t *testing.T) {
	sv := svc(t)
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
	_, _, err := s.Ingest(withDigest(CompletionCallback{
		SHA:         "r0sha",
		AuthorModel: "claude-3-7-sonnet",
		Tier:        TierR0,
	}))
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

func TestLaunchReviewSameFamilyRejected(t *testing.T) {
	sv := svc(t)
	// aaa111 is anthropic/R1 — same family must be rejected.
	err := sv.LaunchReview("aaa111", "claude-3-7-sonnet", "claude-3-7-sonnet")
	if err == nil {
		t.Fatal("expected error for same-family review on R1")
	}
}

func TestLaunchReviewNotPending(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	err := sv.LaunchReview("aaa111", "gemini-2", "gemini-2")
	if err == nil {
		t.Fatal("expected error for non-pending candidate")
	}
}

// TestSelectReviewerUnknownFamilyRejected_RepairProbe: an unrecognized model
// family must never satisfy the cross-family requirement for R1-R3 — it must
// not be treated as automatically cross-family from every known family.
func TestSelectReviewerUnknownFamilyRejected_RepairProbe(t *testing.T) {
	sv := svc(t)
	rev, err := sv.SelectReviewer("aaa111", []ReviewerEntry{
		{Name: "mystery-bot", Model: "totally-unrecognized-model-xyz"},
	})
	if err != nil {
		t.Fatalf("SelectReviewer: %v", err)
	}
	if rev != nil {
		t.Errorf("expected nil (unknown family must not satisfy cross-family), got %s", rev.Name)
	}
}

func TestLaunchReviewUnknownFamilyRejected_RepairProbe(t *testing.T) {
	sv := svc(t)
	err := sv.LaunchReview("aaa111", "mystery-bot", "totally-unrecognized-model-xyz")
	if err == nil {
		t.Fatal("expected error: unknown-family reviewer must not launch on R1-R3 candidate")
	}
}

// TestSubmitVerdictUsesStoredReviewFamily_RepairProbe guards against
// re-deriving the review family from the reviewer's free-text label at
// verdict time. A reviewer label can accidentally contain another family's
// keyword (e.g. a label mentioning the author's family) even though the
// actual review model — recorded in cand.ReviewFamily at LaunchReview time —
// is genuinely cross-family. SubmitVerdict must trust the stored family, not
// re-guess it from the label, or a valid cross-family review gets rejected
// (or worse, a same-family one could slip through under a misleading label).
func TestSubmitVerdictUsesStoredReviewFamily_RepairProbe(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	_, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "sha1",
		AuthorModel: "gpt-4", // openai family
		Tier:        TierR1,
	}))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Real review model is anthropic (genuinely cross-family vs openai), but
	// the reviewer's label deliberately contains "gpt" — the author's family
	// keyword — to probe whether verdict-time logic re-derives family from
	// the label instead of the model recorded at launch.
	if err := sv.LaunchReview("sha1", "gpt-shadow-reviewer", "claude-3-7-sonnet"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}

	_, err = sv.SubmitVerdict(ReviewVerdict{
		SHA:      "sha1",
		Reviewer: "gpt-shadow-reviewer",
		Verdict:  VerdictPASS,
		Artifact: ephemeralPASSArtifact(t, "sha1"),
	})
	if err != nil {
		t.Fatalf("SubmitVerdict: %v (verdict gate must trust cand.ReviewFamily, not the reviewer label)", err)
	}
}

func TestSubmitVerdictPASS(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")

	_, err := sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))
	if err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}

	c := sv.Candidate("aaa111")
	if c.State != StateHarvested {
		t.Errorf("state = %s, want harvested", c.State)
	}
	q := sv.QueueSnapshot()
	var qentry QueueEntry
	for _, entry := range q {
		if entry.SHA == "aaa111" {
			qentry = entry
		}
	}
	if qentry.State != QueueCleanupCandidate {
		t.Fatalf("queue after PASS = %+v, want one cleanup candidate", q)
	}

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

func TestMarkClosedProjectsCleanupCandidateToClosed(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	if _, err := sv.SubmitVerdict(passVerdict("aaa111", "gemini", t)); err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}
	if err := sv.MarkClosed("aaa111"); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}
	q := sv.QueueSnapshot()
	var found QueueEntry
	for _, entry := range q {
		if entry.SHA == "aaa111" {
			found = entry
		}
	}
	if found.State != QueueClosed {
		t.Fatalf("queue after close = %+v, want closed", q)
	}
}

// TestReconstructThenMarkClosed proves the cleanup handoff survives a
// supervisor restart. Before the durable-state fix this was the observed
// failure: Reconstruct restored StatePass while MarkClosed required
// StateHarvested, leaving every retained reviewer pane stuck open.
func TestReconstructThenMarkClosed(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	cfg.AdmitReceipt = testAdmitReceipt
	sv := New(cfg)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{SHA: "restart-sha", Branch: "feat/restart", AuthorModel: "claude", Tier: TierR1})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := sv.LaunchReview("restart-sha", "gemini", "gemini-2.5-flash"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}
	if _, err := sv.SubmitVerdict(passVerdict("restart-sha", "gemini", t)); err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}

	restarted := New(cfg)
	if _, err := restarted.Reconstruct(); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	entries := restarted.QueueSnapshot()
	if len(entries) != 1 || entries[0].State != QueueCleanupCandidate {
		t.Fatalf("reconstructed queue = %+v, want cleanup-candidate", entries)
	}
	if err := restarted.MarkClosed("restart-sha"); err != nil {
		t.Fatalf("MarkClosed after reconstruct: %v", err)
	}
	if got := restarted.QueueSnapshot()[0].State; got != QueueClosed {
		t.Fatalf("queue after reconstructed close = %s, want %s", got, QueueClosed)
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

	ready, _ := sv.ReadyForHarvest(10)
	for _, h := range ready {
		if h.SHA == "aaa111" {
			t.Error("aaa111 should not be in harvest queue after FAIL")
		}
	}
}

func TestSubmitVerdictFAILExceedsRetryLimit(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = fixedNow
	cfg.RetryLimit = 1
	sv2 := New(cfg)
	sv2.Ingest(withDigest(CompletionCallback{
		SHA:         "aaa111",
		AuthorModel: "claude",
		Tier:        TierR1,
	}))
	sv2.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
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
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(ReviewVerdict{SHA: "aaa111", Reviewer: "gemini", Verdict: VerdictBLOCKED, Reason: "can't reproduce"})

	c := sv.Candidate("aaa111")
	if c.State != StateBlocked {
		t.Errorf("state = %s, want blocked", c.State)
	}
}

func TestSubmitVerdictWrongReviewerRejected(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")

	_, err := sv.SubmitVerdict(ReviewVerdict{
		SHA:      "aaa111",
		Reviewer: "impostor",
		Verdict:  VerdictPASS,
	})
	if err == nil {
		t.Fatal("expected error for wrong reviewer")
	}
}

func TestSubmitVerdictNotReviewingRejected(t *testing.T) {
	sv := svc(t)
	_, err := sv.SubmitVerdict(ReviewVerdict{
		SHA:      "aaa111",
		Reviewer: "gemini",
		Verdict:  VerdictPASS,
	})
	if err == nil {
		t.Fatal("expected error for non-reviewing candidate")
	}
}

func TestCapacityTracking(t *testing.T) {
	sv := svc(t)

	if sv.AtCapacity() {
		t.Error("expected not at capacity with 2/3 pending")
	}
	if sv.AvailableCapacity() != 1 {
		t.Errorf("AvailableCapacity = %d, want 1", sv.AvailableCapacity())
	}

	sv.Ingest(withDigest(CompletionCallback{SHA: "ccc333", AuthorModel: "gpt-4", Tier: TierR2}))
	if !sv.AtCapacity() {
		t.Error("expected at capacity with 3/3")
	}
	if sv.AvailableCapacity() != 0 {
		t.Errorf("AvailableCapacity = %d, want 0", sv.AvailableCapacity())
	}
}

func TestVerdictReleasesCapacity(t *testing.T) {
	sv := svc(t)
	sv.Ingest(withDigest(CompletionCallback{SHA: "ccc333", AuthorModel: "gpt-4", Tier: TierR2}))
	if !sv.AtCapacity() {
		t.Fatal("expected at capacity")
	}

	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))

	if sv.PendingCount() != 2 {
		t.Errorf("PendingCount = %d, want 2", sv.PendingCount())
	}
	if sv.AtCapacity() {
		t.Error("expected NOT at capacity after resolving one")
	}
}

func TestMarkHarvested(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))

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
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = func() time.Time {
		return time.Date(2025, 6, 3, 12, 0, 0, 0, time.UTC)
	}
	cfg.StaleDuration = 24 * time.Hour

	cfg2 := DefaultConfig(dir)
	cfg2.AdmitReceipt = testAdmitReceipt
	cfg2.Now = func() time.Time {
		return time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	}
	cfg2.StaleDuration = 24 * time.Hour
	sv2 := New(cfg2)
	sv2.Ingest(withDigest(CompletionCallback{SHA: "oldsha", AuthorModel: "claude", Tier: TierR1}))

	cfg3 := DefaultConfig(dir)
	cfg3.AdmitReceipt = testAdmitReceipt
	cfg3.Now = func() time.Time {
		return time.Date(2025, 6, 3, 12, 0, 0, 0, time.UTC)
	}
	cfg3.StaleDuration = 24 * time.Hour
	sv3 := New(cfg3)
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
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = fixedNow
	sv := New(cfg)

	sv.Ingest(withDigest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1, PatchID: "p1"}))
	sv.Ingest(withDigest(CompletionCallback{SHA: "bbb222", AuthorModel: "gemini", Tier: TierR3, PatchID: "p2"}))
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))

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
		t.Errorf("state = %s, want pass", c.State)
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
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = fixedNow
	sv := New(cfg)

	sv.Ingest(withDigest(CompletionCallback{SHA: "old", PatchID: "p1", AuthorModel: "claude", Tier: TierR1}))
	sv.Ingest(withDigest(CompletionCallback{SHA: "new", PatchID: "p1", AuthorModel: "claude", Tier: TierR1}))

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
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = fixedNow
	sv := New(cfg)

	sv.Ingest(withDigest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1}))
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))
	sv.MarkHarvested("aaa111")

	sv2 := New(cfg)
	sv2.Reconstruct()

	c := sv2.Candidate("aaa111")
	if c == nil {
		t.Fatal("aaa111 not found")
	}
	if c.State != StateHarvested {
		t.Errorf("state = %s, want harvested", c.State)
	}
}

func TestReadyForHarvestReturnsSorted(t *testing.T) {
	sv := svc(t)
	sv.Ingest(withDigest(CompletionCallback{SHA: "ccc333", AuthorModel: "gpt-4", Tier: TierR2}))
	sv.Ingest(withDigest(CompletionCallback{SHA: "ddd444", AuthorModel: "codex", Tier: TierR1}))

	for _, sha := range []string{"aaa111", "bbb222", "ccc333", "ddd444"} {
		rev := "gemini"
		mod := "gemini-2.5-flash"
		if sv.Candidate(sha).AuthorFamily == "google" {
			rev = "claude"
			mod = "claude-3-7-sonnet"
		}
		sv.LaunchReview(sha, rev, mod)
		sv.SubmitVerdict(passVerdict(sha, rev, t))
	}

	ready, _ := sv.ReadyForHarvest(10)
	if len(ready) != 4 {
		t.Fatalf("expected 4 ready, got %d", len(ready))
	}
	if ready[0].SHA != "aaa111" || ready[3].SHA != "ddd444" {
		t.Errorf("unexpected order: %v", ready)
	}
}

func TestReadyForHarvestLimit(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))
	sv.LaunchReview("bbb222", "claude", "claude-3-7-sonnet")
	sv.SubmitVerdict(passVerdict("bbb222", "claude", t))

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
	if CrossFamilyOK(FamilyAnt, FamilyOther) {
		t.Error("unknown reviewer family should NOT satisfy cross-family against a known author family")
	}
	if CrossFamilyOK(FamilyOther, FamilyGoogle) {
		t.Error("unknown author family should NOT satisfy cross-family against a known reviewer family")
	}
	if CrossFamilyOK(FamilyOther, FamilyOther) {
		t.Error("two unknown families should NOT satisfy cross-family")
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
		SHA:     "unknown",
		Verdict: VerdictPASS,
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
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))

	ready, _ := sv.ReadyForHarvest(10)
	if len(ready) != 1 {
		t.Fatalf("expected 1 ready, got %d", len(ready))
	}

	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))
	ready2, _ := sv.ReadyForHarvest(10)
	if len(ready2) != 1 {
		t.Errorf("expected still 1 ready (no duplicate), got %d", len(ready2))
	}
}

func TestLostCallbackRecoveryViaReconstruct(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = func() time.Time {
		return time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	}
	sv := New(cfg)

	sv.Ingest(withDigest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1, PatchID: "p1"}))
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")

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

	err := sv.LaunchReview("", "gemini", "gemini-2.5-flash")
	if err == nil {
		t.Error("expected error for empty SHA")
	}

	_, err = sv.SubmitVerdict(ReviewVerdict{SHA: "", Verdict: VerdictPASS})
	if err == nil {
		t.Error("expected error for empty verdict SHA")
	}

	_, _, err = sv.Ingest(withDigest(CompletionCallback{SHA: "", AuthorModel: "claude", Tier: TierR1}))
	if err == nil {
		t.Error("expected error for empty ingest SHA")
	}
}

func TestAvailableCapacityFloor(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.MaxPendingReviews = 1
	sv := New(cfg)
	if sv.AvailableCapacity() != 1 {
		t.Errorf("expected 1, got %d", sv.AvailableCapacity())
	}
	sv.Ingest(withDigest(CompletionCallback{SHA: "s1", AuthorModel: "claude", Tier: TierR1}))
	if sv.AvailableCapacity() != 0 {
		t.Errorf("expected 0, got %d", sv.AvailableCapacity())
	}
	sv.Ingest(withDigest(CompletionCallback{SHA: "s2", AuthorModel: "gemini", Tier: TierR1}))
	if sv.AvailableCapacity() != 0 {
		t.Errorf("expected 0 (clamped), got %d", sv.AvailableCapacity())
	}
}

func TestLedgerFileCreation(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.AdmitReceipt = testAdmitReceipt
	sv := New(cfg)

	sv.Ingest(withDigest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1}))
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))

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
	sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	sv.SubmitVerdict(passVerdict("aaa111", "gemini", t))

	sv.MarkHarvested("aaa111")
	c := sv.Candidate("aaa111")
	if c.State != StateHarvested {
		t.Errorf("state = %s, want harvested", c.State)
	}
}

func TestSelectReviewerAfterRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = fixedNow
	sv := New(cfg)

	sv.Ingest(withDigest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1}))

	sv2 := New(cfg)
	sv2.Reconstruct()

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
	dir := t.TempDir()
	p := filepath.Join(dir, "missing.jsonl")
	rows, err := readRows(p)
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

	// Fail closed: a corrupted evidence file must surface as a hard error,
	// never silently drop replayable events.
	_, err := readRows(p)
	if err == nil {
		t.Fatal("expected error for malformed JSON row")
	}
}

func TestReconstructFailsOnCorruptQueue_RepairProbe(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = fixedNow
	sv := New(cfg)

	if _, _, err := sv.Ingest(withDigest(CompletionCallback{SHA: "aaa111", AuthorModel: "claude", Tier: TierR1})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := os.WriteFile(cfg.QueuePath, []byte("{not json}\n"), 0644); err != nil {
		t.Fatalf("write corrupt queue: %v", err)
	}

	sv2 := New(cfg)
	if _, err := sv2.Reconstruct(); err == nil {
		t.Fatal("expected Reconstruct to fail closed on an unreadable evidence queue")
	}
}

// TestIngestDirtyWorktreeRejected_RepairProbe: a completion callback for a
// dirty worktree describes a tree state that doesn't match the recorded SHA
// and must never be accepted for review.
func TestIngestDirtyWorktreeRejected_RepairProbe(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	accepted, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:           "dirty1",
		AuthorModel:   "claude",
		Tier:          TierR1,
		DirtyWorktree: true,
	}))
	if err == nil {
		t.Fatal("expected error for dirty-worktree completion callback")
	}
	if accepted {
		t.Error("dirty-worktree callback must not be accepted")
	}
	if sv.Candidate("dirty1") != nil {
		t.Error("dirty-worktree callback must not create a candidate")
	}
}

// TestIngestStaleLeaseGenerationRejected_RepairProbe: a callback carrying a
// lease must present a strictly newer generation than any previously
// accepted callback for that lease, or it is a stale/replayed callback.
func TestIngestStaleLeaseGenerationRejected_RepairProbe(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	_, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "sha-gen2",
		AuthorModel: "claude",
		Tier:        TierR1,
		LeaseID:     "lease-1",
		Generation:  2,
	}))
	if err != nil {
		t.Fatalf("Ingest gen 2: %v", err)
	}

	// Same generation replayed for the same lease must be rejected.
	accepted, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "sha-gen2-replay",
		AuthorModel: "claude",
		Tier:        TierR1,
		LeaseID:     "lease-1",
		Generation:  2,
	}))
	if err == nil {
		t.Fatal("expected error for stale (non-increasing) lease generation")
	}
	if accepted {
		t.Error("stale lease generation must not be accepted")
	}

	// An older generation for the same lease must also be rejected.
	_, _, err = sv.Ingest(withDigest(CompletionCallback{
		SHA:         "sha-gen1",
		AuthorModel: "claude",
		Tier:        TierR1,
		LeaseID:     "lease-1",
		Generation:  1,
	}))
	if err == nil {
		t.Fatal("expected error for older lease generation")
	}

	// A strictly newer generation for the same lease must succeed.
	_, _, err = sv.Ingest(withDigest(CompletionCallback{
		SHA:         "sha-gen3",
		AuthorModel: "claude",
		Tier:        TierR1,
		LeaseID:     "lease-1",
		Generation:  3,
	}))
	if err != nil {
		t.Fatalf("expected newer lease generation to be accepted: %v", err)
	}
}

// TestIngestStaleSHAGenerationRejected_RepairProbe: exact stale-SHA
// validation — when superseding a candidate that shares a PatchID, the new
// commit's generation must be newer than the one it replaces.
func TestIngestStaleSHAGenerationRejected_RepairProbe(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	_, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "sha-a",
		PatchID:     "patch-x",
		AuthorModel: "claude",
		Tier:        TierR1,
		Generation:  5,
	}))
	if err != nil {
		t.Fatalf("Ingest sha-a: %v", err)
	}

	accepted, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA:         "sha-b-stale",
		PatchID:     "patch-x",
		AuthorModel: "claude",
		Tier:        TierR1,
		Generation:  5,
	}))
	if err == nil {
		t.Fatal("expected error: same-generation SHA for the same patch is stale")
	}
	if accepted {
		t.Error("stale-generation superseding SHA must not be accepted")
	}
	if sv.Candidate("sha-a").State != StatePending {
		t.Error("original candidate must remain pending after a rejected stale supersede")
	}

	_, _, err = sv.Ingest(withDigest(CompletionCallback{
		SHA:         "sha-c-fresh",
		PatchID:     "patch-x",
		AuthorModel: "claude",
		Tier:        TierR1,
		Generation:  6,
	}))
	if err != nil {
		t.Fatalf("expected newer-generation supersede to succeed: %v", err)
	}
	if sv.Candidate("sha-a").State != StateEvicted {
		t.Error("original candidate must be evicted after a valid newer-generation supersede")
	}
}

// TestBuilderHandoffDurableRoundTrip_RepairProbe: a FAIL/BLOCKED verdict must
// leave a durable, queryable handoff for the owning builder — not just an
// in-memory state transition — and MarkBuilderDelivered must ack it so it is
// not redelivered.
func TestBuilderHandoffDurableRoundTrip_RepairProbe(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("bbb222", "claude", "claude-3-7-sonnet")
	if _, err := sv.SubmitVerdict(ReviewVerdict{
		SHA:      "bbb222",
		Reviewer: "claude",
		Verdict:  VerdictFAIL,
		Reason:   "off-by-one in calc()",
	}); err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}

	handoffs, err := sv.ReadyForBuilder(10)
	if err != nil {
		t.Fatalf("ReadyForBuilder: %v", err)
	}
	if len(handoffs) != 1 {
		t.Fatalf("expected 1 pending builder handoff, got %d", len(handoffs))
	}
	if handoffs[0].SHA != "bbb222" || handoffs[0].Findings != "off-by-one in calc()" {
		t.Errorf("unexpected handoff: %+v", handoffs[0])
	}

	if err := sv.MarkBuilderDelivered("bbb222"); err != nil {
		t.Fatalf("MarkBuilderDelivered: %v", err)
	}

	handoffs2, err := sv.ReadyForBuilder(10)
	if err != nil {
		t.Fatalf("ReadyForBuilder after ack: %v", err)
	}
	if len(handoffs2) != 0 {
		t.Errorf("expected 0 pending handoffs after ack, got %d", len(handoffs2))
	}
}

// TestBuilderHandoffDurableOnDirectBlocked_RepairProbe covers the explicit
// VerdictBLOCKED submission path (a reviewer directly blocking, as opposed
// to a FAIL exhausting the retry limit) — a separate code branch that must
// also durably hand off findings to the builder.
func TestBuilderHandoffDurableOnDirectBlocked_RepairProbe(t *testing.T) {
	sv := svc(t)
	sv.LaunchReview("bbb222", "claude", "claude-3-7-sonnet")
	if _, err := sv.SubmitVerdict(ReviewVerdict{
		SHA:      "bbb222",
		Reviewer: "claude",
		Verdict:  VerdictBLOCKED,
		Reason:   "unsafe pattern, do not retry",
	}); err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}

	handoffs, err := sv.ReadyForBuilder(10)
	if err != nil {
		t.Fatalf("ReadyForBuilder: %v", err)
	}
	if len(handoffs) != 1 {
		t.Fatalf("expected 1 pending builder handoff for direct BLOCKED, got %d", len(handoffs))
	}
	if handoffs[0].Verdict != VerdictBLOCKED || handoffs[0].Findings != "unsafe pattern, do not retry" {
		t.Errorf("unexpected handoff: %+v", handoffs[0])
	}
}

// TestReconstructBackfillsMissingBuilderCallback_RepairProbe: a ledger
// written by an older version that recorded a terminal FAIL/BLOCKED verdict
// without the durable builder_callback event must have one backfilled on
// replay, so the handoff is never silently lost.
func TestReconstructBackfillsMissingBuilderCallback_RepairProbe(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.AdmitReceipt = testAdmitReceipt
	cfg.Now = fixedNow
	cfg.RetryLimit = 1 // FAIL on first attempt goes straight to BLOCKED

	rows := []Row{
		{Event: string(EventCompletion), SHA: "sha1", AuthorModel: "claude", AuthorFamily: "anthropic", Tier: string(TierR1)},
		{Event: string(EventReview), SHA: "sha1", Reviewer: "gemini", ReviewFamily: "google", Tier: string(TierR1), Attempts: 1},
		{Event: string(EventVerdict), SHA: "sha1", Reviewer: "gemini", Verdict: string(VerdictBLOCKED), Reason: "critical bug"},
	}
	f, err := os.Create(cfg.LedgerPath)
	if err != nil {
		t.Fatalf("create ledger: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			t.Fatalf("encode row: %v", err)
		}
	}
	f.Close()

	sv := New(cfg)
	if _, err := sv.Reconstruct(); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}

	handoffs, err := sv.ReadyForBuilder(10)
	if err != nil {
		t.Fatalf("ReadyForBuilder: %v", err)
	}
	if len(handoffs) != 1 {
		t.Fatalf("expected backfilled handoff for sha1, got %d", len(handoffs))
	}
	if handoffs[0].Findings != "critical bug" {
		t.Errorf("backfilled findings = %q, want %q", handoffs[0].Findings, "critical bug")
	}
}

// FAC-144: CheckCompletion-only (no receipt digest) cannot enter review.
func TestIngest_MissingReceiptDigest_Refused(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	_, _, err := sv.Ingest(CompletionCallback{
		SHA:         "aaa111",
		AuthorModel: "claude",
		Tier:        TierR1,
		// deliberately empty ReceiptDigest
	})
	if err == nil {
		t.Fatal("expected missing receipt digest to refuse ingest")
	}
	if !strings.Contains(err.Error(), "receipt digest") {
		t.Fatalf("error = %v, want receipt digest mention", err)
	}
}

func TestLaunchReview_UnconfiguredAdmit_Refused(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	// AdmitReceipt deliberately nil — production miscomposition.
	sv := New(cfg)
	_, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA: "aaa111", AuthorModel: "claude", Tier: TierR1,
	}))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	err = sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	if err == nil {
		t.Fatal("LaunchReview without AdmitReceipt must refuse")
	}
	if !strings.Contains(err.Error(), "admission is not configured") {
		t.Fatalf("error = %v, want unconfigured admission", err)
	}
}

func TestLaunchReview_AdmitRefuses_BlocksSpawn(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	cfg.AdmitReceipt = func(context.Context, string, string) error {
		return errors.New("not PASS")
	}
	sv := New(cfg)
	_, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA: "aaa111", AuthorModel: "claude", Tier: TierR1,
	}))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	err = sv.LaunchReview("aaa111", "gemini", "gemini-2.5-flash")
	if err == nil {
		t.Fatal("LaunchReview must surface admission refusal")
	}
	if !strings.Contains(err.Error(), "receipt admission refused") {
		t.Fatalf("error = %v, want admission refused", err)
	}
	c := sv.Candidate("aaa111")
	if c == nil || c.State != StatePending {
		t.Fatalf("candidate must remain pending after refused launch, got %+v", c)
	}
}

func TestCompensateLaunch_RevertsReviewingOnLaunchFailed(t *testing.T) {
	sv, dir := newTestSupervisor(t)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA: "launch-fail-sha", Branch: "herd/fac-369", PatchID: "fac-369",
		AuthorModel: "claude-3-7-sonnet", Tier: TierR2,
	})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := sv.LaunchReview("launch-fail-sha", "gemini-reviewer", "gemini-2.5-flash"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}
	if got := sv.Candidate("launch-fail-sha"); got == nil || got.State != StateReviewing {
		t.Fatalf("precondition: want reviewing, got %+v", got)
	}
	if err := sv.CompensateLaunch("launch-fail-sha", "LAUNCH_FAILED: unknown pane"); err != nil {
		t.Fatalf("CompensateLaunch: %v", err)
	}
	c := sv.Candidate("launch-fail-sha")
	if c == nil || c.State != StatePending {
		t.Fatalf("LAUNCH_FAILED must not leave an in-review candidate: %+v", c)
	}
	if c.Reviewer != "" {
		t.Fatalf("compensated launch must clear reviewer, got %q", c.Reviewer)
	}
	if q := sv.QueueSnapshot(); len(q) != 1 || q[0].State != QueueAdmitted {
		t.Fatalf("queue after compensate = %+v, want admitted", q)
	}
	rows, err := readRows(filepath.Join(dir, "supervisor-ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.Event == string(EventLaunchFailed) && r.SHA == "launch-fail-sha" && strings.Contains(r.Reason, "LAUNCH_FAILED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ledger missing durable LAUNCH_FAILED row: %+v", rows)
	}
}

func TestCompensateLaunch_ReconstructDoesNotLeaveInReview(t *testing.T) {
	sv, dir := newTestSupervisor(t)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA: "replay-sha", Branch: "herd/fac-369", PatchID: "replay",
		AuthorModel: "claude-3-7-sonnet", Tier: TierR1,
	})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := sv.LaunchReview("replay-sha", "gemini", "gemini-2.5-flash"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}
	if err := sv.CompensateLaunch("replay-sha", "LAUNCH_FAILED: pane is at a login or authentication screen"); err != nil {
		t.Fatalf("CompensateLaunch: %v", err)
	}
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	cfg.AdmitReceipt = testAdmitReceipt
	replay := New(cfg)
	if _, err := replay.Reconstruct(); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	c := replay.Candidate("replay-sha")
	if c == nil || c.State != StatePending || c.Reviewer != "" {
		t.Fatalf("replayed LAUNCH_FAILED left in-review state: %+v", c)
	}
}

func TestDispatchIndependentCandidateIsNotBlockedBySlowRoute(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	for _, sha := range []string{"slow-sha", "safe-sha"} {
		if _, _, err := sv.Ingest(withDigest(CompletionCallback{
			SHA: sha, AuthorModel: "claude-3-7-sonnet", Tier: TierR1,
		})); err != nil {
			t.Fatalf("Ingest %s: %v", sha, err)
		}
	}
	reviewer := ReviewerEntry{Name: "gemini-reviewer", Model: "gemini-2.5-flash"}
	results := sv.Dispatch(context.Background(), []DispatchRequest{
		{
			SHA: "slow-sha", Reviewers: []ReviewerEntry{reviewer}, MaxAttempts: 1,
			Timeout: 20 * time.Millisecond,
			Launch: func(ctx context.Context, _ string, _ ReviewerEntry) error {
				<-ctx.Done()
				return ctx.Err()
			},
		},
		{
			SHA: "safe-sha", Reviewers: []ReviewerEntry{reviewer}, MaxAttempts: 1,
			Launch: func(ctx context.Context, sha string, entry ReviewerEntry) error {
				return sv.LaunchReviewContext(ctx, sha, entry.Name, entry.Model)
			},
		},
	})
	if len(results) != 2 || results[0].SHA != "safe-sha" || results[1].SHA != "slow-sha" {
		t.Fatalf("results = %+v, want deterministic exact-SHA order", results)
	}
	if results[0].State != QueueLaunched || results[0].Reviewer != reviewer.Name {
		t.Fatalf("safe result = %+v, want launched reviewer", results[0])
	}
	if results[1].State != QueueBlocked || !strings.Contains(results[1].Reason, "no routable review surface") {
		t.Fatalf("slow result = %+v, want durable blocked reason", results[1])
	}
	q := sv.QueueSnapshot()
	if len(q) != 2 || q[0].SHA != "safe-sha" || q[0].State != QueueLaunched || q[1].State != QueueBlocked {
		t.Fatalf("queue snapshot = %+v, want exact refs and states", q)
	}
	if q[1].Reason == "" {
		t.Fatal("blocked queue entry lost durable no-route reason")
	}
	if sv.PendingCount() != 1 {
		t.Fatalf("pending count = %d, want only safe candidate", sv.PendingCount())
	}
}

func TestDispatchBlockedRouteReconstructsDurableReason(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	cfg.Now = fixedNow
	cfg.RetryLimit = 2
	cfg.AdmitReceipt = testAdmitReceipt
	sv := New(cfg)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{SHA: "refused-sha", AuthorModel: "claude", Tier: TierR2})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	results := sv.Dispatch(context.Background(), []DispatchRequest{
		{SHA: "refused-sha", Reviewers: []ReviewerEntry{{Name: "gemini", Model: "gemini-2.5-flash"}}, MaxAttempts: 2,
			Launch: func(context.Context, string, ReviewerEntry) error { return errors.New("route refused") }},
	})
	if len(results) != 1 || results[0].State != QueueBlocked || results[0].Attempts != 2 {
		t.Fatalf("dispatch result = %+v, want two-attempt blocked result", results)
	}
	restarted := New(cfg)
	if _, err := restarted.Reconstruct(); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	q := restarted.QueueSnapshot()
	if len(q) != 1 || q[0].State != QueueBlocked || !strings.Contains(q[0].Reason, "route refused") {
		t.Fatalf("reconstructed queue = %+v, want durable blocked reason", q)
	}
}

func TestDispatchCancellationLeavesCandidatePending(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{SHA: "cancel-sha", AuthorModel: "claude", Tier: TierR2})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := sv.Dispatch(ctx, []DispatchRequest{
		{SHA: "cancel-sha", Reviewers: []ReviewerEntry{{Name: "gemini", Model: "gemini-2.5-flash"}}, MaxAttempts: 2},
	})
	if len(results) != 1 || results[0].State == QueueBlocked {
		t.Fatalf("dispatch result = %+v, cancellation must not be durable blocked state", results)
	}
	c := sv.Candidate("cancel-sha")
	if c == nil || c.State != StatePending {
		t.Fatalf("candidate after cancellation = %+v, want pending", c)
	}
	if got := sv.PendingCount(); got != 1 {
		t.Fatalf("pending count after cancellation = %d, want 1", got)
	}
	rows, err := readRows(sv.cfg.LedgerPath)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	for _, row := range rows {
		if row.Event == string(EventDispatchBlocked) {
			t.Fatal("cancellation must not append dispatch_blocked evidence")
		}
	}
}

func TestDispatchStateErrorsDoNotMutateCandidates(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{SHA: "reviewing-sha", AuthorModel: "claude", Tier: TierR2})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := sv.LaunchReview("reviewing-sha", "gemini", "gemini-2.5-flash"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}

	tests := []struct {
		name string
		sha  string
	}{
		{name: "unknown", sha: "missing-sha"},
		{name: "not pending", sha: "reviewing-sha"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := sv.Dispatch(context.Background(), []DispatchRequest{
				{SHA: tt.sha, Reviewers: []ReviewerEntry{{Name: "gemini", Model: "gemini-2.5-flash"}}, MaxAttempts: 1},
			})
			if len(results) != 1 || results[0].State == QueueBlocked {
				t.Fatalf("dispatch result = %+v, state error must not be durable blocked", results)
			}
		})
	}

	c := sv.Candidate("reviewing-sha")
	if c == nil || c.State != StateReviewing {
		t.Fatalf("candidate after state errors = %+v, want reviewing", c)
	}
	if got := sv.PendingCount(); got != 1 {
		t.Fatalf("pending count after state errors = %d, want 1", got)
	}
	rows, err := readRows(sv.cfg.LedgerPath)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	for _, row := range rows {
		if row.Event == string(EventDispatchBlocked) {
			t.Fatal("state errors must not append dispatch_blocked evidence")
		}
	}
}

// TestSubmitVerdictPASSRetainsInboxBeforeCleanupCandidate is the green path
// for FAC-373: a PASS reported from an ephemeral chainseer-style temp path
// becomes cleanup-candidate only after the exact-SHA body is retained under
// .herd/review/inbox, and ReadyForHarvest (coordinator merge-ready) requires
// that same retained state.
func TestSubmitVerdictPASSRetainsInboxBeforeCleanupCandidate(t *testing.T) {
	sv, root := newTestSupervisor(t)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA: "retain-sha", Branch: "herd/fac-373", AuthorModel: "claude", Tier: TierR1,
	})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := sv.LaunchReview("retain-sha", "gemini", "gemini-2.5-flash"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}
	src := ephemeralPASSArtifact(t, "retain-sha")
	if _, err := sv.SubmitVerdict(ReviewVerdict{
		SHA: "retain-sha", Reviewer: "gemini", Verdict: VerdictPASS, Artifact: src,
	}); err != nil {
		t.Fatalf("SubmitVerdict: %v", err)
	}
	// Simulate reviewer pane cleanup racing after retain: temp source vanishes.
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	c := sv.Candidate("retain-sha")
	if c == nil || c.State != StateHarvested || !c.CleanupCandidate || !c.VerdictRetained || !c.Ingested {
		t.Fatalf("candidate after PASS = %+v, want harvested cleanup-candidate with retain+ingest", c)
	}
	if c.Artifact == "" || !strings.HasPrefix(c.Artifact, ".herd/review/inbox/") {
		t.Fatalf("artifact = %q, want durable inbox path", c.Artifact)
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(c.Artifact))); err != nil {
		t.Fatalf("durable inbox artifact must survive source cleanup: %v", err)
	}
	ready, err := sv.ReadyForHarvest(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].SHA != "retain-sha" {
		t.Fatalf("coordinator merge-ready harvest = %+v, want exact retained PASS", ready)
	}
}

// TestSubmitVerdictPASSMissingArtifactIsDurableBlocked proves the red-then-green
// race: when cleanup (or a missing handoff) leaves no retainable source, the
// supervisor records one durable BLOCKED state and never projects cleanup-
// candidate or coordinator-directed PASS/harvest.
func TestSubmitVerdictPASSMissingArtifactIsDurableBlocked(t *testing.T) {
	sv, _ := newTestSupervisor(t)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA: "race-sha", Branch: "herd/fac-373", AuthorModel: "claude", Tier: TierR1,
	})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := sv.LaunchReview("race-sha", "gemini", "gemini-2.5-flash"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}

	// RED path: claimed PASS with no artifact (vanished temp path).
	st, err := sv.SubmitVerdict(ReviewVerdict{
		SHA: "race-sha", Reviewer: "gemini", Verdict: VerdictPASS, Artifact: "",
	})
	if err == nil {
		t.Fatal("missing artifact must fail closed")
	}
	if st != StateBlocked {
		t.Fatalf("state = %s, want BLOCKED", st)
	}
	c := sv.Candidate("race-sha")
	if c.State != StateBlocked || c.CleanupCandidate || c.HarvestReady || c.Ingested {
		t.Fatalf("candidate = %+v, want durable BLOCKED without cleanup/harvest", c)
	}
	if c.Verdict != VerdictBLOCKED {
		t.Fatalf("verdict = %s, want BLOCKED", c.Verdict)
	}
	ready, err := sv.ReadyForHarvest(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("coordinator must not see merge-ready PASS after vanished artifact: %+v", ready)
	}
	for _, e := range sv.QueueSnapshot() {
		if e.SHA == "race-sha" && e.State == QueueCleanupCandidate {
			t.Fatalf("queue projected cleanup-candidate without retention: %+v", e)
		}
	}

	// Reconstruct must preserve the single durable BLOCKED, not invent PASS.
	restarted := New(sv.cfg)
	if _, err := restarted.Reconstruct(); err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	rc := restarted.Candidate("race-sha")
	if rc == nil || rc.State != StateBlocked || rc.CleanupCandidate {
		t.Fatalf("reconstructed = %+v, want durable BLOCKED", rc)
	}
}

// TestSubmitVerdictPASSVanishedSourceDuringCleanupRace covers the live
// Chainseer failure mode: a temp path under chainseer-herd-review is reported
// then deleted before retain completes. Result must be BLOCKED, never cleanup.
func TestSubmitVerdictPASSVanishedSourceDuringCleanupRace(t *testing.T) {
	sv, root := newTestSupervisor(t)
	if _, _, err := sv.Ingest(withDigest(CompletionCallback{
		SHA: "vanish-sha", Branch: "herd/fac-373", AuthorModel: "claude", Tier: TierR1,
	})); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := sv.LaunchReview("vanish-sha", "gemini", "gemini-2.5-flash"); err != nil {
		t.Fatalf("LaunchReview: %v", err)
	}
	src := ephemeralPASSArtifact(t, "vanish-sha")
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	st, err := sv.SubmitVerdict(ReviewVerdict{
		SHA: "vanish-sha", Reviewer: "gemini", Verdict: VerdictPASS, Artifact: src,
	})
	if err == nil {
		t.Fatal("vanished source must fail closed")
	}
	if st != StateBlocked {
		t.Fatalf("state = %s, want BLOCKED", st)
	}
	inbox := filepath.Join(root, ".herd", "review", "inbox")
	if entries, err := os.ReadDir(inbox); err == nil && len(entries) != 0 {
		t.Fatalf("failed retain must not leave inbox entries: %v", entries)
	}
	ready, err := sv.ReadyForHarvest(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("vanished artifact must not be merge-ready: %+v", ready)
	}
}
