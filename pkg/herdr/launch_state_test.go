package herdr

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestVerifyAgentLaunchRequiresAgentAndForegroundProcess(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	listCalls := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			listCalls++
			return `{"result":{"agents":[{"name":"worker","agent":"grok","agent_status":"working","pane_id":"p1","tab_id":"t1","terminal_id":"term-1"}]}}`, nil
		}
		if len(args) == 4 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"foreground_processes":[{"pid":42,"name":"grok"}]}}}`, nil
		}
		if len(args) == 2 && args[0] == "pane" && args[1] == "list" {
			return `{"result":{"panes":[{"pane_id":"p1","terminal_id":"term-1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			return `{"result":{"text":"ready"}}`, nil
		}
		t.Fatalf("unexpected herdr args: %v", args)
		return "", nil
	}
	obs, err := verifyAgentLaunch("worker", "p1", time.Second)
	if err != nil {
		t.Fatalf("verifyAgentLaunch: %v", err)
	}
	if obs.State != LaunchReady || obs.TabID != "t1" || obs.TerminalID != "term-1" {
		t.Fatalf("observation = %+v", obs)
	}
	if listCalls != 1 {
		t.Fatalf("agent list calls = %d, want 1", listCalls)
	}
}

func TestVerifyAgentLaunchRejectsShellOnlyPane(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","agent":"grok","agent_status":"idle","pane_id":"p1","tab_id":"t1"}]}}`, nil
		}
		if len(args) == 2 && args[0] == "pane" && args[1] == "list" {
			return `{"result":{"panes":[{"pane_id":"p1","terminal_id":"term-1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			return `{"result":{"text":"waiting"}}`, nil
		}
		return `{"result":{"process_info":{"foreground_processes":[{"pid":42,"name":"zsh"}]}}}`, nil
	}
	obs, err := verifyAgentLaunch("worker", "p1", 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not reach READY") {
		t.Fatalf("error = %v, want bounded readiness failure", err)
	}
	if obs.State != LaunchDied {
		t.Fatalf("state = %q, want DIED", obs.State)
	}
}

func TestHasForegroundAgentProcessIgnoresShells(t *testing.T) {
	if hasForegroundAgentProcess([]PaneProcess{{Name: "zsh"}, {Name: "-bash"}}) {
		t.Fatal("shell-only pane reported an agent process")
	}
	if !hasForegroundAgentProcess([]PaneProcess{{Name: "zsh"}, {Name: "node"}}) {
		t.Fatal("non-shell process was not detected")
	}
}

func TestLoginOrAuthScreenTreatsTrustDialogsAsNotReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{name: "folder", text: "Trust this folder to continue"},
		{name: "workspace", text: "Trust this workspace?"},
		{name: "consent", text: "Consent required before access"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !LoginOrAuthScreen("", tc.text) {
				t.Fatalf("dialog %q was accepted as a usable session", tc.text)
			}
		})
	}
}

func TestVerifyAgentLaunchRejectsPaneIncarnationDrift(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","agent":"grok","agent_status":"working","pane_id":"p1","tab_id":"t1","terminal_id":"new"}]}}`, nil
		}
		if len(args) == 2 && args[0] == "pane" && args[1] == "list" {
			return `{"result":{"panes":[{"pane_id":"p1","terminal_id":"old"}]}}`, nil
		}
		t.Fatalf("unexpected herdr args after incarnation drift: %v", args)
		return "", nil
	}
	obs, err := verifyAgentLaunch("worker", "p1", 5*time.Millisecond)
	if err == nil || obs.State != LaunchDied || !strings.Contains(obs.Reason, "incarnation changed") {
		t.Fatalf("observation=%+v err=%v, want bounded drift failure", obs, err)
	}
}

func TestVerifyAgentLaunchRejectsTitleOnlyAuth(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) == 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[{"name":"worker","agent":"grok","agent_status":"working","pane_id":"p1","tab_id":"t1","terminal_id":"term-1","terminal_title":"Sign in to continue"}]}}`, nil
		}
		if len(args) == 2 && args[0] == "pane" && args[1] == "list" {
			return `{"result":{"panes":[{"pane_id":"p1","terminal_id":"term-1"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			return `{"result":{"text":"ready"}}`, nil
		}
		t.Fatalf("unexpected herdr args after auth title: %v", args)
		return "", nil
	}
	obs, err := verifyAgentLaunch("worker", "p1", time.Second)
	if err == nil || obs.State != LaunchRefused || !strings.Contains(obs.Reason, "authentication") {
		t.Fatalf("observation=%+v err=%v, want auth refusal", obs, err)
	}
}

func TestWaitExactPaneReady_UnknownPaneIsLaunchFailed(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	started := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "start" {
			started = true
			t.Fatal("unknown pane must not start a harness")
		}
		if len(args) == 2 && args[0] == "pane" && args[1] == "list" {
			// Exact created pane is absent from inventory: the FAC-369 race.
			return `{"result":{"panes":[{"pane_id":"other","tab_id":"t-other","terminal_id":"term-x"}]}}`, nil
		}
		t.Fatalf("unexpected herdr args during unknown-pane wait: %v", args)
		return "", nil
	}
	obs, err := WaitExactPaneReady("t1", "p1", "term-1", 8*time.Millisecond)
	if !IsLaunchFailed(err) {
		t.Fatalf("err=%v, want LAUNCH_FAILED", err)
	}
	if obs.State != LaunchFailed || !strings.Contains(obs.Reason, "unknown pane") {
		t.Fatalf("observation=%+v, want LAUNCH_FAILED unknown pane", obs)
	}
	if started {
		t.Fatal("harness start must not run when the exact pane is unknown")
	}
}

func TestWaitExactPaneReady_AuthScreenIsLaunchFailed(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	started := false
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "start" {
			started = true
			t.Fatal("auth screen must not start a harness")
		}
		if len(args) == 2 && args[0] == "pane" && args[1] == "list" {
			return `{"result":{"panes":[{"pane_id":"p1","tab_id":"t1","terminal_id":"term-1","cwd":"/wt","foreground_cwd":"/wt","terminal_title":"Sign in to continue"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			return `{"result":{"text":"please log in to continue"}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			t.Fatal("auth refusal must not wait on process inventory")
		}
		t.Fatalf("unexpected herdr args during auth-screen wait: %v", args)
		return "", nil
	}
	obs, err := WaitExactPaneReady("t1", "p1", "term-1", time.Second)
	if !IsLaunchFailed(err) {
		t.Fatalf("err=%v, want LAUNCH_FAILED", err)
	}
	if obs.State != LaunchFailed || !strings.Contains(obs.Reason, "authentication") {
		t.Fatalf("observation=%+v, want LAUNCH_FAILED authentication screen", obs)
	}
	if started {
		t.Fatal("harness start must not run against a login screen")
	}
}

func TestWaitExactPaneReady_ReadyWhenShellAndCwdAppear(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	lists := 0
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "start" {
			t.Fatal("wait must not start a harness")
		}
		if len(args) == 2 && args[0] == "pane" && args[1] == "list" {
			lists++
			if lists < 2 {
				return `{"result":{"panes":[]}}`, nil
			}
			return `{"result":{"panes":[{"pane_id":"p1","tab_id":"t1","terminal_id":"term-1","cwd":"/wt","foreground_cwd":"/wt"}]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "read" {
			return `{"result":{"text":"$ "}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"result":{"process_info":{"foreground_processes":[{"pid":7,"name":"zsh"}]}}}`, nil
		}
		t.Fatalf("unexpected herdr args during ready wait: %v", args)
		return "", nil
	}
	obs, err := WaitExactPaneReady("t1", "p1", "term-1", time.Second)
	if err != nil {
		t.Fatalf("WaitExactPaneReady: %v", err)
	}
	if obs.State != LaunchReady || obs.Cwd != "/wt" {
		t.Fatalf("observation=%+v, want READY with readable cwd", obs)
	}
}

func TestCompensateExactTab_UsesGenerationSafeClose(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	var closed []string
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "tab" && args[1] == "compare-close" {
			closed = append(closed, args[2])
			return `{"result":{"receipt":{"outcome":"closed","resulting_absence":true}}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			t.Fatalf("CompensateExactTab must not fall back to unfenced tab close: %v", args)
		}
		t.Fatalf("unexpected herdr args during compensate: %v", args)
		return "", nil
	}
	err := CompensateExactTab(CloseRequest{
		WorkspaceID: "wK",
		TabID:       "wK:tA8",
		Generation:  "7",
		TabRevision: 1,
		PaneIDs:     []string{"wK:p1"},
		Nonce:       "launch-fail-wK:tA8",
	})
	if err != nil {
		t.Fatalf("CompensateExactTab: %v", err)
	}
	if len(closed) != 1 || !strings.Contains(closed[0], "wK:tA8") {
		t.Fatalf("compare-close payloads = %v, want exact tab", closed)
	}
}

func TestCompensateExactTab_AlreadyAbsentIsSuccess(t *testing.T) {
	old := runHerdr
	defer func() { runHerdr = old }()
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "tab" && args[1] == "close" {
			return "", fmt.Errorf("tab not found")
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		t.Fatalf("unexpected herdr args during absent compensate: %v", args)
		return "", nil
	}
	if err := CompensateExactTab(CloseRequest{WorkspaceID: "wK", TabID: "wK:tGone", Nonce: "n"}); err != nil {
		t.Fatalf("already-absent tab must count as compensated: %v", err)
	}
}
