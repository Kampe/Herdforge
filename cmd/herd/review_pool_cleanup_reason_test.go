package main

import "testing"

func TestReviewCleanupReasonRetainsNativeStartupFailure(t *testing.T) {
	const startup = `start codex reviewer: herdr agent start: {"error":{"code":"agent_not_ready","message":"blocked during startup"}}`

	if got := reviewCleanupReason(startup, ""); got != "launch failure: "+startup {
		t.Fatalf("failure receipt reason = %q, want native startup diagnostic", got)
	}
	got := reviewCleanupReason(startup, "exact tab cleanup completed")
	want := "launch failure: " + startup + "; exact tab cleanup completed"
	if got != want {
		t.Fatalf("cleanup receipt reason = %q, want %q", got, want)
	}
}

func TestReviewCleanupReasonKeepsSuccessfulCleanupReason(t *testing.T) {
	if got := reviewCleanupReason("", "exact tab cleanup completed"); got != "exact tab cleanup completed" {
		t.Fatalf("successful cleanup reason = %q", got)
	}
}
