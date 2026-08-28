package main

import (
	"context"
	"strings"
	"testing"

	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

// FAC-629: `herd approve` reported failed=17 with "no usable launch receipt"
// for cards whose work had genuinely landed and passed review. The error
// named the wrong artifact -- the missing thing is dispatch task-context
// AUTHORIZATION, not a launch receipt, and it is not what evidences
// completion. A landing (completion) receipt is.
//
// This drives approveByCompletionReceipt -- the shipped fallback approveOne
// reaches when boundBoardProvider refuses -- and asserts it reaches the SAME
// fence-required refusal the FAC-563 override route reaches (proving it
// passed the completion-evidence gate), never the dispatch-authorization
// refusal it exists to route around.
func completionReceiptFixtureRoot(t *testing.T, root, ref string) {
	t.Helper()
	r := &hsync.CompletionReceipt{
		RepoID:             "herdforge",
		TaskRef:            ref,
		TaskID:             "task-2165",
		ProviderRevision:   "rev1",
		LeaseGeneration:    1,
		BaseSHA:            strings.Repeat("a", 40),
		CandidateSHA:       strings.Repeat("b", 40),
		MergeSHA:           strings.Repeat("c", 40),
		PatchID:            "patch1",
		AcceptanceDigest:   "digest1",
		VerificationDigest: "sha256:deadbeef",
		RiskTier:           "R1",
		AuthorFamily:       "anthropic",
		ReviewerFamily:     "openai",
		Verdict:            "PASS",
		IntegrationResult:  hsync.IntegrationMerged,
	}
	if err := hsync.WriteReceipt(root, r); err != nil {
		t.Fatalf("write completion receipt fixture: %v", err)
	}
}

func TestApproveFallsBackToCompletionReceiptWithoutDispatchAuthorization(t *testing.T) {
	cfg, mp, root := overrideFixture(t)
	completionReceiptFixtureRoot(t, root, "CHA-2165")

	_, err := approveByCompletionReceipt(context.Background(), cfg, mp, nil, root, "CHA-2165", "", "", nil)
	// No claim stack: must still refuse to write unfenced, same as the
	// override route (FAC-566) -- receipt evidence replaces the dispatch
	// AUTHORIZATION requirement, never the fence.
	if err == nil {
		t.Fatal("a completion-receipt close with no claim stack must refuse to write unfenced")
	}
	if !strings.Contains(err.Error(), "fence") && !strings.Contains(err.Error(), "FAC-147") {
		t.Fatalf("refusal must name the fence requirement, got %v", err)
	}
	if strings.Contains(err.Error(), "dispatch task-context") || strings.Contains(err.Error(), "launch receipt") {
		t.Fatalf("a valid completion receipt must not be refused for missing dispatch authorization: %v", err)
	}
}

// Neither artifact existing must refuse, and name BOTH classes of evidence
// that were checked and found missing -- not just the first one tried.
func TestApproveRefusesWithNeitherDispatchAuthorizationNorCompletionReceipt(t *testing.T) {
	cfg, mp, root := overrideFixture(t)

	_, err := approveByCompletionReceipt(context.Background(), cfg, mp, nil, root, "CHA-2165", "", "",
		errNoDispatchAuthorizationFixture)
	if err == nil {
		t.Fatal("no dispatch authorization and no completion receipt must refuse")
	}
	if !strings.Contains(err.Error(), "no completion receipt") {
		t.Fatalf("refusal must name the missing completion receipt, got %v", err)
	}
	if !strings.Contains(err.Error(), "no dispatch authorization") {
		t.Fatalf("refusal must still surface why the dispatch-authorization path failed, got %v", err)
	}
}

var errNoDispatchAuthorizationFixture = fixtureErr("no dispatch authorization: fixture")

type fixtureErr string

func (e fixtureErr) Error() string { return string(e) }
