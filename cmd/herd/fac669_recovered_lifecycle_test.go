package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

const (
	fac669RejectedCandidate  = "63a61cd895b6bf11df7b704c1c27d163608bbd78"
	fac669RecoveredCandidate = "145c1bd9d4dc865371022fd3d74b75ea935b78a4"
)

// TestFAC669RecoveredDispatchCompletionRebindsLifecycleGeneration reproduces
// the production FAC-614 fence conflict: the rejected generation-1 candidate
// remains the canonical lifecycle candidate while a signed recovered dispatch
// issues generation 2. The recovered candidate's exact generation-2 callback
// must become admissible without rewriting the generation-1 event.
func TestFAC669RecoveredDispatchCompletionRebindsLifecycleGeneration(t *testing.T) {
	root := t.TempDir()
	if err := recordShotLifecycleLease(root, "FAC-614", 1, fac669RejectedCandidate); err != nil {
		t.Fatal(err)
	}
	if err := recordShotLifecycleLease(root, "FAC-614", 2, fac669RecoveredCandidate); err == nil ||
		!strings.Contains(err.Error(), "lifecycle lease generation 1 conflicts with reported 2") {
		t.Fatalf("exact production conflict was not reproduced: %v", err)
	}

	machine, err := lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	prior, err := machine.EventStore().CurrentState("FAC-614")
	if err != nil || prior == nil {
		machine.Close()
		t.Fatalf("generation-1 readback state=%+v err=%v", prior, err)
	}
	if _, err := machine.RebindGeneration(lifecycle.GenerationRebindRequest{
		Expected:         *prior,
		LeaseGeneration:  2,
		ProviderRevision: "fac-614-provider-generation-2",
		Actor:            "coordinator-dispatch",
		IdempotencyKey:   "dispatch-recovery:fac-614:lease:1:2",
		EvidenceDigest:   "sha256:fac-614-production-recovery-evidence",
		Payload:          `{"task_ref":"FAC-614","recovery_from_revision":1,"run_revision":2}`,
	}); err != nil {
		machine.Close()
		t.Fatal(err)
	}
	if err := machine.Close(); err != nil {
		t.Fatal(err)
	}

	if err := recordShotLifecycleLease(root, "FAC-614", 2, fac669RecoveredCandidate); err != nil {
		t.Fatalf("generation-2 completion remained fenced by generation 1: %v", err)
	}

	machine, err = lifecycle.NewMachine(filepath.Join(root, ".herd", "lifecycle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	events, err := machine.EventStore().Events("FAC-614")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].LeaseGeneration != 1 || events[0].CandidateSHA != fac669RejectedCandidate ||
		events[1].FromState != lifecycle.StateEligible || events[1].ToState != lifecycle.StateRecovering ||
		events[2].FromState != lifecycle.StateRecovering || events[2].ToState != lifecycle.StateEligible ||
		events[1].CandidateSHA != fac669RejectedCandidate || events[2].CandidateSHA != fac669RejectedCandidate {
		t.Fatalf("generation-1 lifecycle history was not preserved: %+v", events)
	}
	state, err := machine.EventStore().CurrentState("FAC-614")
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.LeaseGeneration != 2 || state.CandidateSHA != fac669RecoveredCandidate {
		t.Fatalf("recovered lifecycle state = %+v", state)
	}
}
