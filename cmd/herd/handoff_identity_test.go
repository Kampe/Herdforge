package main

import (
	"strings"
	"testing"
)

// TestUnresolvedIdentityIsNotAnEmptyQueue is the FAC-570 regression.
//
// The packet said "--recipient <your-agent-name>", a value an agent cannot know:
// its pane carries HERDR_PANE_ID and HERD_ROLE and no agent-name variable. A live
// supervisor substituted its ROLE id, got an empty inbox, and concluded there was
// no work while the real recipient held two records.
//
// The failure mode is that an unresolvable identity and an empty queue look
// identical. Resolution must fail LOUDLY and say so.
func TestUnresolvedIdentityIsNotAnEmptyQueue(t *testing.T) {
	t.Setenv("HERD_AGENT_NAME", "")
	t.Setenv("HERDR_PANE_ID", "")

	_, err := resolveSelfAgentName()
	if err == nil {
		t.Fatal("no pane and no override must fail, never resolve to something empty")
	}
	// The message must distinguish this from an empty queue and name a remedy.
	for _, want := range []string{"HERDR_PANE_ID", "--recipient"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %q so the caller can act; got %v", want, err)
		}
	}
}

// An explicit override wins, so a coordinator can inspect another lane.
func TestExplicitAgentNameOverrideWins(t *testing.T) {
	t.Setenv("HERD_AGENT_NAME", "forge-review-harvest-su-467b70d7")
	t.Setenv("HERDR_PANE_ID", "wB:pXXX")
	got, err := resolveSelfAgentName()
	if err != nil || got != "forge-review-harvest-su-467b70d7" {
		t.Fatalf("explicit override must win: %q %v", got, err)
	}
}

// The guidance must not reintroduce the unresolvable placeholder.
func TestHelpDoesNotDemandAnUnknowableValue(t *testing.T) {
	help := subcommandUsage["handoffs"]
	if strings.Contains(help, "<your-agent-name>") {
		t.Fatal("help must not ask for a value an agent cannot know")
	}
}
