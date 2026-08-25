package main

import (
	"os"
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

// FAC-617 deliberately replaced FAC-616's canonical fallback. An unreachable
// supervisor now yields "" and that emptiness is MEANINGFUL: it is the signal
// reviewPacketBody uses to render the git verdicts branch as the primary report
// home. Returning a canonical name here would print a mail command that cannot
// work, because herd mail send writes a file in the local checkout and mail does
// not cross hosts -- which is the bug FAC-617 exists to fix.
//
// This test previously asserted the opposite ("target must never be empty") and
// was correct for exactly one PR.
func TestReviewSupervisorTarget_UnreachableFleetYieldsEmpty(t *testing.T) {
	t.Setenv("HERD_REVIEW_SUPERVISOR", "")
	t.Setenv("HERD_NO_LIVE_HERDR", "1")
	if got := reviewSupervisorTarget(); got != "" {
		t.Fatalf("an unreachable fleet must yield \"\" so the packet selects the branch transport, got %q", got)
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

// The production case on the review host: herdr IS reachable, but the supervisor
// runs on the other machine so no local agent matches. This is distinct from an
// unreachable fleet and it is the normal state on WSL -- FAC-616 fell back to the
// canonical name here, which is precisely the dead-letterbox address.
func TestReviewSupervisorTarget_ReachableFleetWithoutSupervisorYieldsEmpty(t *testing.T) {
	t.Setenv("HERD_REVIEW_SUPERVISOR", "")
	// A fake herdr that lists agents, none of them a supervisor.
	dir := t.TempDir()
	fake := dir + "/herdr"
	script := "#!/bin/sh\necho '{\"result\":{\"agents\":[{\"name\":\"review-cha-1-abc\",\"agent_status\":\"working\"}]}}'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_HERDR_BIN", fake)
	t.Setenv("HERD_NO_LIVE_HERDR", "")

	if got := reviewSupervisorTarget(); got != "" {
		t.Fatalf("a reachable fleet with no supervisor lane must yield \"\", got %q — that is the dead-letterbox address", got)
	}
}
