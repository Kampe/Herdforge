package verifier

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// completionRepo builds a repo with origin/main plus a worktree branch whose
// commits are controllable per test.
func completionRepo(t *testing.T, subjects ...string) string {
	key := "completion\x00" + strings.Join(subjects, "\x00")
	dir, _ := copyCachedRepo(t, key, buildCompletionFixture(subjects))
	return dir
}

func TestCheckCompletion_RealWorkPasses(t *testing.T) {
	dir := completionRepo(t, "feat: real work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "true", "true")
	if !c.Passed || !c.HasCommits {
		t.Fatalf("real work must pass: %+v", c)
	}
}

func TestCheckCompletion_OnlyAnchorFails(t *testing.T) {
	dir := completionRepo(t, "chore: anchor FAC-1 worktree (FAC-106 reap-safe)", "wip: partial")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "true", "true")
	if c.Passed || c.HasCommits {
		t.Fatalf("anchor+wip only must fail the gate: %+v", c)
	}
	if len(c.Reasons) == 0 || !strings.Contains(c.Reasons[0], "no real commits") {
		t.Fatalf("must explain the whiff: %+v", c.Reasons)
	}
}

func TestCheckCompletion_BuildFailFails(t *testing.T) {
	dir := completionRepo(t, "feat: work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "false", "true")
	if c.Passed || c.Builds {
		t.Fatalf("build failure must fail the gate: %+v", c)
	}
}

func TestCheckCompletion_TestFailFails(t *testing.T) {
	dir := completionRepo(t, "feat: work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "true", "false")
	if c.Passed || c.TestsPass {
		t.Fatalf("test failure must fail the gate: %+v", c)
	}
}

func TestCheckCompletion_EmptyCmdsFailClosed(t *testing.T) {
	dir := completionRepo(t, "feat: work (FAC-1)")
	v := NewVerifier("")
	c := v.CheckCompletion(context.Background(), dir, "", "")
	if c.Passed || c.Builds || c.TestsPass {
		t.Fatalf("empty build/test commands must fail closed: %+v", c)
	}
}

func TestCheckCompletionAndPersist_WritesAdmissibleBuildAndTestReceipts(t *testing.T) {
	dir := completionRepo(t, "feat: receipt-backed work (FAC-342)")
	candidate, err := currentSHA(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileReceiptStore(filepath.Join(t.TempDir(), "verification-receipts"))
	if err != nil {
		t.Fatal(err)
	}
	c, receipts, err := NewVerifier("").CheckCompletionAndPersist(context.Background(), dir, "true", "true", VerificationRequest{
		TaskRef: "FAC-342", LeaseGeneration: "2", CandidateSHA: candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Passed || receipts.Build == nil || receipts.Test == nil {
		t.Fatalf("completion=%+v receipts=%+v", c, receipts)
	}
	admission := NewReceiptAdmission(store)
	for _, receipt := range []*Receipt{receipts.Build, receipts.Test} {
		if _, err := admission.RequireCurrentPassing(context.Background(), dir, receipt.Digest); err != nil {
			t.Fatalf("receipt %s was not admissible: %v", receipt.Digest, err)
		}
	}
}
