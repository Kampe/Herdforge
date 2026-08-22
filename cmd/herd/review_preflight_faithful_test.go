package main

import (
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/security"
)

// TestPreflightGatesOnWorkerBrokerability is the FAC-579 gate.
//
// The previous preflight ran a real generation as a child of THIS process and
// reported Available; the pane launch then failed with "pane is at a login or
// authentication screen". Both were true — an in-process probe proves the
// coordinator's credential context, not the pane's. A preflight that cannot
// observe the boundary it claims to check is worse than none: it converts an
// honest failure into a false pass.
func TestPreflightGatesOnWorkerBrokerability(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := funcBody(string(src), "func preflightReviewerReadiness")
	if !ok {
		t.Fatal("cannot locate preflightReviewerReadiness")
	}
	if !strings.Contains(body, "DiagnoseKindAuthReadiness") {
		t.Error("preflight must gate on worker credential brokerability, the boundary the launch hits")
	}
	// The brokerability gate must come FIRST: an in-process probe must never be
	// able to stand in for it.
	gate := strings.Index(body, "DiagnoseKindAuthReadiness")
	probe := strings.Index(body, "ProbeProviderModel")
	if probe >= 0 && gate > probe {
		t.Error("brokerability must be checked before the in-process probe, never after")
	}
	if !strings.Contains(body, "hostcreds diagnose") {
		t.Error("the refusal should point at the matching diagnostic command")
	}
}

// A kind that is not brokerable must be refused with the real blocker and an
// action, before any lease or tab.
func TestUnbrokerableKindIsRefusedWithAction(t *testing.T) {
	// opencode has no HostCreds mapping on any host, so this is stable.
	d := security.DiagnoseKindAuthReadiness("opencode")
	if d.Brokerable {
		t.Skip("fixture assumption changed: opencode became brokerable")
	}
	err := preflightReviewerReadiness(poolReviewer{
		Kind: "opencode", Provider: "opencode", Model: "opencode/deepseek-v4-pro",
	})
	if err == nil {
		t.Fatal("a kind whose worker credentials cannot be brokered must be refused")
	}
	msg := err.Error()
	for _, want := range []string{d.Blocker, "No lease or tab was created", "hostcreds diagnose"} {
		if want != "" && !strings.Contains(msg, want) {
			t.Errorf("refusal must include %q, got: %v", want, msg)
		}
	}
	// It must say plainly why an interactive login is not enough, since that is
	// the exact confusion this defect produced.
	if !strings.Contains(msg, "different credential context") {
		t.Error("refusal should explain that the pane runs in a different credential context")
	}
}
