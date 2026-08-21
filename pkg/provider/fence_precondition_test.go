package provider

import (
	"errors"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// TestMissingBrokerIsAPreconditionNotAmbiguity is the FAC-571 regression.
//
// The missing-fence-broker refusal was wrapped in ErrProviderAmbiguous, which
// tells an operator the write MAY have landed and must be reconciled before any
// retry. It had NOT landed: the check runs before any remote or readback side
// effect. A correct operator read that message, refused to retry an ambiguous
// mutation, and escalated -- exactly the right call given wrong information.
//
// The distinction is load-bearing, so it is pinned here rather than left to a
// comment.
func TestMissingBrokerIsAPreconditionNotAmbiguity(t *testing.T) {
	if errors.Is(claim.ErrFenceInfrastructure, claim.ErrProviderAmbiguous) {
		t.Fatal("a precondition refusal must not classify as an ambiguous outcome")
	}
	msg := claim.ErrFenceInfrastructure.Error()
	if !strings.Contains(msg, "before any provider call") {
		t.Fatalf("the error must state that nothing was attempted, got %q", msg)
	}
	if strings.Contains(msg, "reconcile") {
		t.Fatalf("a precondition failure must not demand reconciliation, got %q", msg)
	}
}

// The refusal must tell an operator how to proceed. It previously named the
// broker but not how to start it, so the next step was a source dive.
func TestFenceRefusalNamesTheStartPath(t *testing.T) {
	k := &KaneoProvider{RequireCASMeta: true}
	err := k.guardFencedStatus("done", "op-1", true)
	if err == nil {
		t.Fatal("a fenced status write with no broker must refuse")
	}
	if !errors.Is(err, claim.ErrFenceInfrastructure) {
		t.Fatalf("refusal must classify as a precondition failure, got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"NOTHING WAS WRITTEN", "herd fence-broker", "HERD_FENCE_BROKER_URL",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal must contain %q so the operator can act; got %q", want, msg)
		}
	}
}
