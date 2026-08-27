package main

import (
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/standing"
)

func TestMatchTruncatedStandingNameFindsLiveDigestForm(t *testing.T) {
	lane := "review-harvest-supervisor"
	repo := "github.com/Kampe/Herdforge"
	live := standing.AgentNameForRepository(lane, repo)
	agents := []herdr.AgentEntry{
		{Name: live, PaneID: "wB:p1", Status: "working"},
		{Name: "forge-other-lane-aaaaaaaaaa", PaneID: "wB:p2", Status: "idle"},
	}
	got := matchTruncatedStandingName(lane, agents)
	if got != live {
		t.Fatalf("matchTruncatedStandingName=%q want %q", got, live)
	}
}

func TestMatchTruncatedStandingNameAmbiguousRefuses(t *testing.T) {
	lane := "review-harvest-supervisor"
	// Two agents sharing the same truncated prefix (different digests) — refuse.
	p := standingNamePrefix(standing.AgentNameForRepository(lane, "repo-a"))
	agents := []herdr.AgentEntry{
		{Name: p + "-aaaaaaaa", PaneID: "p1"},
		{Name: p + "-bbbbbbbb", PaneID: "p2"},
	}
	if got := matchTruncatedStandingName(lane, agents); got != "" {
		t.Fatalf("ambiguous must return empty, got %q", got)
	}
}

func TestResolveSendTargetLeavesUnknownBareRoleUnchanged(t *testing.T) {
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		return "{}", nil
	})
	t.Cleanup(restore)
	got := resolveSendTarget("totally-missing-standing-lane")
	if got != "totally-missing-standing-lane" {
		t.Fatalf("got %q", got)
	}
}

// FAC-617: the perf-cost-guard failure — bare standing role must resolve to the
// live truncated forge-* digest name through resolveSendTarget. Chdir to an
// empty tree so AuthenticatedRepositoryIdentity cannot short-circuit via
// LiveAgentName; the truncated-prefix match is what must fire.
func TestResolveSendTargetUsesLiveTruncatedForgeName(t *testing.T) {
	lane := "review-harvest-supervisor"
	live := standing.AgentNameForRepository(lane, "github.com/Kampe/Herdforge")
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"` + live + `","pane_id":"wB:p1","agent_status":"working"}]}}`, nil
		}
		return "{}", nil
	})
	t.Cleanup(restore)
	tmp := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	got := resolveSendTarget(lane)
	if got != live {
		t.Fatalf("resolveSendTarget(%q)=%q want %q", lane, got, live)
	}
}
