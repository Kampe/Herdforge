package reviewledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBindTaskAppendsAuthenticatedCorrectionAndProjectsNewTask(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	prior := VerdictOpts{SHA: sha, Reviewer: "reviewer", ReviewerFamily: "openai", BuilderFamily: "open-weight", Task: "FAC-794", Verdict: VerdictPASS, VfyDigest: "vfy"}
	if _, err := l.Verdict(prior); err != nil {
		t.Fatal(err)
	}
	row, found, err := l.VerdictForReviewer(sha, prior.Reviewer)
	if err != nil || !found {
		t.Fatalf("prior verdict lookup: found=%v err=%v", found, err)
	}
	artifact := filepath.Join(dir, "correction.md")
	if err := os.WriteFile(artifact, []byte("correction evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.BindTask(TaskBindingOpts{SHA: sha, Reviewer: prior.Reviewer, PreviousTask: "FAC-794", Task: "FAC-798", PriorEventDigest: VerdictEventDigest(row), Artifact: artifact, ArtifactDigest: strings.Repeat("b", 64)}); err != nil {
		t.Fatal(err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Task != "FAC-794" || rows[1].Event != string(EventTaskBinding) {
		t.Fatalf("append-only rows=%+v", rows)
	}
	effective, found, err := l.VerdictForReviewer(sha, prior.Reviewer)
	if err != nil || !found || effective.Task != "FAC-798" {
		t.Fatalf("effective task=%q found=%v err=%v", effective.Task, found, err)
	}
}

func TestBindTaskRejectsStaleOrConflictingCorrection(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("c", 40)
	prior := VerdictOpts{SHA: sha, Reviewer: "reviewer", ReviewerFamily: "google", BuilderFamily: "openai", Task: "FAC-794", Verdict: VerdictPASS, VfyDigest: "vfy"}
	if _, err := l.Verdict(prior); err != nil {
		t.Fatal(err)
	}
	base := TaskBindingOpts{SHA: sha, Reviewer: prior.Reviewer, PreviousTask: "FAC-794", Task: "FAC-798", PriorEventDigest: strings.Repeat("d", 64), Artifact: "correction.md", ArtifactDigest: strings.Repeat("e", 64)}
	if err := l.BindTask(base); err == nil {
		t.Fatal("stale prior digest accepted")
	}
	row, _, _ := l.VerdictForReviewer(sha, prior.Reviewer)
	base.PriorEventDigest = VerdictEventDigest(row)
	if err := l.BindTask(base); err != nil {
		t.Fatal(err)
	}
	base.Task = "FAC-799"
	if err := l.BindTask(base); err == nil {
		t.Fatal("conflicting second binding accepted")
	}
}
