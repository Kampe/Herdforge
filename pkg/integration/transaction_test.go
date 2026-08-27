package integration

import (
	"strings"
	"testing"
)

const cand = "aae52d4386b9f475035ac23bdead0215e9974db7"

func drive(t *testing.T, upTo Step) *Transaction {
	t.Helper()
	tx, err := New(cand)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range Order {
		if err := tx.Complete(s, "evidence for "+string(s)); err != nil {
			t.Fatalf("driving to %s failed at %s: %v", upTo, s, err)
		}
		if s == upTo {
			break
		}
	}
	return tx
}

// THE edge this package exists for. Cleanup destroys the source worktree and
// branch; patch-identity proof is what establishes the merged content IS the
// reviewed content. Running cleanup first destroys the only copy of work that
// may never have landed.
func TestCleanupWithoutPatchProofIsRefused(t *testing.T) {
	tx := drive(t, StepRuntimeBind)
	// Skip the proof and go straight for the destructive step.
	err := tx.Complete(StepCleanup, "removed worktree")
	if err == nil {
		t.Fatal("cleanup ran without patch-identity proof")
	}
	if !strings.Contains(err.Error(), "only copy of work that may never have landed") {
		t.Fatalf("refusal does not name the actual risk: %v", err)
	}
}

func TestCleanupIsPermittedOnceProven(t *testing.T) {
	tx := drive(t, StepPatchProof)
	if err := tx.Complete(StepCleanup, "worktree and branch retired; absence verified"); err != nil {
		t.Fatalf("cleanup refused after proof: %v", err)
	}
	if _, more := tx.Next(); more {
		t.Fatal("transaction did not complete after cleanup")
	}
}

func TestOutOfOrderStepIsRefused(t *testing.T) {
	// A merge recorded before a harvest describes a candidate that was never
	// rebased onto current trunk.
	tx, err := New(cand)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Complete(StepPass, "reviewed PASS"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Complete(StepMerge, "merged"); err == nil {
		t.Fatal("merge was accepted before harvest")
	}
}

func TestStepWithoutEvidenceIsRefused(t *testing.T) {
	// A step that cannot be checked later is a claim, not a receipt. This whole
	// session turned on the difference.
	tx, err := New(cand)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Complete(StepPass, "   "); err == nil {
		t.Fatal("a step with no evidence was recorded")
	}
}

func TestInexactCandidateIsRefused(t *testing.T) {
	// A transaction keyed on a short ref cannot prove which content it landed.
	if _, err := New("abc123"); err == nil {
		t.Fatal("an inexact candidate started a transaction")
	}
}

func TestDuplicateStepIsRefused(t *testing.T) {
	tx := drive(t, StepPass)
	if err := tx.Complete(StepPass, "again"); err == nil {
		t.Fatal("a step was recorded twice")
	}
}

func TestBlockedNamesWhereItStopped(t *testing.T) {
	tx := drive(t, StepMerge)
	msg := tx.Blocked("runtime bind refused: image pin mismatch")
	for _, want := range []string{"runtime-bind", "4/7", "image pin mismatch"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("blocked report omits %q: %s", want, msg)
		}
	}
}

func TestCleanupIsLastInTheContract(t *testing.T) {
	// Pinned as a contract, not an implementation detail: if cleanup ever stops
	// being last, the refusal above protects nothing.
	if Order[len(Order)-1] != StepCleanup {
		t.Fatalf("cleanup is no longer the final step: %v", Order)
	}
	if Order[len(Order)-2] != StepPatchProof {
		t.Fatalf("patch proof no longer immediately precedes cleanup: %v", Order)
	}
}
