package provider

import (
	"strings"
	"testing"
)

// TestNoProductionMintAuthority pins the honest state.
//
// FAC-572: a consumer started a live fence broker correctly, exported the
// printed worker URL and token, and still could not close a card. The message
// said "coordinator uses FenceBrokerMinter", which reads as their
// misconfiguration. It was not: NO production path constructs a coordinator
// minter. Both constructors refuse by design, so a fenced Kaneo status write
// cannot complete in production for any card.
//
// This test exists so the gap cannot be quietly forgotten again -- the code
// deferred it to FAC-169 and nothing tracked it, which is why it surfaced only
// when a consumer hit it.
func TestNoProductionMintAuthority(t *testing.T) {
	if _, err := NewFenceBrokerMinterFromEnv(); err == nil {
		t.Fatal("env mint must stay disabled: a process-environment secret is forgeable")
	}
	// FromClaimDir is permitted under testing.Testing(), so its production
	// refusal cannot be asserted from a test. Assert the documented reason
	// instead, so a future change that silently enables it is visible here.
	_, err := NewFenceBrokerMinterFromClaimDir("", "")
	if err == nil {
		t.Fatal("an empty claim dir must be refused")
	}
}

// The refusal must not imply operator error, and must tell the operator not to
// retry.
func TestMissingMintErrorDoesNotBlameTheOperator(t *testing.T) {
	k := &KaneoProvider{}
	// Reach the message directly: constructing the full fenced path needs a
	// live broker, and the wording is what regressed.
	msg := missingMintCapabilityError().Error()
	for _, want := range []string{"NOTHING WAS WRITTEN", "FAC-169", "cannot complete"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message must contain %q; got %q", want, msg)
		}
	}
	if strings.Contains(msg, "coordinator uses FenceBrokerMinter") {
		t.Fatal("message must not imply the coordinator can mint today")
	}
	_ = k
}
