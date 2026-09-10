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
	if got, want := VerdictEventDigest(effective), VerdictEventDigest(row); got != want {
		t.Fatalf("projected verdict changed raw event identity: got=%s want=%s", got, want)
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

func TestBindTaskSupportsFencedCorrectionChainWithStableRawDigests(t *testing.T) {
	dir := t.TempDir()
	l, err := NewReviewLedger(dir, DefaultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("f", 40)
	prior := VerdictOpts{SHA: sha, Reviewer: "reviewer", ReviewerFamily: "openai", BuilderFamily: "open-weight", Task: "FAC-794", Verdict: VerdictPASS, VfyDigest: "vfy"}
	if _, err := l.Verdict(prior); err != nil {
		t.Fatal(err)
	}
	before, found, err := l.VerdictForReviewer(sha, prior.Reviewer)
	if err != nil || !found {
		t.Fatalf("initial verdict: found=%v err=%v", found, err)
	}
	first := TaskBindingOpts{SHA: sha, Reviewer: prior.Reviewer, PreviousTask: "FAC-794", Task: "FAC-798", PriorEventDigest: VerdictEventDigest(before), Artifact: "first.md", ArtifactDigest: strings.Repeat("1", 64)}
	if err := l.BindTask(first); err != nil {
		t.Fatal(err)
	}
	afterFirst, found, err := l.VerdictForReviewer(sha, prior.Reviewer)
	if err != nil || !found || afterFirst.Task != "FAC-798" {
		t.Fatalf("first projection: row=%+v found=%v err=%v", afterFirst, found, err)
	}
	if got, want := VerdictEventDigest(afterFirst), VerdictEventDigest(before); got != want {
		t.Fatalf("raw verdict digest changed after first binding: got=%s want=%s", got, want)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	second := TaskBindingOpts{SHA: sha, Reviewer: prior.Reviewer, PreviousTask: "FAC-798", Task: "FAC-799", PriorEventDigest: VerdictEventDigest(rows[1]), Artifact: "second.md", ArtifactDigest: strings.Repeat("2", 64)}
	if err := l.BindTask(second); err != nil {
		t.Fatalf("second chained binding: %v", err)
	}
	effective, found, err := l.VerdictForReviewer(sha, prior.Reviewer)
	if err != nil || !found || effective.Task != "FAC-799" {
		t.Fatalf("chained projection: row=%+v found=%v err=%v", effective, found, err)
	}
	if err := l.BindTask(second); err != nil {
		t.Fatalf("idempotent chained binding: %v", err)
	}
	conflict := second
	conflict.Task = "FAC-700"
	if err := l.BindTask(conflict); err == nil {
		t.Fatal("conflicting branch accepted")
	}
}
