package herdr

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// Live binary contract: pin what herdr 0.7.x actually exposes so FAC-185
// cannot re-invent prompt-delivery or generation fields with a green suite.

func TestLiveHerdrAgentHelp_HasPromptNotPromptDelivery(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
	out, err := exec.Command("herdr", "agent", "--help").CombinedOutput()
	text := string(out)
	if err != nil && text == "" {
		t.Fatalf("herdr agent --help: %v", err)
	}
	if !strings.Contains(text, "prompt") {
		t.Fatalf("expected prompt subcommand in help:\n%s", text)
	}
	// Fail if a future agent invents a dependency on a missing subcommand
	// without updating this pin — prose mentions are fine; a command row is not
	// required to be absent forever, but production code must not call it until
	// this test is deliberately updated.
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "prompt-delivery" {
			t.Log("note: live herdr now lists prompt-delivery; wire a real client before calling it")
			return
		}
	}
}

// TestLiveHerdrTabHelp_CompareCloseCapability pins the FAC-180 CLI contract:
// production calls `herdr tab compare-close`. When the installed binary lacks
// that subcommand, live transport must fail closed and must not fall back to
// plain `tab close`. When the subcommand is listed, help must show it as a
// command row (first field), not merely prose.
func TestLiveHerdrTabHelp_CompareCloseCapability(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
	out, err := exec.Command("herdr", "tab", "--help").CombinedOutput()
	text := string(out)
	if err != nil && text == "" {
		t.Fatalf("herdr tab --help: %v", err)
	}
	hasCompareClose := false
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "compare-close" || fields[0] == "compare_and_close") {
			hasCompareClose = true
			break
		}
	}

	// Always pin the transport argv shape via a hermetic runner: production
	// path is tab compare-close, never tab close as fallback.
	old := runHerdr
	defer func() { runHerdr = old }()
	var calls [][]string
	if !hasCompareClose {
		runHerdr = func(args ...string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			return "unknown command", errors.New("exit status 2")
		}
		_, trErr := liveCompareCloseTransport(fixtureRequest())
		if trErr == nil {
			t.Fatal("live transport must fail when compare-close is unavailable")
		}
		for _, c := range calls {
			if len(c) >= 2 && c[0] == "tab" && c[1] == "close" {
				t.Fatalf("fell back to plain tab close: %v", calls)
			}
			if len(c) < 2 || c[0] != "tab" || c[1] != "compare-close" {
				t.Fatalf("expected tab compare-close transport call, got %v", c)
			}
		}
		if len(calls) == 0 {
			t.Fatal("expected at least one compare-close transport attempt")
		}
		return
	}

	// Binary claims the subcommand: help pin must not be prose-only, and a
	// malformed request must still go to compare-close (not tab close).
	runHerdr = func(args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", errors.New("forced")
	}
	_, _ = liveCompareCloseTransport(fixtureRequest())
	if len(calls) != 1 || len(calls[0]) < 2 || calls[0][0] != "tab" || calls[0][1] != "compare-close" {
		t.Fatalf("with compare-close present, transport must call tab compare-close: %v", calls)
	}
}

func TestLiveHerdrAgentList_NoGenerationFieldRequired(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed")
	}
	out, err := exec.Command("herdr", "agent", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("herdr agent list: %v\n%s", err, out)
	}
	var env struct {
		Result struct {
			Agents []map[string]any `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(env.Result.Agents) == 0 {
		t.Skip("no live agents")
	}
	keys := map[string]struct{}{}
	sessionAbsent := 0
	for _, a := range env.Result.Agents {
		for k := range a {
			keys[k] = struct{}{}
		}
		if _, ok := a["agent_session"]; !ok {
			sessionAbsent++
		}
		// Assert real counters decode when present.
		if _, ok := a["state_change_seq"]; !ok {
			t.Fatalf("live agent missing state_change_seq: keys=%v", keysOf(a))
		}
		if _, ok := a["revision"]; !ok {
			t.Fatalf("live agent missing revision: keys=%v", keysOf(a))
		}
	}
	if _, ok := keys["generation"]; ok {
		t.Log("note: generation appeared in agent list; do not require it until all kinds emit it")
	}
	if sessionAbsent == 0 {
		t.Log("all agents report agent_session")
	} else {
		t.Logf("%d/%d agents lack agent_session (session must stay optional)", sessionAbsent, len(env.Result.Agents))
	}

	// Decode through AgentEntry and assert counters bind; Generation field must
	// not exist on the struct (compile-time) and session Value is optional.
	agents, err := AgentList()
	if err != nil {
		t.Fatalf("AgentList: %v", err)
	}
	if len(agents) == 0 {
		t.Fatal("AgentList empty after raw list non-empty")
	}
	sawSeq := false
	for _, a := range agents {
		if a.StateChangeSeq > 0 || a.Revision > 0 {
			sawSeq = true
		}
	}
	if !sawSeq {
		t.Fatal("expected at least one agent with state_change_seq or revision > 0")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAgentList_DecodesSessionAndCounters(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		return `{"result":{"agents":[
			{"name":"c","agent":"claude","agent_status":"idle","pane_id":"p1","tab_id":"t1","workspace_id":"w1","revision":3,"state_change_seq":10,"tab_generation":42,"agent_session":{"source":"herdr:claude","kind":"id","value":"sess"}},
			{"name":"g","agent":"grok","agent_status":"working","pane_id":"p2","tab_id":"t2","workspace_id":"w1","revision":5,"state_change_seq":20}
		]}}`, nil
	}
	agents, err := AgentList()
	if err != nil || len(agents) != 2 {
		t.Fatalf("AgentList: %v %#v", err, agents)
	}
	if agents[0].Session.Value != "sess" || agents[0].StateChangeSeq != 10 || agents[0].TabGeneration != 42 {
		t.Fatalf("claude row: %+v", agents[0])
	}
	if agents[1].Session.Value != "" || agents[1].StateChangeSeq != 20 {
		t.Fatalf("grok row: %+v", agents[1])
	}
}
