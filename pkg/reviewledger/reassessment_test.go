package reviewledger

import (
	"strings"
	"testing"
)

func TestSameReviewerReassessment(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	old := VerdictOpts{SHA: strings.Repeat("a", 40), Reviewer: "reviewer", ReviewerFamily: "google", BuilderFamily: "anthropic", Task: "FAC-493", Verdict: VerdictFAIL, VfyDigest: "old-evidence"}
	if _, err := l.Verdict(old); err != nil {
		t.Fatal(err)
	}
	prior, _, err := l.VerdictForReviewer(old.SHA, old.Reviewer)
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.Verdict = VerdictPASS
	next.VfyDigest = "new-evidence"
	next.Reassesses = VerdictEventDigest(prior)
	next.ArtifactDigest = strings.Repeat("b", 64)
	if _, err := l.Verdict(next); err != nil {
		t.Fatal(err)
	}
	rowsBefore, _ := l.AllRows()
	if _, err := l.Verdict(next); err != nil {
		t.Fatal(err)
	}
	rowsAfter, _ := l.AllRows()
	if len(rowsAfter) != len(rowsBefore) {
		t.Fatal("identical reassessment replay appended history")
	}
	stale := next
	stale.ArtifactDigest = strings.Repeat("c", 64)
	stale.VfyDigest = "another-evidence"
	if _, err := l.Verdict(stale); err == nil {
		t.Fatal("stale prior event accepted")
	}
	latest, _, err := l.VerdictForReviewer(old.SHA, old.Reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Verdict != "PASS" || latest.Reassesses != next.Reassesses {
		t.Fatalf("reassessment discarded: %+v", latest)
	}
	if err := l.EnsureRecord(RecordOpts{SHA: old.SHA, Reviewer: old.Reviewer, Task: old.Task, BuilderFamily: old.BuilderFamily, ReviewerFamily: old.ReviewerFamily, Gate: "independent"}); err != nil {
		t.Fatal(err)
	}
	ready, err := l.MergeReadinessFor(old.SHA)
	if err != nil || !ready.Ready {
		t.Fatalf("same reviewer correction not ready: %+v %v", ready, err)
	}
	other := old
	other.Reviewer = "other"
	if _, err := l.Verdict(other); err != nil {
		t.Fatal(err)
	}
	ready, err = l.MergeReadinessFor(old.SHA)
	if err != nil || ready.Ready {
		t.Fatalf("other reviewer FAIL lost: %+v %v", ready, err)
	}
	later := next
	later.Verdict = VerdictFAIL
	later.Reassesses = VerdictEventDigest(latest)
	later.VfyDigest = "later failure evidence"
	later.ArtifactDigest = strings.Repeat("d", 64)
	if _, err := l.Verdict(later); err != nil {
		t.Fatal(err)
	}
	latest, _, err = l.VerdictForReviewer(old.SHA, old.Reviewer)
	if err != nil || latest.Verdict != "FAIL" {
		t.Fatalf("later FAIL lost: %+v %v", latest, err)
	}
}

func TestReassessmentRefusesIdentityAndEvidenceChanges(t *testing.T) {
	prior := LedgerRow{Event: string(EventVerdict), SHA: strings.Repeat("a", 40), Reviewer: "r", Task: "FAC-493", BuilderFamily: "anthropic", ReviewerFamily: "google", Verdict: "FAIL", VerificationDigest: "old"}
	base := VerdictOpts{SHA: prior.SHA, Reviewer: prior.Reviewer, Task: prior.Task, BuilderFamily: prior.BuilderFamily, ReviewerFamily: prior.ReviewerFamily, Verdict: VerdictPASS, VfyDigest: "new", ArtifactDigest: strings.Repeat("b", 64), Reassesses: VerdictEventDigest(prior)}
	for name, mutate := range map[string]func(*VerdictOpts){
		"other reviewer":              func(o *VerdictOpts) { o.Reviewer = "another" },
		"other task":                  func(o *VerdictOpts) { o.Task = "FAC-494" },
		"family":                      func(o *VerdictOpts) { o.ReviewerFamily = "openai" },
		"same evidence":               func(o *VerdictOpts) { o.VfyDigest = "old" },
		"missing artifact":            func(o *VerdictOpts) { o.ArtifactDigest = "" },
		"cross reviewer supersession": func(o *VerdictOpts) { o.RetryOf = "another" },
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			if _, err := CheckReassessment(prior, o); err == nil {
				t.Fatal("invalid correction accepted")
			}
		})
	}
}
