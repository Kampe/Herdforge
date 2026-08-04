package verifier

import (
	"context"
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

func TestMutationDiskDenialPrecedesGitWriteAndProcess(t *testing.T) {
	dir := t.TempDir()
	called := 0
	v := NewVerifierArgs([]string{"definitely-not-started"})
	v.DiskAdmission = resources.DiskAdmissionFunc(func(resources.DiskRequest) resources.DiskDecision {
		called++
		return resources.DiskDecision{State: resources.DiskBlocked, Evidence: resources.DiskEvidence{Reason: resources.DiskReasonBelowThreshold}}
	})
	_, err := v.RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
		CandidateSHA: "not-read-before-gate",
		TargetFile:   "candidate.txt",
		OriginalCode: "before",
		MutantCode:   "after",
	})
	if err == nil {
		t.Fatal("expected disk denial")
	}
	if called != 1 {
		t.Fatalf("admission calls = %d, want one", called)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
		t.Fatalf("mutation created files before denial: entries=%v err=%v", entries, readErr)
	}
}
