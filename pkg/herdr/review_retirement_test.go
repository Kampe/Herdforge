package herdr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func retirementManifest(t *testing.T, generation string) ReviewRetirementManifest {
	t.Helper()
	m := NewReviewRetirementManifest(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC), ReviewRetirementManifest{
		Repository: "herdforge", TaskRef: "FAC-708", TaskID: "task-708",
		CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Branch: "review/FAC-708",
		Worktree: ".herd/reviews/fac-708", Pool: ".herd/pool", Slot: "pool-01", LeaseGeneration: 7,
		Workspace: "wK", TabID: "wK:t15T", PaneID: "wK:p15T", TerminalID: "term-1", SessionGeneration: "g1",
		Reviewer: "forge-mender-fac708-nat-d4b3b8dc", ReviewerFamily: "openai", ReviewerModel: "gpt-5.6-luna",
		PromptArtifact: ".herd/review/prompts/fac-708.md", Generation: generation, Nonce: "nonce-1",
	})
	if err := ValidateReviewRetirementManifest(m); err != nil {
		t.Fatal(err)
	}
	return m
}

func retirementEvidence(m ReviewRetirementManifest) ReviewRetirementEvidence {
	return ReviewRetirementEvidence{Manifest: m, Verdict: ReviewRetirementVerdict{CandidateSHA: m.CandidateSHA, Durable: true, Terminal: true, ReviewerACK: true}, Head: m.CandidateSHA, Branch: m.Branch, WorktreeRoot: ".herd/reviews", PromptRoot: ".herd/review/prompts", Repository: m.Repository}
}

func TestEvaluateReviewRetirementRequiresExactTerminalVerdict(t *testing.T) {
	for name, mutate := range map[string]func(*ReviewRetirementEvidence){
		"missing durable": func(e *ReviewRetirementEvidence) { e.Verdict.Durable = false },
		"missing ack":     func(e *ReviewRetirementEvidence) { e.Verdict.ReviewerACK = false },
		"wrong sha":       func(e *ReviewRetirementEvidence) { e.Verdict.CandidateSHA = strings.Repeat("c", 40) },
		"active":          func(e *ReviewRetirementEvidence) { e.Active = true },
		"focused":         func(e *ReviewRetirementEvidence) { e.Focused = true },
		"dirty":           func(e *ReviewRetirementEvidence) { e.Dirty = true },
		"drift":           func(e *ReviewRetirementEvidence) { e.Head = strings.Repeat("c", 40) },
		"unique":          func(e *ReviewRetirementEvidence) { e.UniqueCommits = true },
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
	events   []string
	evidence map[string]ReviewRetirementEvidence
	fail     string
}

func (f *retirementFake) Observe(m ReviewRetirementManifest) (ReviewRetirementEvidence, error) {
	return f.evidence[m.Generation], nil
}
func (f *retirementFake) step(name string, m ReviewRetirementManifest) error {
	f.events = append(f.events, name)
	if f.fail == name {
		return errors.New("injected failure")
	}
	return nil
}
func (f *retirementFake) Close(m ReviewRetirementManifest) error { return f.step("close", m) }
func (f *retirementFake) LeaseReleased(m ReviewRetirementManifest) (bool, error) {
	f.events = append(f.events, "lease-read")
	return false, nil
}
func (f *retirementFake) ReleaseLease(m ReviewRetirementManifest) error {
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
	f.evidence["g2"] = retirementEvidence(m2)
	f.evidence["g2"] = ReviewRetirementEvidence{Manifest: m2, Verdict: ReviewRetirementVerdict{CandidateSHA: m2.CandidateSHA, Durable: true, Terminal: true, ReviewerACK: true}, Head: m2.CandidateSHA, Branch: m2.Branch, WorktreeRoot: ".herd/other", PromptRoot: ".herd/review/prompts", Repository: m2.Repository}
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
	want := []string{"close", "lease-read", "lease-release", "worktree", "branch", "artifact", "receipt", "close", "lease-read", "lease-release", "worktree", "branch", "artifact", "receipt"}
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

func TestRetireReviewLanesDryRunNeverMutates(t *testing.T) {
	m := retirementManifest(t, "g1")
	f := &retirementFake{evidence: map[string]ReviewRetirementEvidence{"g1": retirementEvidence(m)}}
	r, err := RetireReviewLanes(f, []ReviewRetirementManifest{m}, true)
	if err != nil || !r.DryRun || r.Retired != 0 || len(f.events) != 0 {
		t.Fatalf("report=%+v events=%v err=%v", r, f.events, err)
	}
}
