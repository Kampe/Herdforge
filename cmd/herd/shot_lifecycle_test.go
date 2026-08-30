package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

const shotLifecycleTestSHA = "53f868c967787692799924f9a37370983625fe28"

func TestRecordShotLifecycleLeaseCreatesAndRetriesIdempotently(t *testing.T) {
	root := t.TempDir()
	if err := recordShotLifecycleLease(root, "FAC-305", 1, shotLifecycleTestSHA); err != nil {
		t.Fatal(err)
	}
	if err := recordShotLifecycleLease(root, "FAC-305", 1, shotLifecycleTestSHA); err != nil {
		t.Fatal(err)
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	state, err := machine.EventStore().CurrentState("FAC-305")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.LeaseGeneration != 1 || state.CandidateSHA != shotLifecycleTestSHA {
		t.Fatalf("lifecycle state = %+v", state)
	}
}

func TestRecordShotLifecycleLeaseDrivesRecoveringSupersessionPath(t *testing.T) {
	root := t.TempDir()
	oldSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := recordShotLifecycleLease(root, "FAC-662", 1, shotLifecycleTestSHA); err != nil {
		t.Fatal(err)
	}
	if err := recordShotLifecycleLease(root, "FAC-662", 1, oldSHA); err != nil {
		t.Fatal(err)
	}
	original := runShotCandidateSupersession
	t.Cleanup(func() { runShotCandidateSupersession = original })
	called := false
	runShotCandidateSupersession = func(_ context.Context, gotRoot, gotRef string, gotLease int64, gotSHA string, _ *lifecycle.Machine, current *lifecycle.TaskState) error {
		called = true
		if gotRoot != root || gotRef != "FAC-662" || gotLease != 1 || gotSHA != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
			t.Fatalf("supersession callback args root=%q ref=%q lease=%d sha=%q", gotRoot, gotRef, gotLease, gotSHA)
		}
		if current == nil || current.State != lifecycle.StateRecovering || current.CandidateSHA != oldSHA {
			t.Fatalf("supersession current state = %+v", current)
		}
		return nil
	}
	if err := recordShotLifecycleLease(root, "FAC-662", 1, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("Recovering callback did not drive the shipped supersession path")
	}
}

func TestRecordShotLifecycleLeaseRejectsStaleOrConflictingEvidence(t *testing.T) {
	root := t.TempDir()
	if err := recordShotLifecycleLease(root, "FAC-305", 1, shotLifecycleTestSHA); err != nil {
		t.Fatal(err)
	}
	if err := recordShotLifecycleLease(root, "FAC-305", 2, shotLifecycleTestSHA); err == nil {
		t.Fatal("stale/conflicting lease generation was accepted")
	}
	if err := recordShotLifecycleLease(root, "FAC-305", 1, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal("eligible candidate recovery was rejected:", err)
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	state, err := machine.EventStore().CurrentState("FAC-305")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.State != lifecycle.StateRecovering || state.CandidateSHA != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("recovered lifecycle state = %+v", state)
	}
	if err := machine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recordShotLifecycleLease(root, "FAC-305", 1, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err == nil {
		t.Fatal("conflicting candidate SHA after recovery was accepted")
	}
}
