package herdr

import (
	"fmt"
	"strings"
	"testing"
)

func TestPromptBinding_Validate(t *testing.T) {
	if err := (PromptBinding{Name: "a", AgentSessionID: "pending-x"}).Validate(); err == nil {
		t.Fatal("pending must fail")
	}
	if err := (PromptBinding{Name: "a", AgentSessionID: "herdr-pane:p1"}).Validate(); err == nil {
		t.Fatal("pane must fail")
	}
	if err := (PromptBinding{Name: "a", AgentSessionID: "ses_live_ok_abc"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPromptExact_SessionDriftRejected(t *testing.T) {
	// Fake herdr: list returns session A, then after prompt returns session B.
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	phase := 0
	runHerdr = func(args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "agent list"):
			sid := "ses_AAA"
			if phase >= 2 {
				sid = "ses_BBB" // drift
			}
			return fmt.Sprintf(`{"result":{"agents":[{"name":"task-x","agent":"grok","agent_status":"idle","tab_id":"wF:t1","pane_id":"wF:p1","agent_session":{"value":%q}}],"type":"agent_list"}}`, sid), nil
		case strings.HasPrefix(cmd, "agent prompt"):
			phase = 2
			return "ok", nil
		default:
			return "", fmt.Errorf("unexpected %s", cmd)
		}
	}
	b := PromptBinding{Name: "task-x", TabID: "wF:t1", PaneID: "wF:p1", AgentSessionID: "ses_AAA", Kind: "grok"}
	_, err := AgentPromptExact(b, "hello", false)
	if err == nil {
		t.Fatal("session drift must fail post-prompt")
	}
	if !strings.Contains(err.Error(), "session drift") && !strings.Contains(err.Error(), "post-prompt") {
		t.Fatalf("want drift error, got %v", err)
	}
}

func TestAgentPromptExact_HappyPath(t *testing.T) {
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	runHerdr = func(args ...string) (string, error) {
		cmd := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(cmd, "agent list"):
			return `{"result":{"agents":[{"name":"task-y","agent":"claude","agent_status":"idle","tab_id":"wF:t2","pane_id":"wF:p2","agent_session":{"value":"ses_stable_1"}}],"type":"agent_list"}}`, nil
		case strings.HasPrefix(cmd, "agent prompt"):
			if !strings.Contains(cmd, "task-y") {
				return "", fmt.Errorf("prompt target must be name task-y: %s", cmd)
			}
			return "acked", nil
		default:
			return "", fmt.Errorf("unexpected %s", cmd)
		}
	}
	b := PromptBinding{Name: "task-y", TabID: "wF:t2", AgentSessionID: "ses_stable_1", Kind: "claude"}
	out, err := AgentPromptExact(b, "do work", false)
	if err != nil {
		t.Fatal(err)
	}
	if out != "acked" {
		t.Fatalf("out=%q", out)
	}
}

func TestAgentPromptExact_LoginRejected(t *testing.T) {
	prev := runHerdr
	t.Cleanup(func() { runHerdr = prev })
	runHerdr = func(args ...string) (string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "agent list") {
			return `{"result":{"agents":[{"name":"lp-codex","agent":"codex","agent_status":"idle","tab_id":"wF:t51","terminal_title":"browser-login","agent_session":{"value":"ses_x"}}],"type":"agent_list"}}`, nil
		}
		return "", fmt.Errorf("should not prompt")
	}
	b := PromptBinding{Name: "lp-codex", TabID: "wF:t51", AgentSessionID: "ses_x"}
	if _, err := AgentPromptExact(b, "x", false); err == nil {
		t.Fatal("login screen must reject prompt")
	}
}
