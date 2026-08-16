package main

import (
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
