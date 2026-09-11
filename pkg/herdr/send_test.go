package herdr

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/mail"
)

func TestStatusFromList(t *testing.T) {
	agents := []AgentEntry{
		{Name: "forge-worker", PaneID: "w3:p3", Status: "working"},
		{Name: "", PaneID: "w3:p9", Status: "idle"},
	}
	if got := StatusFromList(agents, "w3:p3"); got != "working" {
		t.Errorf("by pane: got %q", got)
	}
	if got := StatusFromList(agents, "forge-worker"); got != "working" {
		t.Errorf("by name: got %q", got)
	}
	if got := StatusFromList(agents, "w3:p9"); got != "idle" {
		t.Errorf("unnamed pane: got %q", got)
	}
	if got := StatusFromList(agents, "ghost"); got != "" {
		t.Errorf("missing target must be empty, got %q", got)
	}
}

// FAC-579: the qualifier no longer names the MECHANISM. Consumption is proven
// by an echoed prompt where the harness echoes it, and by a status transition
// plus an advanced pane where it does not (Claude Code never echoes). Claiming
// "task text observed in pane" made the line a lie on the second path.
func TestFormatSendResultExplainsDeliveryGuarantee(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{name: "working", status: "working", want: "herd send: worker -> working (consumption confirmed)"},
		{name: "done", status: "done", want: "herd send: worker -> done (consumption confirmed)"},
		{name: "queued", status: "queued", want: "herd send: worker -> queued (queued but not consumed; explicit retry or defer required)"},
		{name: "queued-durable", status: StatusQueuedDurable, want: "herd send: worker -> queued-durable (durable inbox copy queued; not consumed)"},
		{name: "submitted", status: "submitted", want: "herd send: worker -> submitted (UNVERIFIED: --no-verify)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FormatSendResult("worker", tc.status); got != tc.want {
				t.Fatalf("FormatSendResult(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestFormatSendResultInWorkspaceAuditsAuthorizedRoute(t *testing.T) {
	if got, want := FormatSendResultInWorkspace("wB:p391", "wB", "working"), "herd send: wB:p391 [workspace=wB] -> working (consumption confirmed)"; got != want {
		t.Fatalf("FormatSendResultInWorkspace() = %q, want %q", got, want)
	}
}

func TestSendInWorkspaceAllowsExplicitCrossWorkspacePeer(t *testing.T) {
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"forge-orchestrator","pane_id":"wB:p391","workspace_id":"wB","agent_status":"idle"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if got, err := SendInWorkspace("wB:p391", "authorized coordinator packet", false, time.Second, "wB"); err != nil || got != "submitted" {
		t.Fatalf("SendInWorkspace() = %q, %v; want submitted", got, err)
	}
	if !prompted {
		t.Fatal("authorized peer must be prompted")
	}
}

func TestSendPressesEnterImmediatelyAfterPrompt(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	oldStatus := statusProbe
	t.Cleanup(func() {
		runHerdr = oldRun
		statusProbe = oldStatus
	})

	var calls []string
	statusCalls := 0
	paneReads := 0
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			statusCalls++
			status := "working"
			if statusCalls == 1 {
				status = "idle"
			}
			return fmt.Sprintf(`{"result":{"agents":[{"name":"worker","pane_id":"pane-1","workspace_id":"wK","agent_status":%q}]}}`, status), nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			paneReads++
			if paneReads == 1 {
				return `{"result":{"text":"empty pane"}}`, nil
			}
			return `{"result":{"text":"short kick"}}`, nil
		}
		return "{}", nil
	}

	if got, err := Send("worker", "short kick", true, 10*time.Second); err != nil || got != "working" {
		t.Fatalf("Send = %q, %v; want working", got, err)
	}
	var transportCalls []string
	for _, call := range calls {
		if strings.HasPrefix(call, "agent prompt") || strings.HasPrefix(call, "agent send-keys") {
			transportCalls = append(transportCalls, call)
		}
	}
	if len(transportCalls) < 2 || transportCalls[0] != "agent prompt worker short kick" || transportCalls[1] != "agent send-keys worker Enter" {
		t.Fatalf("herdr transport call order = %#v; all calls = %#v", transportCalls, calls)
	}
}

func TestSendAcceptsBusyLaneOnlyWithTaskTextPaneEvidence(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	box := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	t.Cleanup(SetQueueMailbox(box))
	var transport []string
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && (args[1] == "send-keys" || args[1] == "prompt") {
			transport = append(transport, strings.Join(args, " "))
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"pane-busy","workspace_id":"wK","agent_status":"working"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			return `{"result":{"text":"empty pane"}}`, nil
		}
		return "{}", nil
	}

	got, err := Send("worker", "assigned command: go test ./pkg/herdr", true, time.Second)
	if err != nil || got != StatusQueuedDurable {
		t.Fatalf("busy delivery = %q, %v; want queued-durable with no pane writes", got, err)
	}
	if len(transport) != 0 {
		t.Fatalf("busy routine transport = %#v; want zero keystrokes", transport)
	}
}

func TestSendRefusesAssignmentWhenStandingGoalCannotBePreempted(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	box := mail.NewMailbox(filepath.Join(t.TempDir(), "mail.jsonl"))
	t.Cleanup(SetQueueMailbox(box))
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"pane-busy","workspace_id":"wK","agent_status":"working"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			return `{"result":{"text":"empty pane"}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "send-keys" {
			return "standing goal could not be interrupted", fmt.Errorf("send-keys refused")
		}
		return "{}", nil
	}

	status, err := Send("worker", "assigned command: go test ./pkg/herdr", true, time.Second)
	if err != nil || status != StatusQueuedDurable {
		t.Fatalf("busy routine must queue even if send-keys would fail: status %q err %v", status, err)
	}
}

func TestSendRejectsStagedTextEvenWhenPaneStatusLooksHealthy(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	paneReads := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"pane-staged","workspace_id":"wK","agent_status":"idle"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			paneReads++
			if paneReads == 1 {
				return `{"result":{"text":"empty pane"}}`, nil
			}
			return `{"result":{"text":"❯ [Pasted text #1]"}}`, nil
		}
		return "{}", nil
	}

	_, err := Send("worker", "assigned command: go test ./pkg/herdr", true, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "queued-but-not-consumed") || !strings.Contains(err.Error(), "staged/unsubmitted") {
		t.Fatalf("staged delivery error = %v; want explicit staged/unsubmitted failure", err)
	}
}

func TestSendRejectsCrossWorkspaceDuplicateName(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[
				{"name":"worker","pane_id":"pane-a","workspace_id":"wK","agent_status":"idle"},
				{"name":"worker","pane_id":"pane-b","workspace_id":"wB","agent_status":"idle"}
			]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if _, err := Send("worker", "do not misroute", false, time.Second); err == nil {
		t.Fatal("cross-workspace duplicate must be rejected")
	} else if !strings.Contains(err.Error(), `workspace "wB"`) || !strings.Contains(err.Error(), `workspace "wK"`) {
		t.Fatalf("error = %v; want both workspace IDs", err)
	}
	if prompted {
		t.Fatal("cross-workspace target must be rejected before prompt")
	}
}

func TestSendRejectsAmbiguousBareLabelAcrossForgeDerivation(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[
				{"name":"scout-planner","pane_id":"pane-chainseer","workspace_id":"wC","agent_status":"idle"},
				{"name":"forge-scout-planner","pane_id":"pane-herdforge","workspace_id":"wK","agent_status":"idle"}
			]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if _, err := Send("scout-planner", "do not misdeliver", false, time.Second); err == nil {
		t.Fatal("bare label with exact and forge-derived live agents must be rejected")
	} else if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "forge-scout-planner") {
		t.Fatalf("error = %v; want an explicit ambiguous forge-derived candidate error", err)
	}
	if prompted {
		t.Fatal("ambiguous target must be rejected before prompt")
	}
}

func TestSendAllowsSameWorkspaceDelivery(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	prompted := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","pane_id":"pane-a","workspace_id":"wK","agent_status":"idle"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "agent" && args[1] == "prompt" {
			prompted = true
		}
		return "{}", nil
	}

	if got, err := Send("worker", "same workspace", false, time.Second); err != nil || got != "submitted" {
		t.Fatalf("Send = %q, %v; want submitted", got, err)
	}
	if !prompted {
		t.Fatal("same-workspace target must be prompted")
	}
}

// FAC-815: a warm claude-kind lane resting at "done" before this send is
// already paneAdvanced the instant the submitted text renders into the
// pane -- submission itself guarantees the delta. Status never leaves
// "done" during the poll (no genuine new turn observed), so this must
// refuse as queued-but-not-consumed, never confirm on the resting state.
func TestSendRefusesRestingDoneWithoutFreshWorkingObservation(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	paneReads := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","agent":"claude","pane_id":"pane-resting","workspace_id":"wK","agent_status":"done"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			paneReads++
			if paneReads == 1 {
				return `{"result":{"text":"idle prompt"}}`, nil
			}
			// Claude Code renders a compact transcript, never the literal
			// submitted text (FAC-579): the pane changes (paneAdvanced)
			// without the submitted string ever appearing (no echo proof).
			return `{"result":{"text":"idle prompt\n> Marinating..."}}`, nil
		}
		return "{}", nil
	}

	got, err := Send("worker", "assigned command: go test ./pkg/herdr", true, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "queued-but-not-consumed") {
		t.Fatalf("Send = %q, %v; want a queued-but-not-consumed refusal, resting done must never confirm", got, err)
	}
	if got != "queued" {
		t.Fatalf("Send status = %q; want queued", got)
	}
}

// FAC-815 correction (GLM independent review, mail seq 2974): the
// observationCount echo-proof branch (send.go:518) is unconditional --
// it is not scoped to harnessEchoesPrompt. A non-echo harness (claude)
// can render the literal submitted text transiently on submission itself
// (a compose-time render, a wrapped command line in the pane tail), which
// is the exact same resting-done false positive this task exists to
// kill, one branch over: status never leaves "done", yet the literal
// text appearing in the pane is (incorrectly, pre-correction) accepted as
// proof on its own. This must refuse identically to the no-literal-text
// case above.
func TestSendRefusesRestingDoneEvenWithLiteralTextRender(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	paneReads := 0
	const task = "assigned command: go test ./pkg/herdr"
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","agent":"claude","pane_id":"pane-literal","workspace_id":"wK","agent_status":"done"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			paneReads++
			if paneReads == 1 {
				return `{"result":{"text":"idle prompt"}}`, nil
			}
			// The literal submitted text DOES render into the pane -- a
			// compose-time echo of the user's own message -- while status
			// stays "done" the whole poll: no fresh working was ever
			// observed. This must NOT be accepted as proof for a
			// non-echoing harness.
			return fmt.Sprintf(`{"result":{"text":"idle prompt\n> %s"}}`, task), nil
		}
		return "{}", nil
	}

	got, err := Send("worker", task, true, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "queued-but-not-consumed") {
		t.Fatalf("Send = %q, %v; want a queued-but-not-consumed refusal even with the literal text rendered, resting done must never confirm", got, err)
	}
	if got != "queued" {
		t.Fatalf("Send status = %q; want queued", got)
	}
}

// A genuine new turn (status actually departs into "working" during this
// delivery's poll, then returns to "done") must still confirm -- the fix
// must not produce a false negative on real consumption.
func TestSendAcceptsGenuineFreshWorkingThenDone(t *testing.T) {
	t.Setenv("HERD_WORKSPACE", "wK")
	oldRun := runHerdr
	t.Cleanup(func() { runHerdr = oldRun })
	statusCalls := 0
	paneReads := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			statusCalls++
			status := "done"
			if statusCalls == 2 {
				status = "working"
			}
			return fmt.Sprintf(`{"result":{"agents":[{"name":"worker","agent":"claude","pane_id":"pane-fresh","workspace_id":"wK","agent_status":%q}]}}`, status), nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			paneReads++
			if paneReads == 1 {
				return `{"result":{"text":"idle prompt"}}`, nil
			}
			return `{"result":{"text":"idle prompt\n> Marinating..."}}`, nil
		}
		return "{}", nil
	}

	got, err := Send("worker", "assigned command: go test ./pkg/herdr", true, 5*time.Second)
	if err != nil || (got != "working" && got != "done") {
		t.Fatalf("Send = %q, %v; genuine working turn must confirm", got, err)
	}
}
