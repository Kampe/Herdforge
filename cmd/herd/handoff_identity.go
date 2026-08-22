package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// resolveSelfAgentName determines the caller's own exact agent name.
//
// FAC-570: the packet told a supervisor to run
// "handoffs list --recipient <your-agent-name>", and an agent has no way to know
// that value. Its pane environment carries HERDR_PANE_ID and HERD_ROLE and no
// agent-name variable at all. A live supervisor substituted its ROLE id
// (review-harvest-supervisor), got an EMPTY inbox, and concluded there was no
// work -- while the real recipient forge-review-harvest-su-467b70d7 held two
// records.
//
// That is the dangerous shape: an empty queue and an unresolvable identity look
// identical. So resolution is done here, once, from the pane the caller is
// actually in -- and a failure to resolve is LOUD rather than an empty list.
//
// Deliberately not solved by a prompt-side jq lookup: that would be a second
// identity derivation living in a document, free to drift from this one.
func resolveSelfAgentName() (string, error) {
	// An explicit override wins, for a coordinator inspecting another lane.
	if v := strings.TrimSpace(os.Getenv("HERD_AGENT_NAME")); v != "" {
		return v, nil
	}
	pane := strings.TrimSpace(os.Getenv("HERDR_PANE_ID"))
	if pane == "" {
		return "", fmt.Errorf("cannot resolve your agent name: HERDR_PANE_ID is not set, so this " +
			"process is not running inside a herdr pane. Pass --recipient <exact-agent-name> " +
			"explicitly, or set HERD_AGENT_NAME")
	}
	agents, err := herdr.AgentList()
	if err != nil {
		return "", fmt.Errorf("cannot resolve your agent name from pane %s: %w", pane, err)
	}
	for _, a := range agents {
		if strings.TrimSpace(a.PaneID) == pane && strings.TrimSpace(a.Name) != "" {
			return a.Name, nil
		}
	}
	// Fail closed and say why. Returning an empty queue here is exactly the bug:
	// it reads as "nothing to do".
	return "", fmt.Errorf("cannot resolve your agent name: no live agent is bound to pane %s. "+
		"This is NOT the same as an empty queue; pass --recipient <exact-agent-name> if you know it", pane)
}
