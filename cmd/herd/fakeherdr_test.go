package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// FAC-145 hermeticity: compiled CLI tests must NEVER reach the operator's
// live herdr fleet. Every test that can trigger a launch injects this
// protocol-faithful fake executable and sets the no-live guard, so a
// production fallback that tried the real socket fails the test instead of
// spawning an agent into the live workspace.
//
// The fake speaks the same JSON shapes the real CLI does and records every
// invocation, so tests can assert the exact tab/agent lifecycle AND that the
// live workspace census is untouched.

// Pane identity the fake mints. terminal_id is `required` on herdr's real
// PaneInfo schema and is the incarnation token production binds to.
const (
	fakeTabID      = "fakeT1"
	fakePaneID     = "fakeP1"
	fakeTerminalID = "fakeTERM1"
	// fakeReplacedTerminal is the incarnation a test installs to simulate a
	// DIFFERENT agent taking over the same tab/pane slot — the exact
	// situation a receipt bound to tab/pane alone would fail to notice.
	fakeReplacedTerminal = "fakeTERM2"
)

// The fake keeps pane state so `agent list` reports the pane `tab create`
// minted (with its live cwd), and reports nothing after `tab close` — the
// liveness and cwd-readback paths are only honestly exercised if the fake
// models the pane lifecycle.
const fakeHerdrScript = `#!/bin/sh
# Protocol-faithful herdr fake (FAC-145 hermetic tests).
LOG="$HERD_FAKE_LOG"
STATE="$HERD_FAKE_STATE"
printf '%s\n' "$*" >> "$LOG"
case "$1 $2" in
  "tab create")
    label=""; cwd=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --label) label="$2"; shift 2 ;;
        --cwd) cwd="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    printf '%s|%s|%s|%s\n' "fakeT1" "fakeP1" "fakeTERM1" "$cwd" > "$STATE"
    printf '{"result":{"tab":{"tab_id":"fakeT1","label":"%s"},"root_pane":{"pane_id":"fakeP1","tab_id":"fakeT1","terminal_id":"fakeTERM1"}}}\n' "$label"
    ;;
  "tab close") : > "$STATE"; printf '{"result":{"closed":true}}\n' ;;
  "tab compare-close") : > "$STATE"; printf '{"result":{"receipt":{"outcome":"closed","resulting_absence":true}}}\n' ;;
  "agent start") printf '{"result":{"started":true}}\n' ;;
  "agent prompt") printf '{"result":{"delivered":true}}\n' ;;
  "agent list")
    if [ -s "$STATE" ]; then
      IFS='|' read -r t p term cwd < "$STATE"
      printf '{"result":{"agents":[{"tab_id":"%s","pane_id":"%s","terminal_id":"%s","workspace_id":"wFAKE","cwd":"%s","foreground_cwd":"%s","agent_status":"idle","revision":1,"focused":false}],"type":"agents"}}\n' "$t" "$p" "$term" "$cwd" "$cwd"
    else
      printf '{"result":{"agents":[],"type":"agents"}}\n'
    fi
    ;;
  "pane list")
    if [ "${HERD_FAKE_PANE_MODE:-}" = "unknown" ]; then
      printf '{"result":{"panes":[]}}\n'
    elif [ -s "$STATE" ]; then
      IFS='|' read -r t p term cwd < "$STATE"
      title="${HERD_FAKE_PANE_TITLE:-}"
      printf '{"result":{"panes":[{"pane_id":"%s","tab_id":"%s","terminal_id":"%s","cwd":"%s","foreground_cwd":"%s","terminal_title":"%s"}]}}\n' "$p" "$t" "$term" "$cwd" "$cwd" "$title"
    else
      printf '{"result":{"panes":[]}}\n'
    fi
    ;;
  "pane read")
    printf '{"result":{"text":"%s"}}\n' "${HERD_FAKE_PANE_BODY:-ready}"
    ;;
  "pane process-info")
    if [ "${HERD_FAKE_PANE_MODE:-}" = "dead" ]; then
      printf '{"result":{"process_info":{"foreground_processes":[]}}}\n'
    else
      printf '{"result":{"process_info":{"foreground_processes":[{"pid":1,"name":"zsh"}]}}}\n'
    fi
    ;;
  "workspace list") printf '{"result":{"workspaces":[{"workspace_id":"wFAKE","name":"fake"}]}}\n' ;;
  *) printf '{"result":{}}\n' ;;
esac
exit 0
`

// installProtocolFakeHerdr writes the protocol-faithful fake and returns its
// path plus a log reader. Distinct from forge_driver_e2e_test.go's PATH stub.
func installProtocolFakeHerdr(t *testing.T) (bin string, calls func() []string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "herdr")
	if err := os.WriteFile(bin, []byte(fakeHerdrScript), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeStatePath(logPath), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(herdr.BinaryEnv, bin)
	t.Setenv("HERD_FAKE_LOG", logPath)
	t.Setenv("HERD_FAKE_STATE", fakeStatePath(logPath))
	t.Setenv(herdr.NoLiveEnv, "1")
	return bin, func() []string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if l != "" {
				out = append(out, l)
			}
		}
		return out
	}
}

// fakeStatePath derives the fake's pane-state file from its call log, so
// every process sharing the log shares the same simulated fleet.
func fakeStatePath(logPath string) string { return logPath + ".state" }

// setFakePane installs the one pane the fake's `agent list` reports. Tests
// use it to seed a live pane, or to swap in a DIFFERENT terminal_id on the
// same tab/pane slot (a replacement agent) while a broker is running.
func setFakePane(t *testing.T, logPath, terminalID, cwd string) {
	t.Helper()
	line := fmt.Sprintf("%s|%s|%s|%s\n", fakeTabID, fakePaneID, terminalID, cwd)
	if err := os.WriteFile(fakeStatePath(logPath), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hermeticHerdrEnv returns the env every child CLI process must carry so it
// can only ever reach the fake.
func hermeticHerdrEnv(bin, logPath string) []string {
	return []string{
		herdr.BinaryEnv + "=" + bin,
		herdr.NoLiveEnv + "=1",
		"HERD_FAKE_LOG=" + logPath,
		"HERD_FAKE_STATE=" + fakeStatePath(logPath),
	}
}

// liveWorkspaceCensus captures the operator's REAL fleet tab/agent listing
// so a test can prove it is byte-identical before and after. It shells the
// real herdr directly (never through the guarded package path) and returns
// "" when no live fleet exists on this host.
func liveWorkspaceCensus(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("herdr")
	if err != nil {
		return "" // no live fleet on this host
	}
	cmd := exec.Command(real, "agent", "list")
	cmd.Env = append(os.Environ(), "HERD_FAKE_LOG=/dev/null")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// Compare STABLE membership only: workspace/tab/pane/agent/name. Live
	// panes legitimately churn volatile fields (spinner glyphs, revision
	// counters, status) every second; a test must fail on tabs appearing or
	// disappearing, not on a title animating.
	var parsed struct {
		Result struct {
			Agents []struct {
				Workspace string `json:"workspace_id"`
				TabID     string `json:"tab_id"`
				PaneID    string `json:"pane_id"`
				Agent     string `json:"agent"`
				Name      string `json:"name"`
			} `json:"agents"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &parsed) != nil {
		return string(out)
	}
	ids := make([]string, 0, len(parsed.Result.Agents))
	for _, a := range parsed.Result.Agents {
		ids = append(ids, fmt.Sprintf("%s|%s|%s|%s|%s", a.Workspace, a.TabID, a.PaneID, a.Agent, a.Name))
	}
	sort.Strings(ids)
	return strings.Join(ids, "\n")
}

// assertLiveFleetUntouched fails when the live census changed across the
// body — the direct proof that a compiled test never spawned or closed
// anything in the operator's workspace.
func assertLiveFleetUntouched(t *testing.T, before string) {
	t.Helper()
	after := liveWorkspaceCensus(t)
	if before != after {
		t.Fatalf("LIVE FLEET MUTATED BY A TEST (FAC-145 hermeticity):\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestHermeticity_GuardRefusesLiveHerdr proves the guard itself works: with
// the no-live guard set and no override, any herdr call fails instead of
// reaching the operator's fleet.
func TestHermeticity_GuardRefusesLiveHerdr(t *testing.T) {
	t.Setenv(herdr.NoLiveEnv, "1")
	t.Setenv(herdr.BinaryEnv, "")
	if herdr.IsAvailable() {
		t.Fatal("herdr must report unavailable under the no-live guard")
	}
	if _, err := herdr.AgentList(); err == nil {
		t.Fatal("herdr calls must fail closed under the no-live guard")
	} else if !strings.Contains(err.Error(), "refusing to reach the LIVE herdr fleet") {
		t.Fatalf("expected the hermeticity refusal, got: %v", err)
	}
}

// TestHermeticity_FakeSpeaksProtocol proves the injected fake satisfies the
// real client parsing, so tests exercise production code paths honestly.
func TestHermeticity_FakeSpeaksProtocol(t *testing.T) {
	before := liveWorkspaceCensus(t)
	_, calls := installProtocolFakeHerdr(t)

	paneCwd := t.TempDir()
	tab, err := herdr.TabCreateForTask("wFAKE", "review-fac-1", paneCwd, true)
	if err != nil {
		t.Fatalf("fake tab create: %v", err)
	}
	if tab.ID != fakeTabID || tab.Pane.ID != fakePaneID || tab.Pane.TerminalID != fakeTerminalID {
		t.Fatalf("fake tab shape wrong: %+v", tab)
	}
	// A raw start carries no decision provenance and can never create a
	// process — it must fail closed even against the fake.
	if err := herdr.AgentStart("review-fac-1", "fake-agent", tab.Pane.ID); err == nil {
		t.Fatal("raw AgentStart must fail closed: no compiled LaunchDecision")
	}
	// The fake models the pane lifecycle, so the live readback paths run
	// for real: a created pane is listed with its cwd, a closed one is not.
	session := herdr.SessionID(tab.Pane)
	if got, err := herdr.PaneLiveCwd(session); err != nil || got != paneCwd {
		t.Fatalf("live cwd readback = %q, %v; want %q", got, err, paneCwd)
	}
	if alive, err := herdr.SessionExists(session); err != nil || !alive {
		t.Fatalf("created pane must be live: %v %v", alive, err)
	}
	// FAC-180: a bare close carries no generation/session compare, so it is
	// refused outright — closing goes through TabCloseCAS, whose
	// compare-close RPC this fake deliberately does not model.
	if err := herdr.TabClose(tab.ID); err == nil {
		t.Fatal("bare TabClose must be refused: compare-and-close is required")
	}

	got := calls()
	if len(got) < 2 {
		t.Fatalf("fake did not observe the lifecycle: %v", got)
	}
	if !strings.Contains(got[0], "tab create") || !strings.Contains(fmt.Sprint(got), "agent list") {
		t.Fatalf("unexpected lifecycle: %v", got)
	}
	assertLiveFleetUntouched(t, before)
}
