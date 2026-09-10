package herdr

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func retirementManifest(t *testing.T, generation string) ReviewRetirementManifest {
	t.Helper()
	m := NewReviewRetirementManifest(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), ReviewRetirementManifest{
		Repository: "herdforge", TaskRef: "FAC-708", TaskID: "task-708",
		CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Branch: "review/FAC-708",
		Worktree: ".herd/reviews/fac-708", Pool: ".herd/pool", Slot: "pool-01", LeaseGeneration: 7,
		Workspace: "wK", TabID: "wK:t15T", PaneID: "wK:p15T", TerminalID: "term-1", SessionID: "session-1", SessionGeneration: "",
		Reviewer: "forge-mender-fac708-nat-d4b3b8dc", ReviewerFamily: "openai", ReviewerModel: "gpt-5.6-luna",
		PromptArtifact: ".herd/review/prompts/fac-708.md", Generation: generation, Nonce: "nonce-1",
	})
	if err := ValidateReviewRetirementManifest(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func retirementEvidence(m ReviewRetirementManifest) ReviewRetirementEvidence {
	launch := reviewledger.LedgerRow{Event: string(reviewledger.EventRecord), SHA: m.CandidateSHA, Reviewer: m.Reviewer, Lease: m.Nonce, Branch: m.TaskRef}
	verdict := reviewledger.LedgerRow{Event: string(reviewledger.EventVerdict), SHA: m.CandidateSHA, CandidateSHA: m.CandidateSHA, Reviewer: m.Reviewer, Verdict: string(reviewledger.VerdictPASS), ArtifactDigest: "artifact"}
	ack := reviewack.Ack{SHA: m.CandidateSHA, Reviewer: m.Reviewer, LaunchIdentity: m.Reviewer, ArtifactDigest: verdict.ArtifactDigest}
	focused := false
	return ReviewRetirementEvidence{Manifest: m, Launch: launch, Verdict: ReviewRetirementVerdict{Row: verdict, Ack: ack}, Live: ReviewRetirementLive{Status: "idle", Focused: &focused, SessionID: m.SessionID}, Worktree: ReviewRetirementWorktree{Known: true, Head: m.CandidateSHA, Branch: m.Branch}, WorktreeRoot: ".herd/reviews", PromptRoot: ".herd/review/prompts", Repository: m.Repository}
}

func TestEvaluateReviewRetirementRequiresExactTerminalVerdict(t *testing.T) {
	for name, mutate := range map[string]func(*ReviewRetirementEvidence){
		"missing durable": func(e *ReviewRetirementEvidence) { e.Verdict.Row.Event = "" },
		"missing ack":     func(e *ReviewRetirementEvidence) { e.Verdict.Ack = reviewack.Ack{} },
		"wrong sha":       func(e *ReviewRetirementEvidence) { e.Verdict.Row.SHA = strings.Repeat("c", 40) },
		"active":          func(e *ReviewRetirementEvidence) { e.Live.Status = "working" },
		"focused":         func(e *ReviewRetirementEvidence) { *e.Live.Focused = true },
		"dirty":           func(e *ReviewRetirementEvidence) { e.Worktree.Dirty = true },
		"drift":           func(e *ReviewRetirementEvidence) { e.Worktree.Head = strings.Repeat("c", 40) },
		"unique":          func(e *ReviewRetirementEvidence) { e.Worktree.UniqueRefs = true },
		"namespace":       func(e *ReviewRetirementEvidence) { e.Manifest.Worktree = ".herd/worktrees/fac-708" },
	} {
		t.Run(name, func(t *testing.T) {
			e := retirementEvidence(retirementManifest(t, "g1"))
			mutate(&e)
			if d := EvaluateReviewRetirement(e); d.Eligible || !strings.HasPrefix(d.Reason, "BLOCKED:") {
				t.Fatalf("decision=%+v", d)
			}
		})
	}
}

func TestEvaluateReviewRetirementAllowsExactCleanSettledLane(t *testing.T) {
	if d := EvaluateReviewRetirement(retirementEvidence(retirementManifest(t, "g1"))); !d.Eligible {
		t.Fatalf("decision=%+v", d)
	}
}

func TestEvaluateReviewRetirementTaskAndBranchBindings(t *testing.T) {
	m := retirementManifest(t, "g-bind")
	baseEvidence := retirementEvidence(m)

	// Canonical launch row with Task set and empty Branch (production pattern)
	eTaskOnly := baseEvidence
	eTaskOnly.Launch = reviewledger.LedgerRow{Event: string(reviewledger.EventRecord), SHA: m.CandidateSHA, Reviewer: m.Reviewer, Lease: m.Nonce, Task: m.TaskRef}
	if d := EvaluateReviewRetirement(eTaskOnly); !d.Eligible {
		t.Fatalf("launch with Task only should be eligible: %+v", d)
	}

	// Launch row with matching Task and feature Branch
	eTaskAndBranch := baseEvidence
	eTaskAndBranch.Launch = reviewledger.LedgerRow{Event: string(reviewledger.EventRecord), SHA: m.CandidateSHA, Reviewer: m.Reviewer, Lease: m.Nonce, Task: m.TaskRef, Branch: "recovery/fac-708-review-retirement"}
	if d := EvaluateReviewRetirement(eTaskAndBranch); !d.Eligible {
		t.Fatalf("launch with matching Task and branch should be eligible: %+v", d)
	}

	// Launch row with wrong Task
	eWrongTask := baseEvidence
	eWrongTask.Launch = reviewledger.LedgerRow{Event: string(reviewledger.EventRecord), SHA: m.CandidateSHA, Reviewer: m.Reviewer, Lease: m.Nonce, Task: "FAC-999"}
	if d := EvaluateReviewRetirement(eWrongTask); d.Eligible || !strings.Contains(d.Reason, "verified launch provenance does not bind") {
		t.Fatalf("wrong task must be blocked: %+v", d)
	}

	// Launch row with wrong legacy Branch task
	eWrongBranch := baseEvidence
	eWrongBranch.Launch = reviewledger.LedgerRow{Event: string(reviewledger.EventRecord), SHA: m.CandidateSHA, Reviewer: m.Reviewer, Lease: m.Nonce, Branch: "FAC-999"}
	if d := EvaluateReviewRetirement(eWrongBranch); d.Eligible || !strings.Contains(d.Reason, "verified launch provenance does not bind") {
		t.Fatalf("wrong legacy branch task must be blocked: %+v", d)
	}

	// Unbound launch row (no Task, no Branch)
	eUnbound := baseEvidence
	eUnbound.Launch = reviewledger.LedgerRow{Event: string(reviewledger.EventRecord), SHA: m.CandidateSHA, Reviewer: m.Reviewer, Lease: m.Nonce}
	if d := EvaluateReviewRetirement(eUnbound); d.Eligible || !strings.Contains(d.Reason, "verified launch provenance does not bind") {
		t.Fatalf("unbound launch must be blocked: %+v", d)
	}
}

func TestReviewRetirementManifestDigestAndRegistryAreBoundAndAppendOnly(t *testing.T) {
	m := retirementManifest(t, "g1")
	if m.BindingDigest != ReviewRetirementBindingDigest(m) {
		t.Fatal("digest does not bind manifest")
	}
	m.CandidateSHA = strings.Repeat("c", 40)
	if err := ValidateReviewRetirementManifest(m); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	reg := ReviewRetirementRegistry{Path: t.TempDir() + "/manifests.jsonl"}
	m = retirementManifest(t, "g1")
	if err := reg.Record(m); err != nil {
		t.Fatal(err)
	}
	if err := reg.Record(m); err != nil {
		t.Fatal(err)
	}
	rows, err := reg.Latest()
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}

type retirementFake struct {
	events     []string
	evidence   map[string]ReviewRetirementEvidence
	observeErr map[string]error
	fail       string
	failGen    string
}

func (f *retirementFake) Observe(m ReviewRetirementManifest) (ReviewRetirementEvidence, error) {
	if err := f.observeErr[m.Generation]; err != nil {
		return ReviewRetirementEvidence{}, err
	}
	return f.evidence[m.Generation], nil
}
func (f *retirementFake) Revalidate(m ReviewRetirementManifest, phase string) error { return nil }
func (f *retirementFake) Journal(m ReviewRetirementManifest, phase string) error {
	return f.step("journal-"+phase, m)
}
func (f *retirementFake) step(name string, m ReviewRetirementManifest) error {
	f.events = append(f.events, name)
	if f.fail == name && (f.failGen == "" || f.failGen == m.Generation) {
		return errors.New("injected failure")
	}
	return nil
}
func (f *retirementFake) Close(m ReviewRetirementManifest) error { return f.step("close", m) }
func (f *retirementFake) LeaseReleased(m ReviewRetirementManifest) (bool, error) {
	f.events = append(f.events, "lease-read")
	return false, nil
}
func (f *retirementFake) ReleaseLease(_ context.Context, m ReviewRetirementManifest) error {
	return f.step("lease-release", m)
}
func (f *retirementFake) RemoveWorktree(m ReviewRetirementManifest) error {
	return f.step("worktree", m)
}
func (f *retirementFake) RemoveBranch(m ReviewRetirementManifest) error { return f.step("branch", m) }
func (f *retirementFake) RemoveArtifact(m ReviewRetirementManifest) error {
	return f.step("artifact", m)
}
func (f *retirementFake) Receipt(m ReviewRetirementManifest, d ReviewRetirementDecision) error {
	return f.step("receipt", m)
}

func TestRetireReviewLanesPreflightsAllBeforeMutationAndOrdersOperations(t *testing.T) {
	m1, m2 := retirementManifest(t, "g1"), retirementManifest(t, "g2")
	f := &retirementFake{evidence: map[string]ReviewRetirementEvidence{"g1": retirementEvidence(m1), "g2": retirementEvidence(m2)}}
	e2 := retirementEvidence(m2)
	e2.WorktreeRoot = ".herd/other"
	f.evidence["g2"] = e2
	r, err := RetireReviewLanes(f, []ReviewRetirementManifest{m1, m2}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Retired != 0 || r.Blocked != 1 || len(f.events) != 0 {
		t.Fatalf("report=%+v events=%v", r, f.events)
	}
	f.evidence["g2"] = retirementEvidence(m2)
	r, err = RetireReviewLanes(f, []ReviewRetirementManifest{m2, m1}, false)
	if err != nil || r.Retired != 2 {
		t.Fatalf("report=%+v err=%v", r, err)
	}
	want := []string{"close", "lease-read", "lease-release", "journal-worktree-intent", "worktree", "journal-worktree-done", "journal-ref-intent", "branch", "journal-ref-done", "journal-artifacts-intent", "artifact", "journal-artifacts-done", "receipt", "close", "lease-read", "lease-release", "journal-worktree-intent", "worktree", "journal-worktree-done", "journal-ref-intent", "branch", "journal-ref-done", "journal-artifacts-intent", "artifact", "journal-artifacts-done", "receipt"}
	for i := range want {
		if f.events[i] != want[i] {
			t.Fatalf("events=%v want=%v", f.events, want)
		}
	}
}

func TestRetireReviewLanesStopsAfterEarlierFailure(t *testing.T) {
	m := retirementManifest(t, "g1")
	f := &retirementFake{evidence: map[string]ReviewRetirementEvidence{"g1": retirementEvidence(m)}, fail: "lease-release"}
	r, err := RetireReviewLanes(f, []ReviewRetirementManifest{m}, false)
	if err == nil || r.Failed != 1 {
		t.Fatalf("report=%+v err=%v", r, err)
	}
	if strings.Join(f.events, ",") != "close,lease-read,lease-release" {
		t.Fatalf("unsafe later operations ran: %v", f.events)
	}
}

func TestRetireReviewLanesFaultMatrixStopsBeforeLaterDestructiveBoundary(t *testing.T) {
	m1 := retirementManifest(t, "g1")
	m2 := retirementManifest(t, "g2")
	m3 := retirementManifest(t, "g3")
	boundaries := []struct {
		name          string
		expectedError string
	}{
		{name: "close", expectedError: "close "},
		{name: "lease-release", expectedError: "release lease "},
		{name: "journal-worktree-intent", expectedError: "journal worktree phase "},
		{name: "worktree", expectedError: "remove worktree "},
		{name: "journal-worktree-done", expectedError: "journal worktree completion "},
		{name: "journal-ref-intent", expectedError: "journal ref phase "},
		{name: "branch", expectedError: "remove branch "},
		{name: "journal-ref-done", expectedError: "journal ref completion "},
		{name: "journal-artifacts-intent", expectedError: "journal artifact phase "},
		{name: "artifact", expectedError: "remove owned artifacts "},
		{name: "journal-artifacts-done", expectedError: "journal artifacts completion "},
		{name: "receipt", expectedError: "write retirement receipt "},
	}
	for _, b := range boundaries {
		t.Run(b.name, func(t *testing.T) {
			f := &retirementFake{
				evidence: map[string]ReviewRetirementEvidence{
					"g1": retirementEvidence(m1),
					"g2": retirementEvidence(m2),
					"g3": retirementEvidence(m3),
				},
				fail:    b.name,
				failGen: "g2",
			}
			r, err := RetireReviewLanes(f, []ReviewRetirementManifest{m1, m2, m3}, false)
			if err == nil || r.Failed != 1 {
				t.Fatalf("boundary %s was not surfaced: report=%+v err=%v events=%v", b.name, r, err, f.events)
			}
			if r.Retired != 1 {
				t.Fatalf("expected 1 retired candidate (g1), got %d: %+v", r.Retired, r)
			}
			if len(r.Candidates) != 3 {
				t.Fatalf("expected 3 candidates, got %d", len(r.Candidates))
			}

			// Candidate 0: cleanly retired before failure
			c0 := r.Candidates[0]
			if !c0.Retired || !c0.Completed || c0.Failed || c0.Error != "" {
				t.Fatalf("candidate 0 should be cleanly retired: %+v", c0)
			}
			if !c0.Decision.Eligible {
				t.Fatalf("candidate 0 should remain eligible: %+v", c0)
			}

			// Candidate 1: failed at this boundary; must NOT be marked Retired or Completed
			c1 := r.Candidates[1]
			if c1.Retired {
				t.Fatalf("candidate 1 at boundary %s must have Retired=false, got Retired=true: %+v", b.name, c1)
			}
			if c1.Completed {
				t.Fatalf("candidate 1 at boundary %s must have Completed=false, got Completed=true: %+v", b.name, c1)
			}
			if !c1.Failed {
				t.Fatalf("candidate 1 at boundary %s must have Failed=true: %+v", b.name, c1)
			}
			if !strings.Contains(c1.Error, b.expectedError) || !strings.Contains(c1.Error, "injected failure") {
				t.Fatalf("candidate 1 error %q does not match expected prefix %q with injected failure", c1.Error, b.expectedError)
			}
			if !c1.Decision.Eligible {
				t.Fatalf("candidate 1 should preserve initial Eligible=true decision: %+v", c1)
			}

			// Candidate 2: unattempted due to prior failure
			c2 := r.Candidates[2]
			if c2.Retired || c2.Completed || c2.Failed || c2.Error != "" {
				t.Fatalf("candidate 2 should be unattempted: %+v", c2)
			}
			if !c2.Decision.Eligible {
				t.Fatalf("candidate 2 should preserve initial Eligible=true decision: %+v", c2)
			}

			// Candidate 0 ran 13 phases. Verify that candidate 1 (starting at index 13)
			// never executed any later destructive boundary after the injected failure.
			c1Events := f.events[13:]
			seen := false
			for _, event := range c1Events {
				if event == b.name {
					seen = true
					continue
				}
				if seen && (event == "worktree" || event == "branch" || event == "artifact" || event == "receipt") {
					t.Fatalf("later destructive boundary %q ran for candidate 1 after injected %q: %v", event, b.name, c1Events)
				}
			}
		})
	}
}

func TestRetireReviewLanesDryRunNeverMutates(t *testing.T) {
	m := retirementManifest(t, "g1")
	f := &retirementFake{evidence: map[string]ReviewRetirementEvidence{"g1": retirementEvidence(m)}}
	r, err := RetireReviewLanes(f, []ReviewRetirementManifest{m}, true)
	if err != nil || !r.DryRun || r.Retired != 0 || len(f.events) != 0 {
		t.Fatalf("report=%+v events=%v err=%v", r, f.events, err)
	}
}

func TestRetireReviewLanesCloseFailurePopulatesCandidateStateAndLeavesLaterUnattempted(t *testing.T) {
	m1 := retirementManifest(t, "g1")
	m2 := retirementManifest(t, "g2")
	m3 := retirementManifest(t, "g3")
	f := &retirementFake{
		evidence: map[string]ReviewRetirementEvidence{
			"g1": retirementEvidence(m1),
			"g2": retirementEvidence(m2),
			"g3": retirementEvidence(m3),
		},
		fail:    "close",
		failGen: "g2",
	}

	r, err := RetireReviewLanes(f, []ReviewRetirementManifest{m1, m2, m3}, false)
	if err == nil {
		t.Fatalf("expected close failure error, got nil")
	}
	if r.Retired != 1 {
		t.Fatalf("expected 1 retired lane, got %d", r.Retired)
	}
	if r.Failed != 1 {
		t.Fatalf("expected 1 failed lane, got %d", r.Failed)
	}
	if len(r.Candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(r.Candidates))
	}

	// First candidate: retired cleanly
	if !r.Candidates[0].Retired || !r.Candidates[0].Completed || r.Candidates[0].Failed {
		t.Fatalf("candidate 0 should be retired: %+v", r.Candidates[0])
	}

	// Second candidate: close failed during mutation
	if r.Candidates[1].Retired || !r.Candidates[1].Failed || !strings.Contains(r.Candidates[1].Error, "close") {
		t.Fatalf("candidate 1 should have Failed=true and close error, got: %+v", r.Candidates[1])
	}
	if !r.Candidates[1].Decision.Eligible {
		t.Fatalf("candidate 1 had Eligible=true during observation, should remain eligible: %+v", r.Candidates[1])
	}

	// Third candidate: unattempted due to prior failure
	if r.Candidates[2].Retired || r.Candidates[2].Failed || r.Candidates[2].Completed {
		t.Fatalf("candidate 2 should be unattempted (not retired, not failed): %+v", r.Candidates[2])
	}
	if !r.Candidates[2].Decision.Eligible {
		t.Fatalf("candidate 2 should remain Eligible=true: %+v", r.Candidates[2])
	}
}
