package herdr

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDeliverAndProveRetriesSubmitUntilConsumed is the FAC-533 regression.
//
// A codex TUI pane was observed holding a full assignment in its composer,
// unsubmitted, while dispatch reported the launch failed. The cause was that
// the re-nudge guard was initialized to true, making the retry unreachable:
// exactly one Enter was ever sent, immediately after AgentPrompt, racing the
// composer. This stubs an agent that stays idle until a second Enter arrives.
func TestDeliverAndProveRetriesSubmitUntilConsumed(t *testing.T) {
	var mu sync.Mutex
	enters := 0
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch {
		case args[0] == "agent" && len(args) > 1 && args[1] == "send-keys":
			mu.Lock()
			enters++
			mu.Unlock()
			return `{"result":{"type":"ok"}}`, nil
		case args[0] == "agent" && len(args) > 1 && args[1] == "prompt":
			return `{"result":{"type":"ok"}}`, nil
		case args[0] == "agent" && len(args) > 1 && args[1] == "list":
			// The pane only starts working once a SECOND Enter has landed.
			mu.Lock()
			status := "idle"
			if enters >= 2 {
				status = "working"
			}
			mu.Unlock()
			return `{"result":{"type":"agents","agents":[{"name":"lane","agent_status":"` +
				status + `","pane_id":"p1","workspace_id":"w1","cwd":"/tmp"}]}}`, nil
		}
		return `{"result":{"type":"ok"}}`, nil
	})
	defer restore()

	rec, err := DeliverAndProve("lane", "ASSIGNMENT", 20*time.Second)
	if err != nil {
		t.Fatalf("delivery must recover once the composer is submitted: %v", err)
	}
	if rec == nil || !rec.Consumed || !rec.Verified {
		t.Fatalf("want a consumed+verified receipt, got %+v", rec)
	}
	mu.Lock()
	got := enters
	mu.Unlock()
	if got < 2 {
		t.Fatalf("want at least one re-nudge after the initial Enter, got %d", got)
	}
}

// TestDeliverAndProveReportsFailedSubmitKey proves an unsubmitted assignment
// names its cause. The old code discarded the Enter error entirely, so a pane
// holding the text looked identical to an agent that ignored the prompt.
func TestDeliverAndProveReportsFailedSubmitKey(t *testing.T) {
	restore := SetRunHerdrForTest(func(args ...string) (string, error) {
		if len(args) > 1 && args[0] == "agent" && args[1] == "send-keys" {
			return "", errSubmitRefused
		}
		if len(args) > 1 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"type":"agents","agents":[{"name":"lane","agent_status":"idle","pane_id":"p1","workspace_id":"w1","cwd":"/tmp"}]}}`, nil
		}
		return `{"result":{"type":"ok"}}`, nil
	})
	defer restore()

	_, err := DeliverAndProve("lane", "ASSIGNMENT", 3*time.Second)
	if err == nil {
		t.Fatal("an unsubmitted assignment must fail closed")
	}
	if !strings.Contains(err.Error(), "submit key failed") {
		t.Fatalf("failure must name the failed submit key, got: %v", err)
	}
}

var errSubmitRefused = &submitError{}

type submitError struct{}

func (*submitError) Error() string { return "send-keys refused" }
