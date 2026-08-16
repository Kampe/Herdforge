package herdr

import (
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
