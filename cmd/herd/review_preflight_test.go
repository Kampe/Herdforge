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

// FAC-579 corrected this test's premise.
//
// It used to require that the preflight "execute a request" and forbid any
// mention of an auth check, on the belief that a real generation was the
// strongest available evidence. That belief was wrong: the generation ran in
// THIS process, so it proved the coordinator's credential context while the
// reviewer runs in a pane with a different one. The probe passed and the launch
// then failed at an authentication screen.
//
// What must hold is that the preflight checks the boundary the LAUNCH hits, and
// that an in-process signal can never substitute for it.
func TestPreflightChecksTheBoundaryTheLaunchHits(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func preflightReviewerReadiness")
	if !ok {
		t.Fatal("cannot locate preflightReviewerReadiness")
	}
	if !strings.Contains(body, "DiagnoseKindAuthReadiness") {
		t.Error("preflight must gate on worker credential brokerability")
	}
	if !strings.Contains(body, "Brokerable") {
		t.Error("preflight must act on the brokerability verdict, not merely consult it")
	}
	// An in-process probe may remain as a secondary quota/model signal, but the
	// brokerability gate must be able to refuse on its own.
	gate := strings.Index(body, "!auth.Brokerable")
	if gate < 0 {
		t.Error("brokerability must be a refusal on its own, not advisory")
	}
}
