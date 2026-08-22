package main

import (
	"os"
	"strings"
	"testing"
)

// TestReviewerReadinessIsProvedBeforeLease is the FAC-577 ordering gate.
//
// A routed claude reviewer launched into a pane sitting at an authentication
// screen. Quota was healthy and the host CLI reported an interactive login, but
// neither is worker credential readiness — and the failure surfaced only AFTER
// the warm-pool lease and the tab existed, so both had to be compensated.
func TestReviewerReadinessIsProvedBeforeLease(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func runPoolReview")
	if !ok {
		t.Fatal("cannot locate runPoolReview")
	}
	resolve := strings.Index(body, "resolvePoolReviewer(*provider")
	preflight := strings.Index(body, "preflightReviewerReadiness(")
	lease := strings.Index(body, "p.Lease(")
	create := strings.Index(body, "herdr.TabCreate(")
	for name, idx := range map[string]int{
		"resolvePoolReviewer": resolve, "preflightReviewerReadiness": preflight,
		"p.Lease": lease, "herdr.TabCreate": create,
	} {
		if idx < 0 {
			t.Fatalf("expected %s in the launch path", name)
		}
	}
	if resolve > lease || preflight > lease {
		t.Error("reviewer must be resolved and preflighted before the pool lease is taken")
	}
	if lease > create {
		t.Error("fixture assumption changed: the lease is no longer taken before the tab")
	}
}

// A probe that reports unavailable must refuse, carrying the provider, the
// model, and the reason — an operator needs to know which surface to fix.
func TestPreflightRefusesUnreadyReviewer(t *testing.T) {
	// An unroutable provider cannot produce a probe command, so the probe fails
	// closed without needing a live CLI.
	err := preflightReviewerReadiness(poolReviewer{
		Kind: "nope", Provider: "definitely-not-a-provider", Model: "no-such-model",
	})
	if err == nil {
		t.Fatal("an unready reviewer must be refused")
	}
	for _, want := range []string{"definitely-not-a-provider", "no-such-model", "No lease or tab was created"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must mention %q, got: %v", want, err)
		}
	}
}

// The probe must be a REAL request, not a credential-presence check: a host CLI
// that reports loggedIn=true while its pane opens at an auth screen is exactly
// the case that got through.
func TestPreflightUsesARealGenerationProbe(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func preflightReviewerReadiness")
	if !ok {
		t.Fatal("cannot locate preflightReviewerReadiness")
	}
	if !strings.Contains(body, "herdr.ProbeProviderModel") {
		t.Error("preflight must run a real generation probe, not an auth-status check")
	}
	if strings.Contains(body, "auth status") || strings.Contains(body, "LookPath") {
		t.Error("credential presence is not readiness; probe must execute a request")
	}
}
