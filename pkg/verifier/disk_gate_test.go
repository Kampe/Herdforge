package verifier

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

func TestMutationDiskDenialPrecedesGitWriteAndProcess(t *testing.T) {
	dir := t.TempDir()
	called := 0
	var seen resources.DiskRequest
	v := NewVerifierArgs([]string{"definitely-not-started"})
	v.DiskAdmission = resources.DiskAdmissionFunc(func(req resources.DiskRequest) resources.DiskDecision {
		called++
		seen = req
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
	if seen.Path == "" || seen.TempPath == "" {
		t.Fatalf("admission must resolve candidate and temp volumes: %+v", seen)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
		t.Fatalf("mutation created files before denial: entries=%v err=%v", entries, readErr)
	}
}

// Compat entry must admit disk before currentSHA/git. A denied volume must not
// fork git on an already-unsafe path (regression against RunMutationCheck
// resolving HEAD first).
func TestRunMutationCheckDiskDenialPrecedesGit(t *testing.T) {
	dir := t.TempDir()
	// No .git here — if disk gate fails open, currentSHA would error differently
	// or leave git residue. Denial must win first.
	called := 0
	v := NewVerifierArgs([]string{"definitely-not-started"})
	v.DiskAdmission = resources.DiskAdmissionFunc(func(req resources.DiskRequest) resources.DiskDecision {
		called++
		if req.TempPath == "" {
			t.Fatal("compat path must pass TempPath for temp-volume capacity")
		}
		return resources.DiskDecision{State: resources.DiskBlocked, Evidence: resources.DiskEvidence{Reason: resources.DiskReasonBelowThreshold}}
	})
	_, err := v.RunMutationCheck(context.Background(), dir, "candidate.txt", "before", "after")
	if err == nil {
		t.Fatal("expected disk denial")
	}
	if called != 1 {
		t.Fatalf("admission calls = %d, want one", called)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(statErr) {
		t.Fatalf("compat path inspected/created git state under denied volume: %v", statErr)
	}
}
