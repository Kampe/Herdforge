package main

import (
	"os"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// FAC-617: end-to-end through sendWithResolvedTarget (the seam runSend uses).
// Mocked AgentList returns a live truncated forge-* name; assert that name is
// what herdr.Send prompts — not the bare standing role.
func TestSendWithResolvedTargetPromptsLiveForgeName(t *testing.T) {
	lane := "review-harvest-supervisor"
	live := standing.AgentNameForRepository(lane, "github.com/Kampe/Herdforge")
	var prompted []string
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"` + live + `","pane_id":"wB:p1","workspace_id":"wB","agent_status":"idle"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = append(prompted, args[2])
		}
		return "{}", nil
	})
	t.Cleanup(restore)
	t.Setenv("HERD_WORKSPACE", "wB")
	tmp := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	resolved, status, err := sendWithResolvedTarget(lane, "ping", false, time.Second, "wB")
	if err != nil {
		t.Fatalf("sendWithResolvedTarget: %v", err)
	}
	if status != "submitted" {
		t.Fatalf("status=%q", status)
	}
	if resolved != live {
		t.Fatalf("resolved=%q want %q", resolved, live)
	}
	if len(prompted) != 1 || prompted[0] != live {
		t.Fatalf("AgentPrompt targets=%v want [%s]", prompted, live)
	}
}

func TestResolveSendTargetLeavesUnknownBareRoleUnchanged(t *testing.T) {
	restore := herdr.SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"forge-other-aaaaaaaaaa","pane_id":"p1","agent_status":"idle"}]}}`, nil
		}
		return "{}", nil
	})
	t.Cleanup(restore)
	got := resolveSendTarget("totally-missing-standing-lane")
	if got != "totally-missing-standing-lane" {
		t.Fatalf("got %q", got)
	}
}
