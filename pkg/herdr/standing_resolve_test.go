package herdr

import (
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/standing"
)

func TestResolveStandingLaneMatchesTruncatedDigestForm(t *testing.T) {
	lane := "review-harvest-supervisor"
	live := standing.AgentNameForRepository(lane, "github.com/Kampe/Herdforge")
	agents := []AgentEntry{
		{Name: live, PaneID: "wB:p1", Status: "working"},
		{Name: "forge-other-lane-aaaaaaaaaa", PaneID: "wB:p2", Status: "idle"},
	}
	got, err := ResolveStandingLaneMatches(lane, agents, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != live {
		t.Fatalf("got=%v want [%s]", got, live)
	}
}

func TestResolveStandingLaneMatchesAmbiguousPrefixRefusesWithCandidates(t *testing.T) {
	lane := "review-harvest-supervisor"
	p := standingNamePrefix(standing.AgentNameForRepository(lane, "repo-a"))
	agents := []AgentEntry{
		{Name: p + "-aaaaaaaa", PaneID: "p1"},
		{Name: p + "-bbbbbbbb", PaneID: "p2"},
	}
	_, err := ResolveStandingLaneMatches(lane, agents, "")
	if err == nil {
		t.Fatal("expected ambiguous error")
	}
	var amb *AmbiguousStandingTargetError
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err=%v", err)
	}
	if e, ok := err.(*AmbiguousStandingTargetError); ok {
		amb = e
	}
	if amb == nil {
		// wrapped? still require candidates named
		if !strings.Contains(err.Error(), p+"-aaaaaaaa") || !strings.Contains(err.Error(), p+"-bbbbbbbb") {
			t.Fatalf("refusal must name candidates: %v", err)
		}
		return
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("candidates=%v", amb.Candidates)
	}
}

func TestRequireAgentInWorkspaceUsesStandingResolveNotNaiveForgePrefix(t *testing.T) {
	lane := "review-harvest-supervisor"
	live := standing.AgentNameForRepository(lane, "github.com/Kampe/Herdforge")
	// Only the digest form is live — the shared resolver must find it without
	// relying on the removed "forge-"+label concatenation.
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[
				{"name":"` + live + `","pane_id":"wB:p1","workspace_id":"wB","agent_status":"idle"}
			]}}`, nil
		}
		return "{}", nil
	})
	t.Cleanup(restore)
	got, err := requireAgentInWorkspace(lane, "wB")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != live {
		t.Fatalf("resolved=%q want digest form %q", got.Name, live)
	}
	if got.Name == "forge-"+lane {
		t.Fatal("must not resolve via naive forge-+label alone when digest form is the live name")
	}
}

func TestRequireAgentInWorkspaceNamesLiveCensusOnMiss(t *testing.T) {
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[
				{"name":"forge-other-aaaaaaaaaa","pane_id":"wB:p1","workspace_id":"wB","agent_status":"idle"}
			]}}`, nil
		}
		return "{}", nil
	})
	t.Cleanup(restore)
	_, err := requireAgentInWorkspace("review-harvest-supervisor", "wB")
	if err == nil {
		t.Fatal("expected miss")
	}
	if !strings.Contains(err.Error(), "live now:") || !strings.Contains(err.Error(), "forge-other-aaaaaaaaaa") {
		t.Fatalf("refusal must name the live census: %v", err)
	}
}

// Source proof: send.go must not contain the naive concatenation FAC-617 removes.
func TestSendGoHasNoNaiveForgeConcatenation(t *testing.T) {
	body, err := os.ReadFile("send.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"forge-" + target`) || strings.Contains(string(body), `"forge-"+target`) {
		t.Fatal(`send.go still contains naive "forge-"+target concatenation; route through ResolveStandingLaneMatches`)
	}
}
