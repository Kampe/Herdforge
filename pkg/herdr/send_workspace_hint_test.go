package herdr

import (
	"strings"
	"testing"
)

// TestCrossWorkspaceRefusalNamesTheAuthorizedPath is the FAC-554 regression.
//
// A peer coordinator ran `herdforge send wK:p2 --file report.md` from a repo
// registered to wB, got "refusing cross-workspace delivery", and reported it as
// contradicting the documented authorization. Cross-workspace delivery IS
// supported -- via an explicit --workspace -- but the refusal never said so, so
// the guard read as a broken feature and blocked the feedback channel itself.
func TestCrossWorkspaceRefusalNamesTheAuthorizedPath(t *testing.T) {
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) > 1 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"type":"agents","agents":[
				{"name":"peer-coordinator","agent_status":"idle","pane_id":"wK:p2","tab_id":"wK:t2","workspace_id":"wK","cwd":"/tmp"}
			]}}`, nil
		}
		return `{"result":{"type":"ok"}}`, nil
	})
	defer restore()

	_, err := requireAgentInWorkspace("peer-coordinator", "wB")
	if err == nil {
		t.Fatal("implicit cross-workspace delivery must still be refused")
	}
	msg := err.Error()
	for _, want := range []string{"--workspace wK", "explicitly"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal must name the authorized path (%q); got: %s", want, msg)
		}
	}
}

// An explicit workspace authorizes the delivery, which is what the message now
// tells the caller to do.
func TestExplicitWorkspaceAuthorizesCrossWorkspaceDelivery(t *testing.T) {
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) > 1 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"type":"agents","agents":[
				{"name":"peer-coordinator","agent_status":"idle","pane_id":"wK:p2","tab_id":"wK:t2","workspace_id":"wK","cwd":"/tmp"}
			]}}`, nil
		}
		return `{"result":{"type":"ok"}}`, nil
	})
	defer restore()

	got, err := requireAgentWorkspaceIn("peer-coordinator", "wK")
	if err != nil {
		t.Fatalf("explicit workspace must authorize delivery: %v", err)
	}
	if got.Workspace != "wK" || got.PaneID != "wK:p2" {
		t.Fatalf("resolved the wrong agent: %+v", got)
	}
}
