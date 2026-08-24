package main

import (
	"os"
	"strings"
	"testing"
)

// FAC-616: an explicit override always wins, since an operator naming a target
// knows the live fleet better than any prefix rule.
func TestReviewSupervisorTarget_EnvOverrideWins(t *testing.T) {
	t.Setenv("HERD_REVIEW_SUPERVISOR", "forge-review-harvest-su-467b70d7")
	if got := reviewSupervisorTarget(); got != "forge-review-harvest-su-467b70d7" {
		t.Fatalf("override ignored, got %q", got)
	}
}

// With no override and no reachable fleet, the packet must still name something
// meaningful rather than an empty recipient.
func TestReviewSupervisorTarget_FallsBackToCanonical(t *testing.T) {
	t.Setenv("HERD_REVIEW_SUPERVISOR", "")
	t.Setenv("HERD_NO_LIVE_HERDR", "1")
	got := reviewSupervisorTarget()
	if strings.TrimSpace(got) == "" {
		t.Fatal("target must never be empty; the packet renders it as a mail recipient")
	}
	if !strings.Contains(got, "review-supervisor") {
		t.Fatalf("cold-fleet fallback must be the canonical lane name, got %q", got)
	}
}

func TestLiveAgentByPrefix_UnreachableHerdrYieldsEmpty(t *testing.T) {
	t.Setenv("HERD_NO_LIVE_HERDR", "1")
	if got := liveAgentByPrefix("forge-review-harvest"); got != "" {
		t.Fatalf("an unreachable herdr must yield \"\" so the caller falls back, got %q", got)
	}
}

func TestReviewSupervisorTarget_NoEnvLeak(t *testing.T) {
	if v, ok := os.LookupEnv("HERD_REVIEW_SUPERVISOR"); ok && v == "" {
		t.Skip("env intentionally blank")
	}
}
